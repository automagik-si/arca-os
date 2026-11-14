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

func (realSystem) FreeBytes() (int64, error) {
	for _, path := range []string{"/var/lib/incus/backups", "/var/lib/incus"} {
		var st unix.Statfs_t

		err := unix.Statfs(path, &st)
		if err == nil {
			return int64(st.Bavail) * st.Bsize, nil //nolint:gosec
		}
	}

	return 0, errors.New("cannot read the free space of Incus' storage")
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
