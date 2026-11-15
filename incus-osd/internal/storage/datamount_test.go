package storage

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
)

func TestValidateDataMounts(t *testing.T) {
	t.Parallel()

	got, err := ValidateDataMounts([]api.SystemStorageDataMount{
		{Name: "nvme", Device: "/dev/vg0/data"},
		{Name: "other", Device: "/dev/disk/by-id/dm-name-vg1-data", Filesystem: "btrfs", Options: "noatime"},
		{Name: "by-uuid", Device: "/dev/disk/by-uuid/0f1e2d3c-0000-4000-8000-000000000000"},
		{Name: "mapper_1", Device: "/dev/mapper/vg2-data"},
	})
	require.NoError(t, err)
	require.Equal(t, "btrfs", got[0].Filesystem)
	require.Equal(t, DefaultDataMountOptions, got[0].Options)
	require.Equal(t, "noatime", got[1].Options)

	empty, err := ValidateDataMounts([]api.SystemStorageDataMount{})
	require.NoError(t, err)
	require.NotNil(t, empty)

	bad := map[string]api.SystemStorageDataMount{
		"no device":         {Name: "a"},
		"bare kernel name":  {Name: "a", Device: "/dev/sda"},
		"relative path":     {Name: "a", Device: "dev/vg0/data"},
		"parent directory":  {Name: "a", Device: "/dev/disk/by-id/../../sda"},
		"unclean path":      {Name: "a", Device: "/dev//vg0/data"},
		"empty by-id":       {Name: "a", Device: "/dev/disk/by-id/"},
		"nested mapper":     {Name: "a", Device: "/dev/mapper/a/b"},
		"not a vg":          {Name: "a", Device: "/dev/pts/0"},
		"outside dev":       {Name: "a", Device: "/var/lib/data"},
		"ext4":              {Name: "a", Device: "/dev/vg0/data", Filesystem: "ext4"},
		"zfs":               {Name: "a", Device: "/dev/vg0/data", Filesystem: "zfs"},
		"name with slash":   {Name: "a/b", Device: "/dev/vg0/data"},
		"name parent dir":   {Name: "..", Device: "/dev/vg0/data"},
		"empty name":        {Device: "/dev/vg0/data"},
		"empty option":      {Name: "a", Device: "/dev/vg0/data", Options: "noatime,,compress=zstd:1"},
		"option with space": {Name: "a", Device: "/dev/vg0/data", Options: "noatime, ro"},
	}

	for name, mount := range bad {
		_, err := ValidateDataMounts([]api.SystemStorageDataMount{mount})
		require.Error(t, err, name)
	}

	_, err = ValidateDataMounts([]api.SystemStorageDataMount{{Name: "a", Device: "/dev/vg0/data"}, {Name: "a", Device: "/dev/vg0/other"}})
	require.ErrorContains(t, err, "used twice")

	_, err = ValidateDataMounts([]api.SystemStorageDataMount{{Name: "a", Device: "/dev/vg0/data"}, {Name: "b", Device: "/dev/vg0/data"}})
	require.ErrorContains(t, err, "used twice")
}

// fakeDataMountSystem plays the encrypted drive, lvm2's late activation and the kernel's mounts.
type fakeDataMountSystem struct {
	mu        sync.Mutex
	events    []string
	unlocked  bool
	unlockErr error
	// Polls before a device shows up once the drive is unlocked; -1 never.
	appearAfter map[string]int
	fsTypes     map[string]string
	mountErr    map[string]error
	mounts      map[string]string
	scrubErr    map[string]error
	// Devices whose mount never returns on its own, and the paths that are links to another device.
	hang    map[string]bool
	aliases map[string]string
}

func (f *fakeDataMountSystem) UnlockDrives(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("unlock")

	if f.unlockErr != nil {
		return f.unlockErr
	}

	f.unlocked = true

	return nil
}

func (f *fakeDataMountSystem) DeviceExists(device string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.unlocked || f.appearAfter[device] < 0 {
		return false
	}

	if f.appearAfter[device] > 0 {
		f.appearAfter[device]--

		return false
	}

	f.record("device " + device)

	return true
}

func (f *fakeDataMountSystem) ResolveDevice(device string) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	target, ok := f.aliases[device]
	if ok {
		return target
	}

	return device
}

