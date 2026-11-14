package diskimport

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
)

const (
	testSerial = "S0TESTSERIAL"
	testPass   = "correct horse battery staple"
	testID     = "/dev/disk/by-id/nvme-TEST_S0TESTSERIAL"
	testDev    = "/dev/nvme9n1"
)

type call struct {
	stdin string
	cmd   string
}

type fakeRunner struct {
	calls   []call
	answers map[string]string
	fail    map[string]bool
}

func (f *fakeRunner) Run(_ context.Context, stdin string, name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call{stdin: stdin, cmd: cmd})

	for prefix := range f.fail {
		if strings.HasPrefix(cmd, prefix) {
			return "", errors.New("failed")
		}
	}

	for prefix, out := range f.answers {
		if strings.HasPrefix(cmd, prefix) {
			return out, nil
		}
	}

	return "", nil
}

func (f *fakeRunner) cmds() []string {
	ret := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		ret = append(ret, c.cmd)
	}

	return ret
}

type fakeSystem struct {
	drives    []api.SystemStorageDrive
	protected []string
	pools     []string
	wiped     []string
	created   []string
}

func (f *fakeSystem) Drives(context.Context) ([]api.SystemStorageDrive, error) {
	return f.drives, nil
}

func (f *fakeSystem) ProtectedSerials() []string { return f.protected }

func (f *fakeSystem) PoolExists(_ context.Context, name string) bool {
	return slices.Contains(f.pools, name)
}

func (f *fakeSystem) WipeDrive(_ context.Context, id string) error {
	f.wiped = append(f.wiped, id)

	return nil
}

func (f *fakeSystem) CreatePool(_ context.Context, name string, id string) error {
	f.created = append(f.created, name+"="+id)

	return nil
}

func (*fakeSystem) FreeBytes() (int64, error) { return 1 << 40, nil }

type fakeIncus struct {
	sources map[string]string
}

func (f *fakeIncus) DiskSources(context.Context) (map[string]string, error) {
	return f.sources, nil
}

func (*fakeIncus) ImportInstance(context.Context, string, string, string, io.Reader) error {
	return nil
}

func (*fakeIncus) ImportVolume(context.Context, string, string, string, io.Reader) error {
	return nil
}

// lsblkOne is a drive with one unused, unmounted partition.
const lsblkOne = `{"blockdevices":[{"name":"/dev/nvme9n1","type":"disk","fstype":null,"mountpoints":[null],
"children":[{"name":"/dev/nvme9n1p1","type":"part","fstype":"crypto_LUKS","mountpoints":[null]}]}]}`

func newTest(t *testing.T) (*Manager, *fakeRunner, *fakeSystem, *fakeIncus) {
	t.Helper()

	run := &fakeRunner{answers: map[string]string{
		"lsblk ": lsblkOne,
		"blkid -p -s TYPE -o value /dev/nvme9n1p1": "crypto_LUKS",
		"blkid -p -s TYPE -o value /dev/mapper/":   "LVM2_member",
		"blkid -p -s TYPE -o value /dev/vg0/root":  "btrfs",
		"pvs ": "  vg0\n",
		"vgs ": "  vg0\n",
		"lvs ": "  root /dev/vg0/root\n",
	}, fail: map[string]bool{}}

	sys := &fakeSystem{drives: []api.SystemStorageDrive{
		{ID: "/dev/disk/by-id/boot", SerialNumber: "BOOT1", Boot: true},
		{ID: testID, SerialNumber: testSerial},
	}}

	incus := &fakeIncus{sources: map[string]string{}}

	m := buildManager(run, sys, incus, t.TempDir())
	m.resolve = func(string) (string, error) { return testDev, nil }

	return m, run, sys, incus
}

func TestOpenRefusals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		serial string
		setup  func(*fakeRunner, *fakeSystem, *fakeIncus)
		want   string
	}{
		{"unknown serial", "NOPE", nil, "no drive with serial"},
		{"boot drive", "BOOT1", nil, "boot drive"},
		{"protected", testSerial, func(_ *fakeRunner, s *fakeSystem, _ *fakeIncus) { s.protected = []string{testSerial} }, "protected"},
		{"pool member", testSerial, func(_ *fakeRunner, s *fakeSystem, _ *fakeIncus) { s.drives[1].MemberPool = "local" }, "belongs to pool"},
		{"duplicate serial", testSerial, func(_ *fakeRunner, s *fakeSystem, _ *fakeIncus) {
			s.drives = append(s.drives, api.SystemStorageDrive{ID: "/dev/disk/by-id/other", SerialNumber: testSerial})
		}, "more than one drive"},
		{"given to a VM", testSerial, func(_ *fakeRunner, _ *fakeSystem, i *fakeIncus) {
			i.sources = map[string]string{testDev: "default/seats"}
		}, `in use by instance "default/seats"`},
		{"held by a mapping", testSerial, func(r *fakeRunner, _ *fakeSystem, _ *fakeIncus) {
			r.answers["lsblk "] = `{"blockdevices":[{"name":"/dev/nvme9n1","type":"disk","mountpoints":[null],
"children":[{"name":"/dev/nvme9n1p1","type":"part","mountpoints":[null],"children":[{"name":"/dev/mapper/x","type":"crypt"}]}]}]}`
		}, "held by a crypt device"},
		{"mounted", testSerial, func(r *fakeRunner, _ *fakeSystem, _ *fakeIncus) {
			r.answers["lsblk "] = `{"blockdevices":[{"name":"/dev/nvme9n1","type":"disk","mountpoints":[null],
"children":[{"name":"/dev/nvme9n1p1","type":"part","mountpoints":["/mnt"]}]}]}`
		}, "is mounted at /mnt"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m, run, sys, incus := newTest(t)
			if tc.setup != nil {
				tc.setup(run, sys, incus)
			}

			_, err := m.Open(context.Background(), tc.serial, testPass)
			require.ErrorContains(t, err, tc.want)

			for _, c := range run.cmds() {
				require.NotContains(t, c, "blockdev", "nothing may be touched before the checks pass")
			}
		})
	}
}

