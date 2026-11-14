package diskimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/lxc/incus-os/incus-osd/api"
)

// DefaultRoot is where import file systems are mounted.
const DefaultRoot = "/run/incus-os/import"

// defaultUdevRules keeps udev from acting on the device-mapper devices an import creates.
const defaultUdevRules = "/run/udev/rules.d/68-incus-os-import.rules"

// udevRule stops the lvm2 rule (69-*lvm*.rules) from autoactivating, read-write, a volume group found on an import map.
const udevRule = `# Written by incus-osd: an import drive is only ever activated read-only, by incus-osd.
SUBSYSTEM=="block", ENV{DM_NAME}=="import-*", ENV{DM_UDEV_DISABLE_OTHER_RULES_FLAG}="1"
`

// ErrPassphraseNeeded is returned when an encrypted layer is found and no passphrase was given.
var ErrPassphraseNeeded = errors.New("the drive is encrypted: a passphrase is needed")

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// Manager keeps the drives opened for import and the copy jobs.
type Manager struct {
	mu     sync.Mutex
	run    Runner
	sys    System
	incus  Incus
	root   string
	disks  map[string]*openDisk
	copies []*copyJob
	nextID int

	resolve   func(string) (string, error)
	exists    func(string) bool
	udevRules string
}

type undoStep struct {
	name string
	args []string
}

type filesystem struct {
	name   string
	fsType string
	mount  string
	zpool  string
	info   api.SystemStorageImportFilesystem
}

type openDisk struct {
	serial     string
	id         string
	layers     []string
	fss        []*filesystem
	unreadable []string
	undo       []undoStep
	busy       int
	seq        int
}

// NewManager returns a manager acting on the real system.
func NewManager(sys System, incus Incus) *Manager {
	return buildManager(execRunner{}, sys, incus, DefaultRoot)
}

func buildManager(run Runner, sys System, incus Incus, root string) *Manager {
	return &Manager{run: run, sys: sys, incus: incus, root: root, disks: map[string]*openDisk{}, resolve: filepath.EvalSymlinks, exists: pathExists, udevRules: defaultUdevRules}
}

func pathExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

// State returns the open drives and the copy jobs.
func (m *Manager) State() api.SystemStorageImport {
	m.mu.Lock()
	defer m.mu.Unlock()

	ret := api.SystemStorageImport{Disks: []api.SystemStorageImportDisk{}, Copies: []api.SystemStorageImportCopy{}}

	serials := make([]string, 0, len(m.disks))
	for serial := range m.disks {
		serials = append(serials, serial)
	}

	slices.Sort(serials)

	for _, serial := range serials {
		d := m.disks[serial]

		disk := api.SystemStorageImportDisk{
			Serial:      d.serial,
			ID:          d.id,
			Layers:      slices.Clone(d.layers),
			Filesystems: []api.SystemStorageImportFilesystem{},
			Unreadable:  slices.Clone(d.unreadable),
		}

		for _, fs := range d.fss {
			disk.Filesystems = append(disk.Filesystems, fs.info)
		}

		ret.Disks = append(ret.Disks, disk)
	}

	for _, job := range m.copies {
		ret.Copies = append(ret.Copies, job.snapshot())
	}

	return ret
}

