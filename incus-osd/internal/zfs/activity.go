package zfs

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
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

// resumeTimeout bounds the resume of a paused activity once its job has stopped.
const resumeTimeout = 30 * time.Second

// activityState is what became of a paused activity.
type activityState int

const (
	activityGone activityState = iota
	activityPaused
	activityRunning
)

// poolActivity describes how to start, pause and resume one kind of pool activity.
type poolActivity struct {
	name   string
	start  func(ctx context.Context, zpool zpoolFunc, pool api.SystemStoragePool) error
	pause  []string
	resume func(ctx context.Context, r *activityRunner, pool string) []string
	state  func(pool api.SystemStoragePool) activityState
}

var scrubActivity = poolActivity{
	name:  "scrub",
	start: startScrub,
	pause: []string{"scrub", "-p"},
	resume: func(_ context.Context, _ *activityRunner, pool string) []string {
		return []string{"scrub", pool}
	},
	state: func(pool api.SystemStoragePool) activityState {
		// zpool reports a paused scrub as scanning.
		if pool.LastScrub != nil && pool.LastScrub.State == api.SystemStoragePoolScrubInProgress {
			return activityPaused
		}

		return activityGone
	},
}

var trimActivity = poolActivity{
	name:  "trim",
	start: startTrim,
	pause: []string{"trim", "-s"},
	resume: func(_ context.Context, _ *activityRunner, pool string) []string {
		return []string{"trim", pool}
	},
	state: func(pool api.SystemStoragePool) activityState {
		if pool.LastTrim == nil {
			return activityGone
		}

		if pool.LastTrim.State == api.SystemStoragePoolTrimSuspended {
			return activityPaused
		}

		if pool.LastTrim.State == api.SystemStoragePoolTrimInProgress {
			return activityRunning
		}

		return activityGone
	},
}

// activityRunner runs scrub and trim on every pool, pausing them while IO pressure stays high.
type activityRunner struct {
	zpool   zpoolFunc
	pools   func(ctx context.Context) ([]api.SystemStoragePool, error)
	pool    func(ctx context.Context, name string) (api.SystemStoragePool, error)
	readPSI func() ([]byte, error)
	// trimDevices returns the leaves whose trim is suspended, and whether no other leaf was trimmed.
	trimDevices func(ctx context.Context, pool string) ([]string, bool, error)
	ticker      func() (<-chan time.Time, func())
	record      func(pool string, activity string, pause storage.PoolActivityPause)
	logger      *slog.Logger

	psiOffLogged sync.Once
}

func newActivityRunner() *activityRunner {
	return &activityRunner{
		zpool: runZpool,
		pools: func(ctx context.Context) ([]api.SystemStoragePool, error) {
			_, pools, err := storage.GetStorageInfo(ctx)

			return pools, err
		},
		pool:        storage.GetZpoolMembers,
		readPSI:     func() ([]byte, error) { return os.ReadFile(iopressure.Path) },
		trimDevices: storage.SuspendedTrimDevices,
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

	watch := r.pressureAvailable(ctx)

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

		if watch {
			err = r.waitWithPressure(ctx, act, pool.Name)
		} else {
			_, err = r.zpool(ctx, "wait", "-t", act.name, pool.Name)
		}

		if err != nil {
			return err
		}
	}

	return nil
}

// pressureAvailable reports whether IO pressure can be read, logging once per runner when it can't.
func (r *activityRunner) pressureAvailable(ctx context.Context) bool {
	_, err := r.sample()
	if err == nil {
		return true
	}

	r.psiOffLogged.Do(func() {
		r.log().WarnContext(ctx, "IO pressure isn't available, scrub and trim pausing is off", slog.Any("error", err))
	})

	return false
}

func (r *activityRunner) sample() (iopressure.Sample, error) {
	data, err := r.readPSI()
	if err != nil {
		return iopressure.Sample{}, err
	}

	return iopressure.Parse(string(data))
}

