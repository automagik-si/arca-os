package diskimport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.yaml.in/yaml/v4"

	"github.com/lxc/incus-os/incus-osd/api"
)

const spoolMargin = 1 << 30

type copyJob struct {
	mu       sync.Mutex
	id       int
	req      api.SystemStorageImportCopyRequest
	state    string
	done     atomic.Int64
	total    int64
	err      string
	started  time.Time
	finished *time.Time
}

func (j *copyJob) snapshot() api.SystemStorageImportCopy {
	j.mu.Lock()
	defer j.mu.Unlock()

	done := j.done.Load()
	if j.state != "done" && done > j.total {
		done = j.total
	}

	return api.SystemStorageImportCopy{
		ID:         j.id,
		Request:    j.req,
		State:      j.state,
		BytesDone:  done,
		BytesTotal: j.total,
		Error:      j.err,
		Started:    j.started,
		Finished:   j.finished,
	}
}

func (j *copyJob) finish(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	now := time.Now()
	j.finished = &now
	j.state = "done"

	if err != nil {
		j.state = "failed"
		j.err = err.Error()
	}
}

type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))

	return n, err
}

// plan is what one copy writes into the tarball.
type plan struct {
	kind    string
	name    string
	project string
	index   map[string]any
	trees   []tree
	image   string
	total   int64
}

// Copy starts a background copy of one listed item into a native pool and returns its job id.
func (m *Manager) Copy(ctx context.Context, req api.SystemStorageImportCopyRequest) (int, error) {
	d, fs, err := m.reserve(req)
	if err != nil {
		return 0, err
	}

	release := func() {
		m.mu.Lock()
		d.busy--
		m.mu.Unlock()
	}

	p, err := m.makePlan(ctx, d, fs, req)
	if err != nil {
		release()

		return 0, err
	}

	free, err := m.sys.FreeBytes()
	if err == nil && free < p.total+spoolMargin {
		err = fmt.Errorf("not enough room: the copy needs %d bytes free to spool, %d are", p.total+spoolMargin, free)
	}

	if err != nil {
		release()

		return 0, err
	}

	m.mu.Lock()
	m.nextID++
	job := &copyJob{id: m.nextID, req: req, state: "running", total: p.total, started: time.Now()}
	m.copies = append(m.copies, job)
	m.mu.Unlock()

	go func() {
		err := m.runCopy(context.WithoutCancel(ctx), job, p)
		job.finish(err)
		release()

		if err != nil {
			slog.Error("Import copy failed", "id", job.id, "source", req.Source, "err", err)
		} else {
			slog.Info("Import copy done", "id", job.id, "source", req.Source)
		}
	}()

	return job.id, nil
}

// reserve finds the drive and file system of a copy and keeps the drive open until the copy ends.
func (m *Manager) reserve(req api.SystemStorageImportCopyRequest) (*openDisk, *filesystem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	d, ok := m.disks[req.Serial]
	if !ok {
		return nil, nil, fmt.Errorf("drive %q is not open", req.Serial)
	}

	if req.TargetPool == "" {
		return nil, nil, errors.New("no target pool given")
	}

	for _, fs := range d.fss {
		if fs.name == req.Filesystem {
			d.busy++

			return d, fs, nil
		}
	}

	return nil, nil, fmt.Errorf("no file system %q on drive %q", req.Filesystem, req.Serial)
}

func (m *Manager) runCopy(ctx context.Context, job *copyJob, p *plan) error {
	pr, pw := io.Pipe()
	writeErr := make(chan error, 1)

	go func() {
		err := writePlan(countingWriter{pw, &job.done}, p)
		writeErr <- err

		_ = pw.CloseWithError(err)
	}()

	var err error
	if p.kind == "instance" {
		err = m.incus.ImportInstance(ctx, p.project, job.req.TargetPool, p.name, pr)
	} else {
		err = m.incus.ImportVolume(ctx, p.project, job.req.TargetPool, p.name, pr)
	}

	_ = pr.CloseWithError(errors.New("import ended"))

	// A read error on the old drive explains a failed import better than Incus' own message.
	wErr := <-writeErr
	if wErr != nil && err != nil {
		return wErr
	}

	return err
}

func writePlan(w io.Writer, p *plan) error {
	index, err := yaml.Marshal(p.index)
	if err != nil {
		return err
	}

	t := newTarball(w)

	err = t.addBytes("backup/index.yaml", index)
	if err != nil {
		return err
	}

	for _, tr := range p.trees {
		err = t.addTree(tr)
		if err != nil {
			return err
		}
	}

	if p.image != "" {
		err = t.addFile("backup/virtual-machine.img", p.image)
		if err != nil {
			return err
		}
	}

	return t.close()
}

func (m *Manager) makePlan(ctx context.Context, d *openDisk, fs *filesystem, req api.SystemStorageImportCopyRequest) (*plan, error) {
	switch req.Kind {
	case "instance":
		return m.instancePlan(ctx, d, fs, req)
	case "volume", "folder":
		return m.volumePlan(ctx, d, fs, req)
	}

	return nil, fmt.Errorf("unknown kind %q", req.Kind)
}

