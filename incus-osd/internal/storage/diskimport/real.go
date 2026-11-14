package diskimport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	incusclient "github.com/lxc/incus/v7/client"
	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/subprocess"
	"golang.org/x/sys/unix"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/storage"
	"github.com/lxc/incus-os/incus-osd/internal/zfs"
)

const incusSocket = "/var/lib/incus/unix.socket"

// NewSystem returns the System of the running OS.
func NewSystem(s *state.State) System {
	return realSystem{s: s}
}

// NewIncus returns the Incus of the running OS.
func NewIncus() Incus {
	return realIncus{}
}

type realSystem struct {
	s *state.State
}

func (realSystem) Drives(ctx context.Context) ([]api.SystemStorageDrive, error) {
	info, _, err := storage.GetStorageInfo(ctx)
	if err != nil {
		return nil, err
	}

	return info.Drives, nil
}

func (r realSystem) ProtectedSerials() []string {
	return r.s.System.Storage.Config.ProtectedSerials
}

func (realSystem) PoolExists(ctx context.Context, name string) bool {
	return storage.PoolExists(ctx, name)
}

func (realSystem) WipeDrive(ctx context.Context, id string) error {
	return storage.WipeDrive(ctx, id, false)
}

func (r realSystem) CreatePool(ctx context.Context, name string, id string) error {
	err := zfs.CreateZpool(ctx, api.SystemStoragePool{Name: name, Type: "zfs-raid0", Devices: []string{id}}, r.s)
	if err != nil {
		return err
	}

	err = zfs.CreateDataset(ctx, name, "incus", map[string]string{"incusos:use": "incus"})
	if err != nil {
		return err
	}

	c, err := incusclient.ConnectIncusUnixWithContext(ctx, incusSocket, nil)
	if err != nil {
		return fmt.Errorf("pool %q is ready but Incus could not be reached: %w", name, err)
	}

	return c.CreateStoragePool(incusapi.StoragePoolsPost{
		Name:   name,
		Driver: "zfs",
		StoragePoolPut: incusapi.StoragePoolPut{
			Config:      map[string]string{"source": name + "/incus"},
			Description: "Local storage pool",
		},
	})
}

func (realSystem) Space(ctx context.Context, pool string) (int64, int64, bool, error) {
	const spoolPath = "/var/lib/incus/backups"

	var st unix.Statfs_t

	err := unix.Statfs(spoolPath, &st)
	if err != nil {
		return 0, 0, false, fmt.Errorf("cannot read the free space of Incus' spool: %w", err)
	}

	spool := int64(st.Bavail) * st.Bsize //nolint:gosec

	c, err := incusclient.ConnectIncusUnixWithContext(ctx, incusSocket, nil)
	if err != nil {
		return 0, 0, false, err
	}

	info, _, err := c.GetStoragePool(pool)
	if err != nil {
		return 0, 0, false, fmt.Errorf("pool %q: %w", pool, err)
	}

	res, err := c.GetStoragePoolResources(pool)
	if err != nil {
		return 0, 0, false, fmt.Errorf("pool %q: %w", pool, err)
	}

	target := int64(res.Space.Total) - int64(res.Space.Used) //nolint:gosec

	// The spool is a zfs dataset on IncusOS; the same zpool as the target means both copies land there.
	out, err := subprocess.RunCommandContext(ctx, "findmnt", "-n", "-o", "FSTYPE,SOURCE", "--target", spoolPath)
	if err != nil {
		return 0, 0, false, err
	}

	fields := strings.Fields(out)
	shared := len(fields) == 2 && fields[0] == "zfs" && info.Driver == "zfs" &&
		zpoolOf(fields[1]) == zpoolOf(info.Config["source"])

	return spool, target, shared, nil
}

// zpoolOf returns the zpool of a dataset name.
func zpoolOf(dataset string) string {
	pool, _, _ := strings.Cut(dataset, "/")

	return pool
}

type realIncus struct{}

func (realIncus) DiskSources(ctx context.Context) (map[string]string, error) {
	ret := map[string]string{}

	_, err := os.Stat(incusSocket)
	if errors.Is(err, os.ErrNotExist) {
		return ret, nil
	}

	c, err := incusclient.ConnectIncusUnixWithContext(ctx, incusSocket, nil)
	if err != nil {
		return nil, err
	}

	instances, err := c.GetInstancesAllProjects(incusapi.InstanceTypeAny)
	if err != nil {
		return nil, err
	}

	for _, inst := range instances {
		for _, dev := range inst.ExpandedDevices {
			if dev["type"] != "disk" && dev["type"] != "unix-block" {
				continue
			}

			for _, key := range []string{"source", "path"} {
				src := dev[key]
				if strings.HasPrefix(src, "/dev/") {
					ret[filepath.Clean(src)] = inst.Project + "/" + inst.Name
				}
			}
		}
	}

	return ret, nil
}

func (realIncus) ImportInstance(ctx context.Context, project string, pool string, name string, r io.Reader) error {
	c, err := incusclient.ConnectIncusUnixWithContext(ctx, incusSocket, nil)
	if err != nil {
		return err
	}

	op, err := c.UseProject(project).CreateInstanceFromBackup(incusclient.InstanceBackupArgs{BackupFile: r, PoolName: pool, Name: name})
	if err != nil {
		return err
	}

	return op.WaitContext(ctx)
}

func (realIncus) ImportVolume(ctx context.Context, project string, pool string, name string, r io.Reader) error {
	c, err := incusclient.ConnectIncusUnixWithContext(ctx, incusSocket, nil)
	if err != nil {
		return err
	}

	op, err := c.UseProject(project).CreateStoragePoolVolumeFromBackup(pool, incusclient.StorageVolumeBackupArgs{BackupFile: r, Name: name})
	if err != nil {
		return err
	}

	return op.WaitContext(ctx)
}

// RefuseProtected refuses a drive id whose serial is protected.
func RefuseProtected(ctx context.Context, sys System, id string) error {
	protected := sys.ProtectedSerials()
	if len(protected) == 0 {
		return nil
	}

	drives, err := sys.Drives(ctx)
	if err != nil {
		return err
	}

	target, err := filepath.EvalSymlinks(id)
	if err != nil {
		target = id
	}

	for _, d := range drives {
		dTarget, err := filepath.EvalSymlinks(d.ID)
		if err != nil {
			dTarget = d.ID
		}

		if d.ID != id && dTarget != target {
			continue
		}

		for _, serial := range protected {
			if d.SerialNumber == serial {
				return fmt.Errorf("drive %q is protected", serial)
			}
		}
	}

	return nil
}