func TestOpenReadOnlyStack(t *testing.T) {
	t.Parallel()

	m, run, _, _ := newTest(t)

	disk, err := m.Open(context.Background(), testSerial, testPass)
	require.NoError(t, err)
	require.Equal(t, []string{"luks", "lvm", "btrfs"}, disk.Layers)
	require.Len(t, disk.Filesystems, 1)
	require.Equal(t, "vg0/root", disk.Filesystems[0].Name)

	cmds := run.cmds()
	require.Equal(t, "blockdev --setro /dev/nvme9n1", cmds[1])
	require.Equal(t, "blockdev --setro /dev/nvme9n1p1", cmds[2])

	for _, c := range run.calls {
		require.NotContains(t, c.cmd, testPass, "the passphrase never reaches a command line")

		if c.stdin != "" {
			require.True(t, strings.HasPrefix(c.cmd, "cryptsetup open --readonly "), c.cmd)
			require.Equal(t, testPass+"\n", c.stdin)
		}
	}

	require.Contains(t, strings.Join(cmds, "\n"), `vgchange -ay --config global{metadata_read_only=1} activation{read_only_volume_list=["vg0"]} vg0`)
	require.Contains(t, strings.Join(cmds, "\n"), "mount -t btrfs -o ro,rescue=nologreplay,subvolid=5 /dev/vg0/root")

	_, err = m.Open(context.Background(), testSerial, testPass)
	require.ErrorContains(t, err, "already open")

	run.calls = nil

	require.NoError(t, m.Close(context.Background(), testSerial))

	cmds = run.cmds()
	require.True(t, strings.HasPrefix(cmds[0], "umount "))
	require.True(t, strings.HasPrefix(cmds[1], "vgchange -an "))
	require.True(t, strings.HasPrefix(cmds[2], "cryptsetup close "))
	require.Equal(t, "blockdev --setrw /dev/nvme9n1", cmds[len(cmds)-1])
	require.Empty(t, m.State().Disks)
}

func TestOpenPassphrase(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		pass string
		fail bool
		want error
		msg  string
	}{
		{name: "missing", want: ErrPassphraseNeeded},
		{name: "wrong", pass: "nope", fail: true, msg: "the passphrase did not open nvme9n1p1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m, run, _, _ := newTest(t)
			if tc.fail {
				run.fail["cryptsetup open"] = true
			}

			_, err := m.Open(context.Background(), testSerial, tc.pass)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			} else {
				require.EqualError(t, err, tc.msg)
			}

			cmds := run.cmds()
			require.Equal(t, "blockdev --setrw /dev/nvme9n1", cmds[len(cmds)-1], "a failed open leaves the drive as it was")
			require.Empty(t, m.State().Disks)
		})
	}
}

func TestConvert(t *testing.T) {
	t.Parallel()

	m, run, sys, _ := newTest(t)
	ctx := context.Background()

	require.ErrorContains(t, m.Convert(ctx, testSerial, "samsung", "S0WRONG"), "does not match")
	require.ErrorContains(t, m.Convert(ctx, testSerial, "Bad Name", testSerial), "invalid pool name")

	sys.pools = []string{"local"}

	require.ErrorContains(t, m.Convert(ctx, testSerial, "local", testSerial), "already exists")

	_, err := m.Open(ctx, testSerial, testPass)
	require.NoError(t, err)
	require.ErrorContains(t, m.Convert(ctx, testSerial, "data", testSerial), "close it first")
	require.NoError(t, m.Close(ctx, testSerial))

	sys.protected = []string{testSerial}
	require.ErrorContains(t, m.Convert(ctx, testSerial, "data", testSerial), "protected")
	require.Empty(t, sys.wiped)

	sys.protected = nil
	run.calls = nil

	require.NoError(t, m.Convert(ctx, testSerial, "data", testSerial))
	require.Equal(t, []string{testID}, sys.wiped)
	require.Equal(t, []string{"data=" + testID}, sys.created)
}
