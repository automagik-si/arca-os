package diskimport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lxc/incus-os/incus-osd/api"
)

// parseZpoolImport returns the pool name and id from "zpool import -d" output.
func parseZpoolImport(out string) (string, string) {
	var name, id string

	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)

		value, ok := strings.CutPrefix(line, "pool: ")
		if ok && name == "" {
			name = strings.TrimSpace(value)
		}

		value, ok = strings.CutPrefix(line, "id: ")
		if ok && id == "" {
			id = strings.TrimSpace(value)
		}
	}

	return name, id
}

func (m *Manager) openZFS(ctx context.Context, d *openDisk, dev string, passphrase string) error {
	out, err := m.run.Run(ctx, "", "zpool", "import", "-d", dev)
	if err != nil {
		return err
	}

	name, id := parseZpoolImport(out)
	if name == "" || id == "" {
		d.unreadable = append(d.unreadable, filepath.Base(dev)+": zfs member with no importable pool")

		return nil
	}

	// A pool spread over several partitions of the drive is imported once.
	for _, fs := range d.fss {
		if fs.zpool == name {
			return nil
		}
	}

	out, err = m.run.Run(ctx, "", "zpool", "list", "-H", "-o", "name")
	if err != nil {
		return err
	}

	if slices.Contains(strings.Fields(out), name) {
		return fmt.Errorf("zfs pool %q exists on this system too", name)
	}

	altroot := m.mountPath(d, "zfs-"+name)

	err = os.MkdirAll(altroot, 0o700)
	if err != nil {
		return err
	}

	_, err = m.run.Run(ctx, "", "zpool", "import", "-d", dev, "-o", "readonly=on", "-N", "-f", "-R", altroot, id)
	if err != nil {
		_ = os.Remove(altroot)

		return err
	}

	d.undo = append(d.undo, undoStep{"rmdir", []string{altroot}}, undoStep{"zpool", []string{"export", name}})
	m.addLayer(d, "zfs")

	out, err = m.run.Run(ctx, "", "zfs", "list", "-H", "-o", "name,type,encryptionroot,keystatus", "-r", name)
	if err != nil {
		return err
	}

	datasets := []string{}
	roots := []string{}

	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			continue
		}

		datasets = append(datasets, fields[0])

		if fields[3] == "unavailable" && fields[2] != "-" && !slices.Contains(roots, fields[2]) {
			roots = append(roots, fields[2])
		}
	}

	for _, root := range roots {
		if passphrase == "" {
			return ErrPassphraseNeeded
		}

		_, err = m.run.Run(ctx, passphrase+"\n", "zfs", "load-key", "-L", "prompt", root)
		if err != nil {
			return fmt.Errorf("the passphrase did not open zfs dataset %s", root)
		}
	}

	fs := &filesystem{name: "zfs-" + name, fsType: "zfs", zpool: name, mount: altroot}
	fs.info = scanDatasets(datasets)
	fs.info.Name = fs.name
	fs.info.Type = "zfs"
	d.fss = append(d.fss, fs)

	return nil
}

// scanDatasets finds the instances and custom volumes of an Incus zfs pool.
func scanDatasets(datasets []string) api.SystemStorageImportFilesystem {
	ret := emptyListing()
	seen := map[string]bool{}

	for _, ds := range datasets {
		parts := strings.Split(ds, "/")
		if len(parts) < 3 {
			if len(parts) == 2 {
				ret.Folders = append(ret.Folders, ds)
			}

			continue
		}

		kind := parts[len(parts)-2]
		leaf := parts[len(parts)-1]

		switch kind {
		case "containers", "virtual-machines":
			leaf = strings.TrimSuffix(leaf, ".block")

			key := kind + "/" + leaf
			if seen[key] {
				continue
			}

			seen[key] = true

			project, name := splitProject(leaf)
			typ := "container"

			if kind == "virtual-machines" {
				typ = "virtual-machine"
			}

			ret.Instances = append(ret.Instances, api.SystemStorageImportInstance{
				Name:    name,
				Project: project,
				Type:    typ,
				Source:  strings.Join(parts[:len(parts)-1], "/") + "/" + leaf,
			})
		case "custom":
			project, name := splitProject(leaf)
			ret.Volumes = append(ret.Volumes, api.SystemStorageImportVolume{Name: name, Project: project, Source: ds})
		default:
		}
	}

	return ret
}

// mountDataset mounts a dataset of an imported pool read-only, once, until the drive closes.
func (m *Manager) mountDataset(ctx context.Context, d *openDisk, dataset string) (string, error) {
	target := m.mountPath(d, "ds-"+dataset)

	for _, step := range d.undo {
		if step.name == "umount" && step.args[0] == target {
			return target, nil
		}
	}

	err := os.MkdirAll(target, 0o700)
	if err != nil {
		return "", err
	}

	_, err = m.run.Run(ctx, "", "mount", "-t", "zfs", "-o", "ro,zfsutil", dataset, target)
	if err != nil {
		return "", err
	}

	d.undo = append(d.undo, undoStep{"rmdir", []string{target}}, undoStep{"umount", []string{target}})

	return target, nil
}