// Open opens the drive with that serial read-only and lists what is on it.
func (m *Manager) Open(ctx context.Context, serial string, passphrase string) (api.SystemStorageImportDisk, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	_, ok := m.disks[serial]
	if ok {
		return api.SystemStorageImportDisk{}, fmt.Errorf("drive %q is already open", serial)
	}

	drive, err := m.findDrive(ctx, serial)
	if err != nil {
		return api.SystemStorageImportDisk{}, err
	}

	devPath, err := m.resolve(drive.ID)
	if err != nil {
		return api.SystemStorageImportDisk{}, err
	}

	parts, err := m.inUse(ctx, devPath)
	if err != nil {
		return api.SystemStorageImportDisk{}, err
	}

	d := &openDisk{serial: serial, id: drive.ID}

	err = m.openDisk(ctx, d, devPath, parts, passphrase)
	if err != nil {
		undoErr := m.undoAll(ctx, d)
		if undoErr != nil {
			// Kept so that a close can finish the job.
			slog.Error("Import drive left partly open", "serial", serial, "err", undoErr)

			d.layers = nil
			d.fss = nil
			d.unreadable = []string{"partly open after a failed open: close it to finish closing"}
			m.disks[serial] = d
		}

		return api.SystemStorageImportDisk{}, err
	}

	m.disks[serial] = d
	slog.Info("Import drive opened read-only", "serial", serial, "layers", d.layers)

	return m.diskInfo(d), nil
}

// Close undoes everything Open did to the drive.
func (m *Manager) Close(ctx context.Context, serial string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	d, ok := m.disks[serial]
	if !ok {
		return fmt.Errorf("drive %q is not open", serial)
	}

	if d.busy > 0 {
		return fmt.Errorf("drive %q has a copy running", serial)
	}

	err := m.undoAll(ctx, d)
	if err != nil {
		return err
	}

	_ = os.Remove(filepath.Join(m.root, unsafeName.ReplaceAllString(serial, "_")))

	delete(m.disks, serial)
	slog.Info("Import drive closed", "serial", serial)

	return nil
}

var poolName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// Convert wipes a closed import drive and makes it a native pool; confirm must repeat the serial.
func (m *Manager) Convert(ctx context.Context, serial string, pool string, confirm string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if confirm != serial {
		return errors.New("the confirmation does not match the serial")
	}

	if !poolName.MatchString(pool) {
		return fmt.Errorf("invalid pool name %q", pool)
	}

	_, ok := m.disks[serial]
	if ok {
		return fmt.Errorf("drive %q is open for import: close it first", serial)
	}

	drive, err := m.findDrive(ctx, serial)
	if err != nil {
		return err
	}

	if m.sys.PoolExists(ctx, pool) {
		return fmt.Errorf("pool %q already exists", pool)
	}

	devPath, err := m.resolve(drive.ID)
	if err != nil {
		return err
	}

	_, err = m.inUse(ctx, devPath)
	if err != nil {
		return err
	}

	slog.Warn("Converting import drive to a native pool", "serial", serial, "pool", pool)

	err = m.sys.WipeDrive(ctx, drive.ID)
	if err != nil {
		return err
	}

	return m.sys.CreatePool(ctx, pool, drive.ID)
}

// findDrive resolves a serial to exactly one drive that may be imported or converted.
func (m *Manager) findDrive(ctx context.Context, serial string) (api.SystemStorageDrive, error) {
	if serial == "" {
		return api.SystemStorageDrive{}, errors.New("no serial given")
	}

	if slices.Contains(m.sys.ProtectedSerials(), serial) {
		return api.SystemStorageDrive{}, fmt.Errorf("drive %q is protected", serial)
	}

	drives, err := m.sys.Drives(ctx)
	if err != nil {
		return api.SystemStorageDrive{}, err
	}

	var found []api.SystemStorageDrive

	for _, d := range drives {
		if d.SerialNumber == serial {
			found = append(found, d)
		}
	}

	if len(found) == 0 {
		return api.SystemStorageDrive{}, fmt.Errorf("no drive with serial %q", serial)
	}

	if len(found) > 1 {
		return api.SystemStorageDrive{}, fmt.Errorf("more than one drive with serial %q", serial)
	}

	d := found[0]

	switch {
	case d.Boot:
		return d, errors.New("the drive is the boot drive")
	case d.MemberPool != "":
		return d, fmt.Errorf("the drive belongs to pool %q", d.MemberPool)
	case d.Remote:
		return d, errors.New("the drive is a remote drive")
	case d.Encrypted:
		return d, errors.New("the drive is an encrypted drive managed by this system")
	case !strings.HasPrefix(d.ID, "/dev/disk/by-id/"):
		return d, errors.New("the drive has no stable id")
	}

	return d, nil
}

