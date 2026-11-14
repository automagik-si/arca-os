package diskimport

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/lxc/incus-os/incus-osd/api"
)

const (
	scanMaxDepth = 6
	scanMaxDirs  = 50000
)

var scanSkip = []string{"rootfs", "proc", "sys", "dev", "run", "tmp", "snapshots", ".snapshots"}

func emptyListing() api.SystemStorageImportFilesystem {
	return api.SystemStorageImportFilesystem{
		Instances: []api.SystemStorageImportInstance{},
		Volumes:   []api.SystemStorageImportVolume{},
		Folders:   []string{},
	}
}

// splitProject splits an Incus storage name into project and name.
func splitProject(storageName string) (string, string) {
	project, name, ok := strings.Cut(storageName, "_")
	if !ok {
		return "default", storageName
	}

	return project, name
}

func isDir(path string) bool {
	fi, err := os.Lstat(path)

	return err == nil && fi.IsDir()
}

// readBackupYAML reads an instance's backup.yaml as a generic document.
func readBackupYAML(path string) (map[string]any, error) {
	content, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return nil, err
	}

	doc := map[string]any{}

	err = yaml.Unmarshal(content, &doc)
	if err != nil {
		return nil, err
	}

	return doc, nil
}

func mapAt(doc map[string]any, key string) map[string]any {
	v, ok := doc[key].(map[string]any)
	if !ok {
		return nil
	}

	return v
}

func stringAt(doc map[string]any, key string) string {
	v, ok := doc[key].(string)
	if !ok {
		return ""
	}

	return v
}

// instanceFromDir describes the instance whose backup.yaml is in dir.
func instanceFromDir(root string, dir string) (api.SystemStorageImportInstance, bool) {
	doc, err := readBackupYAML(filepath.Join(dir, "backup.yaml"))
	if err != nil {
		return api.SystemStorageImportInstance{}, false
	}

	inst := mapAt(doc, "container")
	if inst == nil {
		return api.SystemStorageImportInstance{}, false
	}

	rel, _ := filepath.Rel(root, dir)
	ret := api.SystemStorageImportInstance{
		Name:    stringAt(inst, "name"),
		Project: stringAt(inst, "project"),
		Type:    stringAt(inst, "type"),
		Source:  rel,
	}

	if ret.Project == "" {
		ret.Project = "default"
	}

	if ret.Type == "" {
		ret.Type = "container"
	}

	config := mapAt(inst, "config")
	if config != nil {
		v := stringAt(config, "boot.autostart")
		ret.Autostart = v == "true" || v == "1" || v == "on" || v == "yes"
	}

	snaps, ok := doc["snapshots"].([]any)
	if !ok {
		snaps = nil
	}

	for _, s := range snaps {
		snap, ok := s.(map[string]any)
		if ok && stringAt(snap, "name") != "" {
			ret.Snapshots = append(ret.Snapshots, stringAt(snap, "name"))
		}
	}

	return ret, ret.Name != ""
}

// scanPool lists the instances and custom volumes of an Incus pool laid out in dir.
func scanPool(root string, dir string, ret *api.SystemStorageImportFilesystem) bool {
	found := false

	for _, kind := range []string{"containers", "virtual-machines"} {
		entries, err := os.ReadDir(filepath.Join(dir, kind))
		if err != nil {
			continue
		}

		for _, e := range entries {
			inst, ok := instanceFromDir(root, filepath.Join(dir, kind, e.Name()))
			if ok {
				ret.Instances = append(ret.Instances, inst)
				found = true
			}
		}
	}

	if !found {
		return false
	}

	entries, err := os.ReadDir(filepath.Join(dir, "custom"))
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}

			project, name := splitProject(e.Name())
			rel, _ := filepath.Rel(root, filepath.Join(dir, "custom", e.Name()))
			ret.Volumes = append(ret.Volumes, api.SystemStorageImportVolume{Name: name, Project: project, Source: rel})
		}
	}

	return true
}

// scanTree lists the folders at the top of root and every Incus pool found below it.
func scanTree(root string) api.SystemStorageImportFilesystem {
	ret := emptyListing()

	entries, err := os.ReadDir(root)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				ret.Folders = append(ret.Folders, e.Name())
			}
		}
	}

	type item struct {
		path  string
		depth int
	}

	queue := []item{{root, 0}}
	visited := 0

	for len(queue) > 0 && visited < scanMaxDirs {
		cur := queue[0]
		queue = queue[1:]
		visited++

		if scanPool(root, cur.path, &ret) || cur.depth >= scanMaxDepth {
			continue
		}

		children, err := os.ReadDir(cur.path)
		if err != nil {
			continue
		}

		for _, c := range children {
			if !c.IsDir() || slices.Contains(scanSkip, c.Name()) {
				continue
			}

			queue = append(queue, item{filepath.Join(cur.path, c.Name()), cur.depth + 1})
		}
	}

	return ret
}

// resolveSource returns the path of a listed source inside root, refusing anything outside it.
func resolveSource(root string, source string) (string, bool) {
	clean := filepath.Clean("/" + source)
	path := filepath.Join(root, clean)

	if !isDir(path) {
		return "", false
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}

	if resolved != realRoot && !strings.HasPrefix(resolved, realRoot+"/") {
		return "", false
	}

	return resolved, true
}
