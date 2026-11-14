package diskimport

import (
	"archive/tar"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/lxc/incus-os/incus-osd/api"
)

const webYAML = `container:
  name: web
  project: default
  type: container
  profiles: [default, lan]
  config:
    boot.autostart: "true"
    volatile.last_state.idmap: '[{"Isuid":true,"Isgid":false,"Hostid":UID,"Nsid":0,"Maprange":65536},{"Isuid":false,"Isgid":true,"Hostid":GID,"Nsid":0,"Maprange":65536}]'
snapshots:
- name: snap0
pool:
  name: old
`

const vmYAML = `container:
  name: vm1
  project: lab
  type: virtual-machine
  config: {}
`

func writeFile(t *testing.T, path string, content string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// oldHost lays out an old host's Incus pool inside a btrfs top level.
func oldHost(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	pool := filepath.Join(root, "@", "var", "lib", "incus", "storage-pools", "old")

	// The test user's files stand for the old host's shifted root.
	web := strings.ReplaceAll(strings.ReplaceAll(webYAML, "UID", strconv.Itoa(os.Getuid())), "GID", strconv.Itoa(os.Getgid()))
	writeFile(t, filepath.Join(pool, "containers", "web", "backup.yaml"), web)
	writeFile(t, filepath.Join(pool, "containers", "web", "rootfs", "etc", "hostname"), "web\n")
	writeFile(t, filepath.Join(pool, "containers", "web", "metadata.yaml"), "architecture: x86_64\n")
	writeFile(t, filepath.Join(pool, "virtual-machines", "lab_vm1", "backup.yaml"), vmYAML)
	writeFile(t, filepath.Join(pool, "virtual-machines", "lab_vm1", "root.img"), "disk-image")
	writeFile(t, filepath.Join(pool, "custom", "default_data", "notes.txt"), "notes")
	writeFile(t, filepath.Join(root, "@home", "owner", "photo.jpg"), "jpeg")

	return root
}

func TestScanTree(t *testing.T) {
	t.Parallel()

	got := scanTree(oldHost(t))

	require.Equal(t, []string{"@", "@home"}, got.Folders)
	require.Equal(t, []api.SystemStorageImportInstance{
		{Name: "web", Project: "default", Type: "container", Source: "@/var/lib/incus/storage-pools/old/containers/web", Autostart: true, Snapshots: []string{"snap0"}},
		{Name: "vm1", Project: "lab", Type: "virtual-machine", Source: "@/var/lib/incus/storage-pools/old/virtual-machines/lab_vm1"},
	}, got.Instances)
	require.Equal(t, []api.SystemStorageImportVolume{
		{Name: "data", Project: "default", Source: "@/var/lib/incus/storage-pools/old/custom/default_data"},
	}, got.Volumes)
}

func TestScanDatasets(t *testing.T) {
	t.Parallel()

	got := scanDatasets([]string{"tank", "tank/incus", "tank/incus/containers/web", "tank/incus/virtual-machines/lab_vm1", "tank/incus/virtual-machines/lab_vm1.block", "tank/incus/custom/default_data"})

	require.Equal(t, []api.SystemStorageImportInstance{
		{Name: "web", Project: "default", Type: "container", Source: "tank/incus/containers/web"},
		{Name: "vm1", Project: "lab", Type: "virtual-machine", Source: "tank/incus/virtual-machines/lab_vm1"},
	}, got.Instances)
	require.Equal(t, "data", got.Volumes[0].Name)
}

type captureIncus struct {
	fakeIncus

	entries map[string][]byte
	owners  map[string][2]int
	done    chan string
}

func readTar(c *captureIncus, r io.Reader) error {
	tr := tar.NewReader(r)

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return err
		}

		content, err := io.ReadAll(tr)
		if err != nil {
			return err
		}

		c.entries[hdr.Name] = content
		c.owners[hdr.Name] = [2]int{hdr.Uid, hdr.Gid}
	}
}

func (c *captureIncus) ImportInstance(_ context.Context, project string, pool string, name string, r io.Reader) error {
	err := readTar(c, r)
	c.done <- project + "/" + name + "@" + pool

	return err
}

func (c *captureIncus) ImportVolume(_ context.Context, project string, pool string, name string, r io.Reader) error {
	err := readTar(c, r)
	c.done <- project + "/" + name + "@" + pool

	return err
}