type lsblkNode struct {
	Name        string      `json:"name"`
	Type        string      `json:"type"`
	FSType      *string     `json:"fstype"`
	Mountpoints []*string   `json:"mountpoints"`
	Children    []lsblkNode `json:"children"`
}

// inUse refuses a drive that is mounted, held by a layer or given to an instance.
func (m *Manager) inUse(ctx context.Context, devPath string) ([]lsblkNode, error) {
	out, err := m.run.Run(ctx, "", "lsblk", "-J", "-b", "-p", "-o", "NAME,TYPE,FSTYPE,MOUNTPOINTS", devPath)
	if err != nil {
		return nil, err
	}

	tree := struct {
		Blockdevices []lsblkNode `json:"blockdevices"`
	}{}

	err = json.Unmarshal([]byte(out), &tree)
	if err != nil {
		return nil, err
	}

	if len(tree.Blockdevices) != 1 {
		return nil, fmt.Errorf("cannot read the layout of %s", devPath)
	}

	disk := tree.Blockdevices[0]
	paths := []string{disk.Name}

	nodes := append([]lsblkNode{disk}, disk.Children...)
	for i, n := range nodes {
		if i > 0 && n.Type != "part" {
			return nil, fmt.Errorf("%s is held by a %s device", disk.Name, n.Type)
		}

		for _, mp := range n.Mountpoints {
			if mp != nil && *mp != "" {
				return nil, fmt.Errorf("%s is mounted at %s", n.Name, *mp)
			}
		}

		if i > 0 {
			paths = append(paths, n.Name)

			if len(n.Children) > 0 {
				return nil, fmt.Errorf("%s is held by a %s device", n.Name, n.Children[0].Type)
			}
		}
	}

	sources, err := m.incus.DiskSources(ctx)
	if err != nil {
		return nil, err
	}

	for src, inst := range sources {
		target, err := filepath.EvalSymlinks(src)
		if err != nil {
			target = src
		}

		if slices.Contains(paths, target) || slices.Contains(paths, src) {
			return nil, fmt.Errorf("the drive is in use by instance %q", inst)
		}
	}

	return disk.Children, nil
}

func (*Manager) diskInfo(d *openDisk) api.SystemStorageImportDisk {
	ret := api.SystemStorageImportDisk{Serial: d.serial, ID: d.id, Layers: d.layers, Unreadable: d.unreadable}
	for _, fs := range d.fss {
		ret.Filesystems = append(ret.Filesystems, fs.info)
	}

	return ret
}

func (m *Manager) openDisk(ctx context.Context, d *openDisk, devPath string, parts []lsblkNode, passphrase string) error {
	err := m.installUdevRule(ctx)
	if err != nil {
		return err
	}

	targets := make([]string, 0, 1+len(parts))
	targets = append(targets, devPath)

	for _, p := range parts {
		targets = append(targets, p.Name)
	}

	// The kernel refuses writes below every layer from here on.
	for _, t := range targets {
		_, err := m.run.Run(ctx, "", "blockdev", "--setro", t)
		if err != nil {
			return err
		}

		d.undo = append(d.undo, undoStep{"blockdev", []string{"--setrw", t}})
	}

	if len(parts) == 0 {
		return m.openLayer(ctx, d, devPath, filepath.Base(devPath), passphrase, 0)
	}

	for _, p := range parts {
		err := m.openLayer(ctx, d, p.Name, filepath.Base(p.Name), passphrase, 0)
		if err != nil {
			return err
		}
	}

	return nil
}

