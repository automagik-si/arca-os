package zfs

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/lxc/incus/v7/shared/subprocess"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/iopressure"
	"github.com/lxc/incus-os/incus-osd/internal/storage"
)

// zpoolFunc runs zpool with the given arguments.
type zpoolFunc func(ctx context.Context, args ...string) (string, error)

func runZpool(ctx context.Context, args ...string) (string, error) {
	return subprocess.RunCommandContext(ctx, "zpool", args...)
}

// poolActivity describes how to start, pause and resume one kind of pool activity.
type poolActivity struct {
	name   string
	start  func(ctx context.Context, zpool zpoolFunc, pool api.SystemStoragePool) error
	pause  []string
	resume []string
}

var scrubActivity = poolActivity{
	name:   "scrub",
	start:  startScrub,
	pause:  []string{"scrub", "-p"},
	resume: []string{"scrub"},
}

var trimActivity = poolActivity{
	name:   "trim",
	start:  startTrim,
	pause:  []string{"trim", "-s"},
	resume: []string{"trim"},
}

// activityRunner runs scrub and trim on every pool.
type activityRunner struct {
	zpool   zpoolFunc
	pools   func(ctx context.Context) ([]api.SystemStoragePool, error)
	pool    func(ctx context.Context, name string) (api.SystemStoragePool, error)
	readPSI func() ([]byte, error)
	ticker  func() (<-chan time.Time, func())
	record  func(pool string, activity string, pause storage.PoolActivityPause)
	logger  *slog.Logger

	psiOffLogged sync.Once
}

func newActivityRunner() *activityRunner {
	return &activityRunner{
		zpool: runZpool,
		pools: func(ctx context.Context) ([]api.SystemStoragePool, error) {
			_, pools, err := storage.GetStorageInfo(ctx)

			return pools, err
		},
		pool:    storage.GetZpoolMembers,
		readPSI: func() ([]byte, error) { return os.ReadFile(iopressure.Path) },
		ticker: func() (<-chan time.Time, func()) {
			t := time.NewTicker(iopressure.Tick)

			return t.C, t.Stop
		},
		record: storage.SetPoolActivityPause,
	}
}

var defaultActivityRunner = newActivityRunner()

func (r *activityRunner) log() *slog.Logger {
	if r.logger != nil {
		return r.logger
	}

	return slog.Default()
}

// runOnAllPools starts the zpool activity on every pool sequentially, waiting for each to complete.
// Pools on which the activity can't be started (already in progress, unsupported, ...) are skipped.
func (r *activityRunner) runOnAllPools(ctx context.Context, act poolActivity) error {
	pools, err := r.pools(ctx)
	if err != nil {
		return err
	}

	for _, pool := range pools {
		r.log().InfoContext(ctx, "Starting pool activity", slog.String("activity", act.name), slog.String("pool", pool.Name))

		err = act.start(ctx, r.zpool, pool)
		if errors.Is(err, storage.ErrTrimNotSupported) {
			r.log().InfoContext(ctx, "Skipping pool activity", slog.String("activity", act.name), slog.String("pool", pool.Name), slog.Any("reason", err))

			continue
		}

		if err != nil {
			r.log().WarnContext(ctx, "Unable to start pool activity", slog.String("activity", act.name), slog.String("pool", pool.Name), slog.Any("error", err))

			continue
		}

		// Wait for the activity to finish.
		_, err = r.zpool(ctx, "wait", "-t", act.name, pool.Name)
		if err != nil {
			return err
		}
	}

	return nil
}
