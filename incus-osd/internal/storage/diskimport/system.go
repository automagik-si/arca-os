package diskimport

import (
	"bytes"
	"context"
	"io"
	"strings"

	"github.com/lxc/incus/v7/shared/subprocess"

	"github.com/lxc/incus-os/incus-osd/api"
)

// Runner runs a system command; stdin is passed on the command's standard input only.
type Runner interface {
	Run(ctx context.Context, stdin string, name string, args ...string) (string, error)
}

// System is what the import needs from the rest of the OS.
type System interface {
	// Drives lists the system's drives.
	Drives(ctx context.Context) ([]api.SystemStorageDrive, error)

	// ProtectedSerials lists the serials no import or convert may touch.
	ProtectedSerials() []string

	// PoolExists tells if a native pool of that name exists.
	PoolExists(ctx context.Context, name string) bool

	// WipeDrive erases a drive given by id.
	WipeDrive(ctx context.Context, id string) error

	// CreatePool creates an encrypted single-drive native pool and hands it to Incus.
	CreatePool(ctx context.Context, name string, id string) error

	// Space returns the free bytes of Incus' import spool and of the target pool, and whether they share one zpool.
	Space(ctx context.Context, pool string) (int64, int64, bool, error)
}

// Incus is what the import needs from Incus.
type Incus interface {
	// DiskSources maps each host path used as an instance disk or block device to its instance.
	DiskSources(ctx context.Context) (map[string]string, error)

	// ImportInstance creates an instance from a backup tarball.
	ImportInstance(ctx context.Context, project string, pool string, name string, r io.Reader) error

	// ImportVolume creates a custom volume from a backup tarball.
	ImportVolume(ctx context.Context, project string, pool string, name string, r io.Reader) error
}

type execRunner struct{}

// Run runs the command with stdin and returns its standard output.
func (execRunner) Run(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	var out bytes.Buffer

	var in io.Reader
	if stdin != "" {
		in = strings.NewReader(stdin)
	}

	err := subprocess.RunCommandWithFds(ctx, in, &out, name, args...)

	return out.String(), err
}
