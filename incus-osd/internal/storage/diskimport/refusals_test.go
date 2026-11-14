package diskimport

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
)

func TestOpenMoreRefusals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		setup func(*fakeRunner, *fakeSystem)
		want  string
	}{
		{"remote", func(_ *fakeRunner, s *fakeSystem) { s.drives[1].Remote = true }, "remote drive"},
		{"managed encrypted", func(_ *fakeRunner, s *fakeSystem) { s.drives[1].Encrypted = true }, "managed by this system"},
		{"no stable id", func(_ *fakeRunner, s *fakeSystem) { s.drives[1].ID = "/dev/nvme9n1" }, "no stable id"},
		{"duplicate volume group", func(r *fakeRunner, _ *fakeSystem) { r.answers["vgs "] = "  vg0\n  vg0\n" }, `volume group "vg0" exists on this system too`}, //nolint:dupword
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m, run, sys, _ := newTest(t)
			tc.setup(run, sys)

			_, err := m.Open(context.Background(), testSerial, testPass)
			require.ErrorContains(t, err, tc.want)
			require.Equal(t, -1, indexPrefix(run.cmds(), "mount "))
			require.Empty(t, m.State().Disks)
		})
	}
}

func TestConvertRefusals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		serial string
		setup  func(*fakeRunner, *fakeSystem, *fakeIncus)
		want   string
	}{
		{"boot drive", "BOOT1", nil, "boot drive"},
		{"pool member", testSerial, func(_ *fakeRunner, s *fakeSystem, _ *fakeIncus) { s.drives[1].MemberPool = "local" }, "belongs to pool"},
		{"mounted", testSerial, func(r *fakeRunner, _ *fakeSystem, _ *fakeIncus) {
			r.answers["lsblk "] = `{"blockdevices":[{"name":"/dev/nvme9n1","type":"disk","mountpoints":[null],
"children":[{"name":"/dev/nvme9n1p1","type":"part","mountpoints":["/mnt"]}]}]}`
		}, "is mounted at /mnt"},
		{"given to a VM", testSerial, func(_ *fakeRunner, _ *fakeSystem, i *fakeIncus) {
			i.sources = map[string]string{"/dev/nvme9n1p1": "default/seats"}
		}, `in use by instance "default/seats"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m, run, sys, incus := newTest(t)
			if tc.setup != nil {
				tc.setup(run, sys, incus)
			}

			err := m.Convert(context.Background(), tc.serial, "data", tc.serial)
			require.ErrorContains(t, err, tc.want)
			require.Empty(t, sys.wiped)
			require.Empty(t, sys.created)
		})
	}
}

func TestOpenMountOptions(t *testing.T) {
	t.Parallel()

	for fsType, opts := range map[string]string{"ext4": "ro,noload", "ext3": "ro,noload", "xfs": "ro,norecovery", "vfat": "ro"} {
		t.Run(fsType, func(t *testing.T) {
			t.Parallel()

			m, run, _, _ := newTest(t)
			run.answers["blkid -p -s TYPE -o value /dev/nvme9n1p1"] = fsType

			disk, err := m.Open(context.Background(), testSerial, "")
			require.NoError(t, err)
			require.Equal(t, []string{fsType}, disk.Layers)
			require.Contains(t, run.cmds(), "mount -t "+fsType+" -o "+opts+" /dev/nvme9n1p1 "+m.mountPath(m.disks[testSerial], "nvme9n1p1"))
			requireReadOnlyBeforeUse(t, run.cmds())
		})
	}
}

const zpoolImport = `   pool: tank
     id: 1234567890
  state: ONLINE
 action: The pool can be imported using its name or numeric identifier.
`

func zfsTest(t *testing.T) (*Manager, *fakeRunner) {
	t.Helper()

	m, run, _, _ := newTest(t)
	run.answers["blkid -p -s TYPE -o value /dev/nvme9n1p1"] = "zfs_member"
	run.answers["zpool import -d /dev/nvme9n1p1"] = zpoolImport
	run.answers["zpool list "] = "local\n"
	run.answers["zfs list "] = "tank\tfilesystem\t-\t-\ntank/incus\tfilesystem\ttank/incus\tunavailable\ntank/incus/containers/web\tfilesystem\ttank/incus\tunavailable\n"

	return m, run
}

func TestOpenZFS(t *testing.T) {
	t.Parallel()

	m, run := zfsTest(t)

	disk, err := m.Open(context.Background(), testSerial, testPass)
	require.NoError(t, err)
	require.Equal(t, []string{"zfs"}, disk.Layers)
	require.Equal(t, "web", disk.Filesystems[0].Instances[0].Name)

	altroot := m.mountPath(m.disks[testSerial], "zfs-tank")
	require.Contains(t, run.cmds(), "zpool import -d /dev/nvme9n1p1 -o readonly=on -N -f -R "+altroot+" 1234567890")

	for _, c := range run.calls {
		if strings.HasPrefix(c.cmd, "zfs load-key") {
			require.Equal(t, "zfs load-key -L prompt tank/incus", c.cmd)
			require.Equal(t, testPass, c.stdin)
		}
	}

	_, err = m.Open(context.Background(), testSerial, testPass)
	require.ErrorContains(t, err, "already open")
}

func TestOpenZFSRefusesDuplicatePool(t *testing.T) {
	t.Parallel()

	m, run := zfsTest(t)
	run.answers["zpool list "] = "local\ntank\n"

	_, err := m.Open(context.Background(), testSerial, testPass)
	require.ErrorContains(t, err, `zfs pool "tank" exists on this system too`)
	require.Equal(t, -1, indexPrefix(run.cmds(), "zpool import -d /dev/nvme9n1p1 -o"))
}

func TestMountDatasetLegacy(t *testing.T) {
	t.Parallel()

	for mountpoint, opts := range map[string]string{"legacy": "ro", "none": "ro,zfsutil", "/var/lib/x": "ro,zfsutil"} {
		t.Run(mountpoint, func(t *testing.T) {
			t.Parallel()

			m, run := zfsTest(t)

			_, err := m.Open(context.Background(), testSerial, testPass)
			require.NoError(t, err)

			run.answers["zfs get -H -o value mountpoint tank/incus/containers/web"] = mountpoint + "\n"

			d := m.disks[testSerial]
			target, err := m.mountDataset(context.Background(), d, "tank/incus/containers/web")
			require.NoError(t, err)
			require.Contains(t, run.cmds(), "mount -t zfs -o "+opts+" tank/incus/containers/web "+target)
		})
	}
}

func TestRefuseProtected(t *testing.T) {
	t.Parallel()

	sys := &fakeSystem{drives: []api.SystemStorageDrive{
		{ID: "/dev/disk/by-id/ata-RECOVERY_DISK", SerialNumber: "RECOVERY1"},
		{ID: "/dev/disk/by-id/nvme-OTHER", SerialNumber: "OTHER1"},
	}}

	require.NoError(t, RefuseProtected(context.Background(), sys, "/dev/disk/by-id/ata-RECOVERY_DISK"), "nothing protected yet")

	sys.protected = []string{"RECOVERY1"}
	require.ErrorContains(t, RefuseProtected(context.Background(), sys, "/dev/disk/by-id/ata-RECOVERY_DISK"), `drive "RECOVERY1" is protected`)
	require.NoError(t, RefuseProtected(context.Background(), sys, "/dev/disk/by-id/nvme-OTHER"))
}

func TestCopySpace(t *testing.T) {
	t.Parallel()

	const gib = int64(1 << 30)

	cases := []struct {
		name   string
		spool  int64
		target int64
		shared bool
		ok     bool
	}{
		{"shared pool with room once only", 3 * gib / 2, 3 * gib / 2, true, false},
		{"shared pool with room twice", 4 * gib, 4 * gib, true, true},
		{"separate pools with room", 2 * gib, 2 * gib, false, true},
		{"target pool short", 2 * gib, gib / 2, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := buildManager(&fakeRunner{}, &fakeSystem{spool: tc.spool, target: tc.target, shared: tc.shared}, &fakeIncus{}, t.TempDir())

			err := m.checkSpace(context.Background(), "local", gib/2)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "not enough room")
			}
		})
	}

	// A running copy holds its room until it ends.
	m := buildManager(&fakeRunner{}, &fakeSystem{spool: 4 * gib, target: 4 * gib, shared: true}, &fakeIncus{}, t.TempDir())
	m.copies = append(m.copies, &copyJob{state: "running", total: 2 * gib})
	require.ErrorContains(t, m.checkSpace(context.Background(), "local", gib/2), "not enough room")
}

func TestHardLinksKeyedByDevice(t *testing.T) {
	t.Parallel()

	tb := newTarball(nil)

	_, linked := tb.linkTarget(1, 256, "backup/volume/a")
	require.False(t, linked)

	_, linked = tb.linkTarget(2, 256, "backup/volume/b")
	require.False(t, linked, "the same inode number in another subvolume is another file")

	first, linked := tb.linkTarget(1, 256, "backup/volume/c")
	require.True(t, linked)
	require.Equal(t, "backup/volume/a", first)
}
