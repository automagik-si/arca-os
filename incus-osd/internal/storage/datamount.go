package storage

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lxc/incus/v7/shared/subprocess"

	"github.com/lxc/incus-os/incus-osd/api"
)

// DataMountRoot is where data mounts are mounted, one directory per name.
const DataMountRoot = "/var/lib/incus-os/mounts"

// DefaultDataMountOptions keeps writes down on consumer SSDs; autodefrag is left out as it rewrites files.
const DefaultDataMountOptions = "noatime,compress=zstd:1,discard=async"

var (
	dataMountName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	lvmName       = regexp.MustCompile(`^[A-Za-z0-9_.+][A-Za-z0-9_.+-]*$`)

	// Directories directly under /dev that are never a volume group.
	notVolumeGroups = []string{"block", "bus", "char", "cpu", "disk", "dri", "fd", "hugepages", "input", "mapper", "mqueue", "net", "pts", "shm", "snd", "vfio"}
)

// ValidateDataMounts checks the data mounts and returns them with their defaults filled in.
func ValidateDataMounts(mounts []api.SystemStorageDataMount) ([]api.SystemStorageDataMount, error) {
	ret := make([]api.SystemStorageDataMount, 0, len(mounts))
	names := map[string]bool{}
	devices := map[string]bool{}

	for _, mount := range mounts {
		if !dataMountName.MatchString(mount.Name) {
			return nil, fmt.Errorf("data mount name %q must be 1-64 letters, digits, '-' or '_', starting with a letter or digit", mount.Name)
		}

		if names[mount.Name] {
			return nil, fmt.Errorf("data mount name %q is used twice", mount.Name)
		}

		names[mount.Name] = true

		err := validateDataMountDevice(mount.Device)
		if err != nil {
			return nil, fmt.Errorf("data mount %q: %w", mount.Name, err)
		}

		if devices[mount.Device] {
			return nil, fmt.Errorf("data mount %q: device %q is used twice", mount.Name, mount.Device)
		}

		devices[mount.Device] = true

		if mount.Filesystem == "" {
			mount.Filesystem = "btrfs"
		}

		if mount.Filesystem != "btrfs" {
			return nil, fmt.Errorf("data mount %q: filesystem %q is not supported, only btrfs is", mount.Name, mount.Filesystem)
		}

		if mount.Options == "" {
			mount.Options = DefaultDataMountOptions
		}

		for option := range strings.SplitSeq(mount.Options, ",") {
			if option == "" || strings.ContainsAny(option, " \t\n\r") {
				return nil, fmt.Errorf("data mount %q: invalid mount options %q", mount.Name, mount.Options)
			}
		}

		ret = append(ret, mount)
	}

	return ret, nil
}

func validateDataMountDevice(device string) error {
	if device == "" {
		return errors.New("no device given")
	}

	if filepath.Clean(device) != device || strings.Contains(device, "..") {
		return fmt.Errorf("device %q is not a clean path", device)
	}

	for _, prefix := range []string{"/dev/disk/by-id/", "/dev/disk/by-uuid/", "/dev/mapper/"} {
		name, ok := strings.CutPrefix(device, prefix)
		if ok && name != "" && !strings.Contains(name, "/") {
			return nil
		}
	}

	// An LVM logical volume, /dev/<vg>/<lv>.
	parts := strings.Split(strings.TrimPrefix(device, "/dev/"), "/")
	if strings.HasPrefix(device, "/dev/") && len(parts) == 2 && !slices.Contains(notVolumeGroups, parts[0]) && lvmName.MatchString(parts[0]) && lvmName.MatchString(parts[1]) {
		return nil
	}

	return fmt.Errorf("device %q must be /dev/disk/by-id/<id>, /dev/disk/by-uuid/<uuid>, /dev/mapper/<name> or /dev/<vg>/<lv>", device)
}

// DataMountSystem is what a DataMounter needs from the host.
type DataMountSystem interface {
	// UnlockDrives opens every encrypted drive that has a key file.
	UnlockDrives(ctx context.Context) error
	DeviceExists(device string) bool
	FilesystemType(ctx context.Context, device string) (string, error)
	// MountedFrom reports whether mountPoint is mounted and, if so, whether device is its source.
	MountedFrom(mountPoint string, device string) (bool, bool, error)
	Mount(ctx context.Context, device string, mountPoint string, filesystem string, options string) error
}

// DataMounter mounts the configured data mounts and remembers why one failed.
type DataMounter struct {
	Sys  DataMountSystem
	Root string
	// Wait bounds the time spent waiting for devices to appear, shared by all mounts of one call.
	Wait time.Duration
	Poll time.Duration

	// run keeps a boot and a configuration update from mounting at the same time.
	run      sync.Mutex
	mu       sync.Mutex
	failures map[string]string
}

var defaultDataMounter = &DataMounter{Sys: realDataMountSystem{}, Root: DataMountRoot, Wait: 30 * time.Second, Poll: 250 * time.Millisecond}

// BringUpEncryptedStorage unlocks the keyed encrypted drives, then mounts the data mounts.
func BringUpEncryptedStorage(ctx context.Context, mounts []api.SystemStorageDataMount) error {
	return defaultDataMounter.BringUp(ctx, mounts)
}

// MountDataMounts mounts any data mount that isn't mounted yet.
func MountDataMounts(ctx context.Context, mounts []api.SystemStorageDataMount) {
	defaultDataMounter.Mount(ctx, mounts)
}