func (f *fakeDataMountSystem) FilesystemType(_ context.Context, device string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("probe " + device)

	return f.fsTypes[device], nil
}

func (f *fakeDataMountSystem) MountedFrom(mountPoint string, device string) (bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	source, ok := f.mounts[mountPoint]

	return ok, source == device, nil
}

func (f *fakeDataMountSystem) Mount(ctx context.Context, device string, mountPoint string, filesystem string, options string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("mount " + device + " " + filepath.Base(mountPoint) + " " + filesystem + " " + options)

	if f.hang[device] {
		f.mu.Unlock()
		<-ctx.Done()
		f.mu.Lock()

		return ctx.Err()
	}

	if f.mountErr[device] != nil {
		return f.mountErr[device]
	}

	f.mounts[mountPoint] = device

	return nil
}

func (f *fakeDataMountSystem) Scrub(_ context.Context, mountPoint string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("scrub " + filepath.Base(mountPoint))

	return f.scrubErr[mountPoint]
}

func (f *fakeDataMountSystem) record(event string) {
	f.events = append(f.events, event)
}

// startIncus is what the boot does once local storage is up.
func (f *fakeDataMountSystem) startIncus() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("start incus")
}

func newFakeMounter(t *testing.T, sys *fakeDataMountSystem) *DataMounter {
	t.Helper()

	if sys.mounts == nil {
		sys.mounts = map[string]string{}
	}

	return &DataMounter{Sys: sys, Root: t.TempDir(), Wait: 200 * time.Millisecond, Poll: time.Millisecond, Timeout: 200 * time.Millisecond}
}

func TestDataMounterBootOrder(t *testing.T) {
	t.Parallel()

	sys := &fakeDataMountSystem{
		appearAfter: map[string]int{"/dev/vg0/data": 5},
		fsTypes:     map[string]string{"/dev/vg0/data": "btrfs"},
	}

	m := newFakeMounter(t, sys)
	mounts, err := ValidateDataMounts([]api.SystemStorageDataMount{{Name: "nvme", Device: "/dev/vg0/data"}})
	require.NoError(t, err)

	require.NoError(t, m.BringUp(t.Context(), mounts))
	sys.startIncus()

	// Unlock first, wait for the logical volume, probe it, mount it with the defaults, and only then start Incus.
	require.Equal(t, []string{
		"unlock",
		"device /dev/vg0/data",
		"probe /dev/vg0/data",
		"mount /dev/vg0/data nvme btrfs " + DefaultDataMountOptions,
		"start incus",
	}, sys.events)

	require.DirExists(t, filepath.Join(m.Root, "nvme"))
	require.Equal(t, []api.SystemStorageDataMountState{{Name: "nvme", MountPoint: filepath.Join(m.Root, "nvme"), Mounted: true}}, m.Status(mounts))

	// Running again (a configuration update) leaves a mounted one alone.
	m.Mount(t.Context(), mounts)
	require.Equal(t, []string{"device /dev/vg0/data"}, sys.events[5:])
}

func TestDataMounterFailureDoesNotBlockBoot(t *testing.T) {
	t.Parallel()

	sys := &fakeDataMountSystem{
		appearAfter: map[string]int{"/dev/vg0/gone": -1},
		fsTypes:     map[string]string{"/dev/vg0/ext4": "ext4", "/dev/vg0/broken": "btrfs", "/dev/vg0/good": "btrfs"},
		mountErr:    map[string]error{"/dev/vg0/broken": errors.New("wrong fs type, bad superblock")},
	}

	m := newFakeMounter(t, sys)
	mounts, err := ValidateDataMounts([]api.SystemStorageDataMount{
		{Name: "gone", Device: "/dev/vg0/gone"},
		{Name: "ext4", Device: "/dev/vg0/ext4"},
		{Name: "broken", Device: "/dev/vg0/broken"},
		{Name: "good", Device: "/dev/vg0/good"},
	})
	require.NoError(t, err)

	start := time.Now()

	// The wait for a missing device is bounded, and Incus still starts after the failures.
	require.NoError(t, m.BringUp(t.Context(), mounts))
	require.Less(t, time.Since(start), 5*time.Second)

	sys.startIncus()
	require.Equal(t, "start incus", sys.events[len(sys.events)-1])

	status := m.Status(mounts)
	require.Contains(t, status[0].Error, "did not appear")
	require.Contains(t, status[1].Error, "not btrfs")
	require.Contains(t, status[2].Error, "bad superblock")
	require.True(t, status[3].Mounted)

	for _, s := range status[:3] {
		require.False(t, s.Mounted)
	}

	// Never mounted a non-btrfs device.
	for _, event := range sys.events {
		require.NotContains(t, event, "mount /dev/vg0/ext4")
	}

	// Something else on the mount point is reported, not mounted over.
	sys.mounts[filepath.Join(m.Root, "gone")] = "/dev/sdz"
	require.Contains(t, m.Status(mounts)[0].Error, "another file system")
}