// installUdevRule makes sure udev leaves import maps alone before the first one is created.
func (m *Manager) installUdevRule(ctx context.Context) error {
	current, err := os.ReadFile(m.udevRules)
	if err == nil && string(current) == udevRule {
		return nil
	}

	err = os.MkdirAll(filepath.Dir(m.udevRules), 0o755)
	if err != nil {
		return err
	}

	err = os.WriteFile(m.udevRules, []byte(udevRule), 0o644) //nolint:gosec
	if err != nil {
		return err
	}

	_, err = m.run.Run(ctx, "", "udevadm", "control", "--reload")

	return err
}

// requireReadOnly sets a device read-only and refuses it unless the kernel confirms it.
func (m *Manager) requireReadOnly(ctx context.Context, dev string, set bool) error {
	if set {
		_, err := m.run.Run(ctx, "", "blockdev", "--setro", dev)
		if err != nil {
			return err
		}
	}

	out, err := m.run.Run(ctx, "", "blockdev", "--getro", dev)
	if err != nil {
		return err
	}

	if strings.TrimSpace(out) != "1" {
		return fmt.Errorf("%s is not read-only: refusing to read it", dev)
	}

	return nil
}

func (m *Manager) probe(ctx context.Context, dev string) string {
	out, err := m.run.Run(ctx, "", "blkid", "-p", "-s", "TYPE", "-o", "value", dev)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(out)
}

func (*Manager) addLayer(d *openDisk, layer string) {
	if !slices.Contains(d.layers, layer) {
		d.layers = append(d.layers, layer)
	}
}

func (m *Manager) openLayer(ctx context.Context, d *openDisk, dev string, name string, passphrase string, depth int) error {
	if depth > 4 {
		d.unreadable = append(d.unreadable, name+": too many nested layers")

		return nil
	}

	err := m.requireReadOnly(ctx, dev, depth > 0)
	if err != nil {
		return err
	}

	fsType := m.probe(ctx, dev)

	switch fsType {
	case "crypto_LUKS":
		return m.openLUKS(ctx, d, dev, name, passphrase, depth)
	case "LVM2_member":
		return m.openLVM(ctx, d, dev, name, passphrase, depth)
	case "zfs_member":
		return m.openZFS(ctx, d, dev, passphrase)
	case "btrfs", "ext2", "ext3", "ext4", "xfs", "vfat", "exfat":
		return m.mountFS(ctx, d, dev, name, fsType)
	case "":
		d.unreadable = append(d.unreadable, name+": no file system")
	default:
		d.unreadable = append(d.unreadable, name+": "+fsType+" is not readable here")
	}

	return nil
}

func (m *Manager) openLUKS(ctx context.Context, d *openDisk, dev string, name string, passphrase string, depth int) error {
	if passphrase == "" {
		return ErrPassphraseNeeded
	}

	d.seq++
	mapName := fmt.Sprintf("import-%s-%d", unsafeName.ReplaceAllString(d.serial, "_"), d.seq)

	_, err := m.run.Run(ctx, passphrase, "cryptsetup", "open", "--readonly", "--type", "luks", "--key-file=-", dev, mapName)
	if err != nil {
		return fmt.Errorf("the passphrase did not open %s", name)
	}

	d.undo = append(d.undo, undoStep{"cryptsetup", []string{"close", mapName}})
	m.addLayer(d, "luks")

	return m.openLayer(ctx, d, "/dev/mapper/"+mapName, name, passphrase, depth+1)
}