// DataMountStatus returns the state of each data mount.
func DataMountStatus(mounts []api.SystemStorageDataMount) []api.SystemStorageDataMountState {
	return defaultDataMounter.Status(mounts)
}

// BringUp unlocks the encrypted drives, then mounts the data mounts. Only an unlock error is returned:
// a data mount that fails is logged and reported by Status, and must never stop the system from booting.
func (m *DataMounter) BringUp(ctx context.Context, mounts []api.SystemStorageDataMount) error {
	err := m.Sys.UnlockDrives(ctx)
	if err != nil {
		return err
	}

	m.Mount(ctx, mounts)

	return nil
}

// Mount mounts any data mount that isn't mounted yet.
func (m *DataMounter) Mount(ctx context.Context, mounts []api.SystemStorageDataMount) {
	m.run.Lock()
	defer m.run.Unlock()

	deadline := time.Now().Add(m.Wait)

	for _, mount := range mounts {
		err := m.mountOne(ctx, mount, deadline)

		m.mu.Lock()

		if m.failures == nil {
			m.failures = map[string]string{}
		}

		if err != nil {
			m.failures[mount.Name] = err.Error()
			slog.ErrorContext(ctx, "Couldn't mount data mount", "name", mount.Name, "device", mount.Device, "err", err)
		} else {
			delete(m.failures, mount.Name)
			slog.InfoContext(ctx, "Data mount is mounted", "name", mount.Name, "device", mount.Device)
		}

		m.mu.Unlock()
	}
}

// Status returns the state of each data mount, with the error of its last failed attempt.
func (m *DataMounter) Status(mounts []api.SystemStorageDataMount) []api.SystemStorageDataMountState {
	ret := make([]api.SystemStorageDataMountState, 0, len(mounts))

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, mount := range mounts {
		state := api.SystemStorageDataMountState{
			Name:       mount.Name,
			MountPoint: filepath.Join(m.Root, mount.Name),
		}

		mounted, same, err := m.Sys.MountedFrom(state.MountPoint, mount.Device)

		switch {
		case err != nil:
			state.Error = err.Error()
		case mounted && same:
			state.Mounted = true
		case mounted:
			state.Error = "another file system is mounted at " + state.MountPoint
		case m.failures[mount.Name] != "":
			state.Error = m.failures[mount.Name]
		default:
			state.Error = "not mounted yet"
		}

		ret = append(ret, state)
	}

	return ret
}

func (m *DataMounter) mountOne(ctx context.Context, mount api.SystemStorageDataMount, deadline time.Time) error {
	mountPoint := filepath.Join(m.Root, mount.Name)

	// The encrypted drive was just opened and lvm2 activates its volumes asynchronously.
	for !m.Sys.DeviceExists(mount.Device) {
		if !time.Now().Before(deadline) {
			return fmt.Errorf("device %s did not appear within %s (is its encrypted drive unlocked and its volume group active?)", mount.Device, m.Wait)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.Poll):
		}
	}

	mounted, same, err := m.Sys.MountedFrom(mountPoint, mount.Device)
	if err != nil {
		return err
	}

	if mounted {
		if !same {
			return fmt.Errorf("another file system is already mounted at %s", mountPoint)
		}

		return nil
	}

	fsType, err := m.Sys.FilesystemType(ctx, mount.Device)
	if err != nil {
		return err
	}

	if fsType != mount.Filesystem {
		return fmt.Errorf("device %s holds %q, not %s: nothing was mounted", mount.Device, fsType, mount.Filesystem)
	}

	err = os.MkdirAll(mountPoint, 0o700)
	if err != nil {
		return err
	}

	return m.Sys.Mount(ctx, mount.Device, mountPoint, mount.Filesystem, mount.Options)
}

type realDataMountSystem struct{}

func (realDataMountSystem) UnlockDrives(ctx context.Context) error {
	return DecryptDrives(ctx)
}

func (realDataMountSystem) DeviceExists(device string) bool {
	_, err := os.Stat(device)

	return err == nil
}

func (realDataMountSystem) FilesystemType(ctx context.Context, device string) (string, error) {
	// A low-level probe that only reads the device.
	output, err := subprocess.RunCommandContext(ctx, "blkid", "-p", "-o", "value", "-s", "TYPE", device)
	if err != nil {
		return "", fmt.Errorf("couldn't probe the file system on %s: %w", device, err)
	}

	return strings.TrimSpace(output), nil
}

func (realDataMountSystem) MountedFrom(mountPoint string, device string) (bool, bool, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, false, err
	}

	defer f.Close()

	wanted, err := filepath.EvalSymlinks(device)
	if err != nil {
		wanted = device
	}

	mounted := false
	same := false
	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		// Fields: id parent major:minor root mount-point options [optional...] - type source super-options.
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 || fields[4] != mountPoint {
			continue
		}

		sep := slices.Index(fields, "-")
		if sep < 0 || sep+2 >= len(fields) {
			continue
		}

		// The last mount on a path is the one in use.
		source, err := filepath.EvalSymlinks(fields[sep+2])
		if err != nil {
			source = fields[sep+2]
		}

		mounted = true
		same = source == wanted
	}

	return mounted, same, scanner.Err()
}

func (realDataMountSystem) Mount(ctx context.Context, device string, mountPoint string, filesystem string, options string) error {
	_, err := subprocess.RunCommandContext(ctx, "mount", "-t", filesystem, "-o", options, device, mountPoint)

	return err
}