func TestDataMounterUnlockError(t *testing.T) {
	t.Parallel()

	sys := &fakeDataMountSystem{unlockErr: errors.New("no keys directory")}
	m := newFakeMounter(t, sys)

	require.Error(t, m.BringUp(t.Context(), []api.SystemStorageDataMount{{Name: "nvme", Device: "/dev/vg0/data", Filesystem: "btrfs"}}))
	require.Equal(t, []string{"unlock"}, sys.events)
}

func TestDataMounterScrub(t *testing.T) {
	t.Parallel()

	sys := &fakeDataMountSystem{}
	m := newFakeMounter(t, sys)

	sys.mounts[filepath.Join(m.Root, "good")] = "/dev/vg0/good"
	sys.mounts[filepath.Join(m.Root, "bad")] = "/dev/vg0/bad"
	sys.mounts[filepath.Join(m.Root, "other")] = "/dev/sdz"
	sys.scrubErr = map[string]error{filepath.Join(m.Root, "bad"): errors.New("uncorrectable errors")}

	err := m.Scrub(t.Context(), []api.SystemStorageDataMount{
		{Name: "absent", Device: "/dev/vg0/absent"},
		{Name: "bad", Device: "/dev/vg0/bad"},
		{Name: "other", Device: "/dev/vg0/other"},
		{Name: "good", Device: "/dev/vg0/good"},
	})

	// One failing scrub doesn't stop the next; unmounted or foreign mount points are left alone.
	require.ErrorContains(t, err, `data mount "bad": uncorrectable errors`)
	require.Equal(t, []string{"scrub bad", "scrub good"}, sys.events)
}

func TestDataMounterMountTimeout(t *testing.T) {
	t.Parallel()

	sys := &fakeDataMountSystem{
		fsTypes: map[string]string{"/dev/vg0/stuck": "btrfs", "/dev/vg0/good": "btrfs"},
		hang:    map[string]bool{"/dev/vg0/stuck": true},
	}

	m := newFakeMounter(t, sys)
	mounts, err := ValidateDataMounts([]api.SystemStorageDataMount{{Name: "stuck", Device: "/dev/vg0/stuck"}, {Name: "good", Device: "/dev/vg0/good"}})
	require.NoError(t, err)

	done := make(chan error, 1)

	go func() { done <- m.BringUp(t.Context(), mounts) }()

	// A mount that hangs is cut off, and the boot goes on to the next one and to Incus.
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "a hanging mount blocked the boot")
	}

	status := m.Status(mounts)
	require.Contains(t, status[0].Error, "deadline exceeded")
	require.True(t, status[1].Mounted)
}

func TestDataMounterSameDeviceTwice(t *testing.T) {
	t.Parallel()

	sys := &fakeDataMountSystem{
		fsTypes: map[string]string{"/dev/vg0/data": "btrfs", "/dev/mapper/vg0-data": "btrfs"},
		aliases: map[string]string{"/dev/vg0/data": "/dev/dm-3", "/dev/mapper/vg0-data": "/dev/dm-3"},
	}

	m := newFakeMounter(t, sys)

	// Two paths to one logical volume pass validation, but only the first is mounted.
	mounts, err := ValidateDataMounts([]api.SystemStorageDataMount{{Name: "a", Device: "/dev/vg0/data"}, {Name: "b", Device: "/dev/mapper/vg0-data"}})
	require.NoError(t, err)
	require.NoError(t, m.BringUp(t.Context(), mounts))

	status := m.Status(mounts)
	require.True(t, status[0].Mounted)
	require.Contains(t, status[1].Error, `same device as data mount "a"`)

	for _, event := range sys.events {
		require.NotContains(t, event, "mount /dev/mapper/vg0-data")
	}
}