func (m *Manager) openLVM(ctx context.Context, d *openDisk, dev string, name string, passphrase string, depth int) error {
	out, err := m.run.Run(ctx, "", "pvs", "--noheadings", "-o", "vg_name", dev)
	if err != nil {
		return err
	}

	vg := strings.TrimSpace(out)
	if vg == "" {
		d.unreadable = append(d.unreadable, name+": LVM member with no volume group")

		return nil
	}

	out, err = m.run.Run(ctx, "", "vgs", "--noheadings", "-o", "vg_name")
	if err != nil {
		return err
	}

	count := 0

	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) == vg {
			count++
		}
	}

	if count > 1 {
		return fmt.Errorf("volume group %q exists on this system too", vg)
	}

	readOnly := fmt.Sprintf(`global{metadata_read_only=1} activation{read_only_volume_list=["%s"]}`, vg)

	// Registered first: a half-done activation is undone too.
	d.undo = append(d.undo, undoStep{"vgchange", []string{"-an", "--config", readOnly, vg}})

	// The kernel refuses a thin pool in a read-only table: lvm2 then activates the rest and fails.
	_, activateErr := m.run.Run(ctx, "", "vgchange", "-ay", "--config", readOnly, vg)

	m.addLayer(d, "lvm")

	out, err = m.run.Run(ctx, "", "lvs", "--noheadings", "--separator", "|", "-o", "lv_name,lv_path,lv_active,lv_layout", vg)
	if err != nil {
		return errors.Join(activateErr, err)
	}

	if activateErr != nil {
		slog.Warn("Import volume group partly activated", "serial", d.serial, "vg", vg, "err", activateErr)
	}

	// lv_layout is a per-LV field: a segment field such as segtype would print one row per segment.
	seen := map[string]bool{}

	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) != 4 {
			continue
		}

		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}

		if fields[0] == "" || seen[fields[0]] {
			continue
		}

		seen[fields[0]] = true
		name := vg + "/" + fields[0]

		// Incus and lvm2 thin snapshots set activation skip, and a thin pool is not a block device.
		switch {
		case fields[3] == "thin,pool":
			d.unreadable = append(d.unreadable, name+": thin pool (not a block device)")

			continue
		case fields[2] == "" && fields[3] == "thin,sparse":
			d.unreadable = append(d.unreadable, name+": thin volume (its pool cannot run read-only)")

			continue
		case fields[2] == "":
			d.unreadable = append(d.unreadable, name+": not active")

			continue
		case fields[1] == "" || !m.exists(fields[1]):
			d.unreadable = append(d.unreadable, name+": no device")

			continue
		default:
		}

		err = m.openLayer(ctx, d, fields[1], name, passphrase, depth+1)
		if err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) mountPath(d *openDisk, name string) string {
	return filepath.Join(m.root, unsafeName.ReplaceAllString(d.serial, "_"), unsafeName.ReplaceAllString(name, "_"))
}

func (m *Manager) mountFS(ctx context.Context, d *openDisk, dev string, name string, fsType string) error {
	opts := "ro"

	switch fsType {
	case "btrfs":
		opts = "ro,rescue=nologreplay,subvolid=5"
	case "ext3", "ext4":
		opts = "ro,noload"
	case "xfs":
		opts = "ro,norecovery"
	default:
	}

	target := m.mountPath(d, name)

	err := os.MkdirAll(target, 0o700)
	if err != nil {
		return err
	}

	_, err = m.run.Run(ctx, "", "mount", "-t", fsType, "-o", opts, dev, target)
	if err != nil {
		_ = os.Remove(target)

		return err
	}

	d.undo = append(d.undo, undoStep{"rmdir", []string{target}}, undoStep{"umount", []string{target}})
	m.addLayer(d, fsType)

	fs := &filesystem{name: name, fsType: fsType, mount: target}
	fs.info = scanTree(target)
	fs.info.Name = name
	fs.info.Type = fsType
	d.fss = append(d.fss, fs)

	return nil
}

// undoAll reverses every step taken on the drive, last first.
func (m *Manager) undoAll(ctx context.Context, d *openDisk) error {
	for i, step := range slices.Backward(d.undo) {
		var err error
		if step.name == "rmdir" {
			err = os.Remove(step.args[0])
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		} else {
			_, err = m.run.Run(ctx, "", step.name, step.args...)
		}

		// Stop at the first failure: what lies below must stay as it is under what is still open.
		if err != nil {
			return fmt.Errorf("%s %s: %w", step.name, strings.Join(step.args, " "), err)
		}

		d.undo = d.undo[:i]
	}

	return nil
}