// sourceDir finds the directory of a listed source, mounting a zfs dataset when needed.
func (m *Manager) sourceDir(ctx context.Context, d *openDisk, fs *filesystem, source string) (string, error) {
	if fs.zpool == "" {
		dir, ok := resolveSource(fs.mount, source)
		if !ok {
			return "", fmt.Errorf("%q is not a folder on %s", source, fs.name)
		}

		return dir, nil
	}

	if source != fs.zpool && !strings.HasPrefix(source, fs.zpool+"/") {
		return "", fmt.Errorf("%q is not a dataset of pool %s", source, fs.zpool)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.mountDataset(ctx, d, source)
}

func (m *Manager) instancePlan(ctx context.Context, d *openDisk, fs *filesystem, req api.SystemStorageImportCopyRequest) (*plan, error) {
	dir, err := m.sourceDir(ctx, d, fs, req.Source)
	if err != nil {
		return nil, err
	}

	doc, err := readBackupYAML(filepath.Join(dir, "backup.yaml"))
	if err != nil {
		return nil, fmt.Errorf("%q has no readable backup.yaml: %w", req.Source, err)
	}

	set, err := prepareInstanceConfig(doc, req)
	if err != nil {
		return nil, err
	}

	backupYAML, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}

	inst := mapAt(doc, "container")
	isVM := stringAt(inst, "type") == "virtual-machine"

	p := &plan{kind: "instance", name: stringAt(inst, "name"), project: req.TargetProject}
	if p.project == "" {
		p.project = stringAt(inst, "project")
	}

	if p.project == "" {
		p.project = "default"
	}

	typ := "container"
	prefix := "backup/container"
	skip := []string{}

	if isVM {
		typ = "virtual-machine"
		prefix = "backup/virtual-machine"
		skip = []string{"root.img"}

		p.image, err = vmImage(fs, req.Source, dir)
		if err != nil {
			return nil, err
		}

		fi, err := os.Stat(p.image)
		if err == nil && fi.Mode().IsRegular() {
			p.total += fi.Size()
		} else {
			p.total += blockSize(p.image)
		}
	}

	p.trees = []tree{{dir: dir, prefix: prefix, skip: skip, replace: map[string][]byte{"backup.yaml": backupYAML}, idmap: set}}
	p.total += treeSize(dir, skip)

	optimized := false
	p.index = map[string]any{
		"name":             p.name,
		"backend":          fs.fsType,
		"pool":             req.TargetPool,
		"optimized":        optimized,
		"optimized_header": optimized,
		"type":             typ,
		"config":           doc,
	}

	return p, nil
}

// vmImage returns the disk image of a VM: root.img on a file system, the .block zvol on zfs.
func vmImage(fs *filesystem, source string, dir string) (string, error) {
	if fs.zpool != "" {
		path := "/dev/zvol/" + source + ".block"

		_, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("no disk image for %q: %w", source, err)
		}

		return path, nil
	}

	path := filepath.Join(dir, "root.img")

	_, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("no disk image for %q: %w", source, err)
	}

	return path, nil
}

func blockSize(path string) int64 {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return 0
	}

	defer func() { _ = f.Close() }()

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0
	}

	return size
}

// prepareInstanceConfig turns the old backup.yaml into the copy's: stopped, autostart off, snapshots left behind.
func prepareInstanceConfig(doc map[string]any, req api.SystemStorageImportCopyRequest) (idMap, error) {
	inst := mapAt(doc, "container")
	if inst == nil {
		return nil, errors.New("backup.yaml has no instance")
	}

	config := mapAt(inst, "config")
	if config == nil {
		config = map[string]any{}
		inst["config"] = config
	}

	if req.TargetName != "" {
		inst["name"] = req.TargetName
	}

	if req.TargetProject != "" {
		inst["project"] = req.TargetProject
	}

	old := stringAt(config, "boot.autostart")
	if old != "" {
		config["user.import.autostart"] = old
	}

	config["boot.autostart"] = "false"

	// Snapshots stay on the old drive; the copy is the instance as it was last.
	delete(doc, "snapshots")

	if len(req.Profiles) > 0 {
		inst["profiles"] = slices.Clone(req.Profiles)

		delete(doc, "profiles")
	}

	var set idMap

	if stringAt(inst, "type") != "virtual-machine" {
		last := stringAt(config, "volatile.last_state.idmap")
		if last != "" && last != "[]" {
			parsed, err := parseIDMap(last)
			if err != nil {
				return nil, fmt.Errorf("cannot read the instance's id map: %w", err)
			}

			set = parsed
		}

		// The files arrive unshifted, so Incus maps them freshly on first start.
		config["volatile.last_state.idmap"] = "[]"
	}

	return set, nil
}

func (m *Manager) volumePlan(ctx context.Context, d *openDisk, fs *filesystem, req api.SystemStorageImportCopyRequest) (*plan, error) {
	dir, err := m.sourceDir(ctx, d, fs, req.Source)
	if err != nil {
		return nil, err
	}

	name := req.TargetName
	project := req.TargetProject

	if name == "" {
		name = filepath.Base(req.Source)
		if req.Kind == "volume" {
			project, name = splitProject(name)
			if req.TargetProject != "" {
				project = req.TargetProject
			}
		}
	}

	if project == "" {
		project = "default"
	}

	if name == "" || name == "." || name == "/" || strings.Contains(name, "/") {
		return nil, errors.New("a target name is needed")
	}

	optimized := false
	p := &plan{
		kind:    "volume",
		name:    name,
		project: project,
		trees:   []tree{{dir: dir, prefix: "backup/volume"}},
		total:   treeSize(dir, nil),
	}

	p.index = map[string]any{
		"name":             name,
		"backend":          fs.fsType,
		"pool":             req.TargetPool,
		"optimized":        optimized,
		"optimized_header": optimized,
		"type":             "custom",
		"config": map[string]any{
			"volume": map[string]any{
				"name":         name,
				"type":         "custom",
				"content_type": "filesystem",
				"config":       map[string]any{},
				"description":  "Imported from " + req.Serial + ": " + req.Source,
			},
		},
	}

	return p, nil
}