func copyOne(t *testing.T, req api.SystemStorageImportCopyRequest) (*captureIncus, string) {
	t.Helper()

	root := oldHost(t)
	incus := &captureIncus{entries: map[string][]byte{}, owners: map[string][2]int{}, done: make(chan string, 1)}
	m := buildManager(&fakeRunner{}, &fakeSystem{}, incus, t.TempDir())
	m.disks[testSerial] = &openDisk{serial: testSerial, fss: []*filesystem{{name: "vg0/root", fsType: "btrfs", mount: root}}}

	req.Serial = testSerial
	req.Filesystem = "vg0/root"
	req.TargetPool = "local"

	_, err := m.Copy(context.Background(), req)
	require.NoError(t, err)

	target := <-incus.done

	for range 100 {
		if m.State().Copies[0].State != "running" {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	return incus, target
}

func TestCopyContainer(t *testing.T) {
	t.Parallel()

	incus, target := copyOne(t, api.SystemStorageImportCopyRequest{Kind: "instance", Source: "@/var/lib/incus/storage-pools/old/containers/web"})
	require.Equal(t, "default/web@local", target)

	index := map[string]any{}
	require.NoError(t, yaml.Unmarshal(incus.entries["backup/index.yaml"], &index))
	require.Equal(t, "container", index["type"])
	require.Equal(t, "local", index["pool"])
	require.Equal(t, false, index["optimized"])

	config := mapAt(mapAt(mapAt(index, "config"), "container"), "config")
	require.Equal(t, "false", config["boot.autostart"])
	require.Equal(t, "true", config["user.import.autostart"])
	require.Equal(t, "[]", config["volatile.last_state.idmap"])
	require.NotContains(t, mapAt(index, "config"), "snapshots")

	require.Equal(t, "web\n", string(incus.entries["backup/container/rootfs/etc/hostname"]))
	require.Equal(t, [2]int{0, 0}, incus.owners["backup/container/rootfs/etc/hostname"])
	require.Equal(t, [2]int{os.Getuid(), os.Getgid()}, incus.owners["backup/container/metadata.yaml"])
	require.Contains(t, string(incus.entries["backup/container/backup.yaml"]), "boot.autostart: \"false\"")
}

func TestCopyVMAndFolder(t *testing.T) {
	t.Parallel()

	incus, target := copyOne(t, api.SystemStorageImportCopyRequest{Kind: "instance", Source: "@/var/lib/incus/storage-pools/old/virtual-machines/lab_vm1", TargetName: "vm2"})
	require.Equal(t, "lab/vm2@local", target)
	require.Equal(t, "disk-image", string(incus.entries["backup/virtual-machine.img"]))
	require.NotContains(t, incus.entries, "backup/virtual-machine/root.img")
	require.Contains(t, incus.entries, "backup/virtual-machine/backup.yaml")

	incus, target = copyOne(t, api.SystemStorageImportCopyRequest{Kind: "folder", Source: "@home/owner"})
	require.Equal(t, "default/owner@local", target)
	require.Equal(t, "jpeg", string(incus.entries["backup/volume/photo.jpg"]))
	require.Contains(t, string(incus.entries["backup/index.yaml"]), "type: custom")
}

func TestCopyRefusesOutsideSource(t *testing.T) {
	t.Parallel()

	m := buildManager(&fakeRunner{}, &fakeSystem{}, &fakeIncus{}, t.TempDir())
	m.disks[testSerial] = &openDisk{serial: testSerial, fss: []*filesystem{{name: "p1", fsType: "ext4", mount: oldHost(t)}}}

	_, err := m.Copy(context.Background(), api.SystemStorageImportCopyRequest{Serial: testSerial, Filesystem: "p1", Kind: "folder", Source: "../../etc", TargetPool: "local"})
	require.Error(t, err)
	require.Zero(t, m.disks[testSerial].busy)
}

func TestUnshift(t *testing.T) {
	t.Parallel()

	set, err := parseIDMap(`[{"Isuid":true,"Isgid":false,"Hostid":1000000,"Nsid":0,"Maprange":65536},{"Isuid":false,"Isgid":true,"Hostid":1000000,"Nsid":0,"Maprange":65536}]`)
	require.NoError(t, err)

	uid, gid := set.unshift(1001000, 1000033)
	require.Equal(t, []int64{1000, 33}, []int64{uid, gid})

	uid, _ = set.unshift(5, 1000000)
	require.Equal(t, int64(-1), uid)

	acl := make([]byte, 4+8)
	binary.LittleEndian.PutUint32(acl, 2)
	binary.LittleEndian.PutUint16(acl[4:], 0x02)
	binary.LittleEndian.PutUint32(acl[8:], 1001000)
	require.Equal(t, uint32(1000), binary.LittleEndian.Uint32(unshiftACL(acl, set)[8:]))
}

// Two trees from different file systems may hold the same inode number for different files.
func TestAddTreeKeysLinksByDevice(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a"), "same inode")
	require.NoError(t, os.Link(filepath.Join(dir, "a"), filepath.Join(dir, "b")))

	var buf strings.Builder

	tb := newTarball(&buf)
	tb.devOf = func(name string, _ uint64) uint64 {
		if strings.HasPrefix(name, "backup/one") {
			return 1
		}

		return 2
	}

	require.NoError(t, tb.addTree(tree{dir: dir, prefix: "backup/one"}))
	require.NoError(t, tb.addTree(tree{dir: dir, prefix: "backup/two"}))
	require.NoError(t, tb.close())

	types := map[string]byte{}
	tr := tar.NewReader(strings.NewReader(buf.String()))

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		require.NoError(t, err)

		types[hdr.Name] = hdr.Typeflag
	}

	require.Equal(t, byte(tar.TypeReg), types["backup/two/a"], "the first file of the second tree is not a link into the first")
	require.Equal(t, byte(tar.TypeLink), types["backup/two/b"])
}