// waitWithPressure waits for the activity to finish, pausing it while IO pressure stays high.
// A pause isn't a completion: the wait carries on until the resumed activity finishes.
func (r *activityRunner) waitWithPressure(ctx context.Context, act poolActivity, poolName string) error {
	ticks, stopTicks := r.ticker()
	defer stopTicks()

	var (
		watcher iopressure.Watcher
		pause   storage.PoolActivityPause
	)

	r.record(poolName, act.name, pause)

	waitDone, stopWait := r.startWait(ctx, act, poolName)

	defer func() { stopWait() }()

	for {
		select {
		case <-ctx.Done():
			if pause.Paused {
				r.resumeAfterStop(ctx, act, poolName)

				pause.Paused, pause.Since, pause.Reason = false, time.Time{}, ""
				r.record(poolName, act.name, pause)
			}

			return ctx.Err()

		case err := <-waitDone:
			return err

		case now := <-ticks:
			sample, err := r.sample()
			if err != nil {
				r.log().WarnContext(ctx, "Unable to read IO pressure", slog.String("activity", act.name), slog.String("pool", poolName), slog.Any("error", err))

				continue
			}

			decision, reason := watcher.Observe(now, sample)

			switch decision { //nolint:exhaustive // the default is Hold
			case iopressure.Pause:
				_, err = r.zpool(ctx, append(slices.Clone(act.pause), poolName)...)
				if err != nil {
					r.log().WarnContext(ctx, "Unable to pause pool activity", slog.String("activity", act.name), slog.String("pool", poolName), slog.Any("error", err))
					watcher.Set(false)

					continue
				}

				// A paused activity ends zpool wait, so stop it rather than read that as completion.
				stopWait()

				waitDone = nil

				pause = storage.PoolActivityPause{Paused: true, Since: now, Reason: reason, Pauses: pause.Pauses + 1}
				r.record(poolName, act.name, pause)
				r.log().WarnContext(ctx, "Paused pool "+act.name+" because the disks are busy", slog.String("pool", poolName), slog.String("reason", reason), slog.Int("pauses", pause.Pauses))

			case iopressure.Resume:
				state, err := r.resume(ctx, act, poolName)
				if err != nil {
					r.log().WarnContext(ctx, "Unable to resume pool activity", slog.String("activity", act.name), slog.String("pool", poolName), slog.Any("error", err))
					watcher.Set(true)

					continue
				}

				pausedFor := now.Sub(pause.Since)
				pause.Paused, pause.Since, pause.Reason = false, time.Time{}, ""
				r.record(poolName, act.name, pause)

				if state == activityGone {
					r.log().InfoContext(ctx, "Pool "+act.name+" ended while paused, not starting it again", slog.String("pool", poolName))

					return nil
				}

				r.log().InfoContext(ctx, "Resumed pool "+act.name+" as the disks are quiet again", slog.String("pool", poolName), slog.String("reason", reason), slog.Duration("paused_for", pausedFor))
				waitDone, stopWait = r.startWait(ctx, act, poolName)

			default:
			}
		}
	}
}

// startWait runs zpool wait for the activity in the background.
func (r *activityRunner) startWait(ctx context.Context, act poolActivity, poolName string) (<-chan error, context.CancelFunc) {
	waitCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)

	go func() {
		_, err := r.zpool(waitCtx, "wait", "-t", act.name, poolName)
		done <- err
	}()

	return done, cancel
}

// resume resumes the paused activity unless it was cancelled, finished or resumed meanwhile.
func (r *activityRunner) resume(ctx context.Context, act poolActivity, poolName string) (activityState, error) {
	pool, err := r.pool(ctx, poolName)
	if err != nil {
		return activityGone, err
	}

	state := act.state(pool)
	if state != activityPaused {
		return state, nil
	}

	_, err = r.zpool(ctx, act.resume(ctx, r, poolName)...)
	if err != nil {
		return activityGone, err
	}

	return activityRunning, nil
}

// resumeAfterStop resumes the activity once its job stops, so it isn't left paused for good.
func (r *activityRunner) resumeAfterStop(ctx context.Context, act poolActivity, poolName string) {
	resumeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resumeTimeout)
	defer cancel()

	_, err := r.resume(resumeCtx, act, poolName)
	if err != nil {
		r.log().WarnContext(ctx, "Unable to resume pool activity after its job stopped", slog.String("activity", act.name), slog.String("pool", poolName), slog.Any("error", err))

		return
	}

	r.log().InfoContext(ctx, "Resumed pool "+act.name+" as its job stopped", slog.String("pool", poolName))
}

// resumePausedScrubs resumes every scrub found paused, as nothing else would.
func (*activityRunner) resumePausedScrubs(_ context.Context) error {
	return nil
}
