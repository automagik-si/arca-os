package zfs

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/storage"
)

const (
	psiHigh  = "some avg10=30.00 avg60=20.00 avg300=5.00 total=123\nfull avg10=23.40 avg60=12.00 avg300=3.00 total=99\n"
	psiQuiet = "some avg10=1.00 avg60=3.00 avg300=1.00 total=123\nfull avg10=0.50 avg60=0.20 avg300=0.10 total=99\n"
)

type record struct {
	pool     string
	activity string
	pause    storage.PoolActivityPause
}

// fakeHost stands in for zpool, the pool status, /proc/pressure/io and the ticker.
type fakeHost struct {
	mu      sync.Mutex
	calls   []string
	records []record
	psi     string
	psiErr  error
	pool    api.SystemStoragePool

	// trimLeaves and allSuspended answer trimDevices.
	trimLeaves   []string
	allSuspended bool

	// failPause makes a pause exit non-zero after taking effect, like a partial trim -s.
	failPause  bool
	failResume bool

	// paused is closed while the activity is paused, which ends zpool wait as in OpenZFS.
	paused chan struct{}

	ticks chan time.Time
	waits chan string
	// pauseEnded gets each zpool wait that the pause ended.
	pauseEnded chan string
	finish     chan struct{}
	tickers    int
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		psi:        psiQuiet,
		paused:     make(chan struct{}),
		ticks:      make(chan time.Time),
		waits:      make(chan string, 16),
		pauseEnded: make(chan string, 16),
		finish:     make(chan struct{}),
	}
}

// isPaused must be called with mu held.
func (f *fakeHost) isPaused() bool {
	select {
	case <-f.paused:
		return true
	default:
		return false
	}
}

// pausedBefore leaves the activity paused, as a prior run that stopped uncleanly would.
func (f *fakeHost) pausedBefore() {
	f.mu.Lock()
	defer f.mu.Unlock()

	close(f.paused)
}

// resumedElsewhere resumes the activity without going through the runner.
func (f *fakeHost) resumedElsewhere() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.paused = make(chan struct{})
}

func (f *fakeHost) zpool(ctx context.Context, args ...string) (string, error) {
	call := strings.Join(args, " ")

	f.mu.Lock()
	f.calls = append(f.calls, call)

	var err error

	switch {
	case args[0] == "wait":
	case slices.Contains(args, "-p") || slices.Contains(args, "-s"):
		if !f.isPaused() {
			close(f.paused)
		}

		if f.failPause {
			err = errors.New("cannot suspend: no such trim")
		}

	case f.isPaused() && f.failResume:
		err = errors.New("cannot resume")

	default:
		// A start or resume clears a pause.
		if f.isPaused() {
			f.paused = make(chan struct{})
		}
	}

	paused := f.paused
	f.mu.Unlock()

	if args[0] != "wait" {
		return "", err
	}

	f.waits <- call

	select {
	case <-f.finish:
		return "", nil
	case <-paused:
		f.pauseEnded <- call

		return "", nil
	case <-ctx.Done():
		// The runner stops the wait right after a pause, so either may come first.
		select {
		case <-paused:
			f.pauseEnded <- call
		default:
		}

		return "", ctx.Err()
	}
}

func (f *fakeHost) setPSI(psi string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.psi = psi
}

func (f *fakeHost) runner(pools ...api.SystemStoragePool) *activityRunner {
	return &activityRunner{
		zpool: f.zpool,
		pools: func(context.Context) ([]api.SystemStoragePool, error) {
			return pools, nil
		},
		pool: func(context.Context, string) (api.SystemStoragePool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()

			return f.pool, nil
		},
		readPSI: func() ([]byte, error) {
			f.mu.Lock()
			defer f.mu.Unlock()

			return []byte(f.psi), f.psiErr
		},
		trimDevices: func(context.Context, string) ([]string, bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()

			return f.trimLeaves, f.allSuspended, nil
		},
		ticker: func() (<-chan time.Time, func()) {
			f.mu.Lock()
			defer f.mu.Unlock()

			f.tickers++

			return f.ticks, func() {}
		},
		record: func(pool string, activity string, pause storage.PoolActivityPause) {
			f.mu.Lock()
			defer f.mu.Unlock()

			f.records = append(f.records, record{pool, activity, pause})
		},
	}
}

func (f *fakeHost) snapshot() ([]string, []record) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string{}, f.calls...), append([]record{}, f.records...)
}

// tick hands the runner one tick, failing if it isn't taken.
func (f *fakeHost) tick(t *testing.T, at time.Time) {
	t.Helper()

	select {
	case f.ticks <- at:
	case <-time.After(5 * time.Second):
		t.Fatalf("runner didn't take the tick at %s", at)
	}
}

// waitStarted waits for the runner to start a zpool wait.
func (f *fakeHost) waitStarted(t *testing.T) {
	t.Helper()

	select {
	case <-f.waits:
	case <-time.After(5 * time.Second):
		t.Fatal("runner didn't start waiting")
	}
}

// start runs the activity in the background and returns its result channel.
func start(ctx context.Context, r *activityRunner, act poolActivity) <-chan error {
	done := make(chan error, 1)

	go func() {
		done <- r.runOnAllPools(ctx, act)
	}()

	return done
}

func result(t *testing.T, done <-chan error) error {
	t.Helper()

	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("runner didn't return")
	}

	return nil
}

// pauseAndResume drives 60s of high pressure then 5 minutes of quiet, and returns the time it paused.
func pauseAndResume(t *testing.T, f *fakeHost, done <-chan error, base time.Time) time.Time {
	t.Helper()

	f.setPSI(psiHigh)

	// The runner reads PSI after taking a tick, so the eighth tick makes sure the seventh paused.
	for i := range 8 {
		f.tick(t, base.Add(time.Duration(i)*10*time.Second))
	}

	pausedAt := base.Add(60 * time.Second)

	// The pause ends zpool wait, as it does in OpenZFS, but the run isn't over.
	select {
	case <-f.pauseEnded:
	case <-time.After(5 * time.Second):
		t.Fatal("the pause didn't end zpool wait")
	}

	select {
	case err := <-done:
		t.Fatalf("the pause was taken as completion: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	f.setPSI(psiQuiet)

	quietStart := pausedAt.Add(20 * time.Second)
	for i := range 31 {
		f.tick(t, quietStart.Add(time.Duration(i)*10*time.Second))
	}

	return pausedAt
}

var (
	pausedScrub   = api.SystemStoragePool{Name: "tank", LastScrub: &api.SystemStoragePoolScrubStatus{State: api.SystemStoragePoolScrubInProgress, Paused: true}}
	runningScrub  = api.SystemStoragePool{Name: "tank", LastScrub: &api.SystemStoragePoolScrubStatus{State: api.SystemStoragePoolScrubInProgress}}
	suspendedTrim = api.SystemStoragePool{Name: "tank", LastTrim: &api.SystemStoragePoolTrimStatus{State: api.SystemStoragePoolTrimSuspended}}
)

func TestScrubPausesAndResumesUnderPressure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	f := newFakeHost()
	f.pool = pausedScrub

	done := start(ctx, f.runner(api.SystemStoragePool{Name: "tank"}), scrubActivity)
	f.waitStarted(t)

	pausedAt := pauseAndResume(t, f, done, time.Unix(1000, 0))

	f.waitStarted(t)
	close(f.finish)
	require.NoError(t, result(t, done))

	calls, records := f.snapshot()
	require.Equal(t, []string{
		"scrub tank",
		"wait -t scrub tank",
		"scrub -p tank",
		"scrub tank",
		"wait -t scrub tank",
	}, calls)

	require.Equal(t, []record{
		{"tank", "scrub", storage.PoolActivityPause{}},
		{"tank", "scrub", storage.PoolActivityPause{Paused: true, Since: pausedAt, Reason: "io pressure: full avg10 23.4", Pauses: 1}},
		{"tank", "scrub", storage.PoolActivityPause{Pauses: 1}},
	}, records)
}

func TestTrimSuspendsAndResumesUnderPressure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	f := newFakeHost()
	f.pool = suspendedTrim
	f.trimLeaves, f.allSuspended = []string{"d1", "d2"}, true

	done := start(ctx, f.runner(api.SystemStoragePool{Name: "tank"}), trimActivity)
	f.waitStarted(t)

	pauseAndResume(t, f, done, time.Unix(1000, 0))

	f.waitStarted(t)
	close(f.finish)
	require.NoError(t, result(t, done))

	calls, _ := f.snapshot()
	require.Equal(t, []string{
		"trim tank",
		"wait -t trim tank",
		"trim -s tank",
		"trim tank",
		"wait -t trim tank",
	}, calls)
}

func TestScrubEndedWhilePausedIsNotStartedAgain(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	f := newFakeHost()
	f.pool = api.SystemStoragePool{Name: "tank", LastScrub: &api.SystemStoragePoolScrubStatus{State: api.SystemStoragePoolScrubFinished}}

	done := start(ctx, f.runner(api.SystemStoragePool{Name: "tank"}), scrubActivity)
	f.waitStarted(t)

	pauseAndResume(t, f, done, time.Unix(1000, 0))
	require.NoError(t, result(t, done))

	calls, _ := f.snapshot()
	require.Equal(t, []string{"scrub tank", "wait -t scrub tank", "scrub -p tank"}, calls)
}

func TestNoPauseWhenActivityWasNotOurs(t *testing.T) {
	t.Parallel()

	f := newFakeHost()
	f.setPSI(psiHigh)

	err := f.runner(runningScrub).runOnAllPools(t.Context(), scrubActivity)
	require.NoError(t, err)

	calls, records := f.snapshot()
	require.Empty(t, calls)
	require.Empty(t, records)
}

func TestNoPauseWithoutPSI(t *testing.T) {
	t.Parallel()

	f := newFakeHost()
	f.psiErr = &fs.PathError{Op: "open", Path: "/proc/pressure/io", Err: fs.ErrNotExist}
	close(f.finish)

	var logs bytes.Buffer

	r := f.runner(api.SystemStoragePool{Name: "a"}, api.SystemStoragePool{Name: "b"})
	r.logger = slog.New(slog.NewTextHandler(&logs, nil))

	require.NoError(t, r.runOnAllPools(t.Context(), scrubActivity))
	require.NoError(t, r.runOnAllPools(t.Context(), scrubActivity))

	calls, records := f.snapshot()
	once := []string{"scrub a", "wait -t scrub a", "scrub b", "wait -t scrub b"}
	require.Equal(t, append(once, once...), calls)
	require.Empty(t, records)
	require.Zero(t, f.tickers)
	require.Equal(t, 1, strings.Count(logs.String(), "pausing is off"), logs.String())
}

func TestCancelDuringPauseReturns(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	f := newFakeHost()
	f.pool = pausedScrub
	f.setPSI(psiHigh)

	done := start(ctx, f.runner(api.SystemStoragePool{Name: "tank"}), scrubActivity)
	f.waitStarted(t)

	base := time.Unix(1000, 0)
	for i := range 8 {
		f.tick(t, base.Add(time.Duration(i)*10*time.Second))
	}

	cancel()
	require.ErrorIs(t, result(t, done), context.Canceled)

	// The scrub isn't left paused once the job stops, as it would never be scrubbed again.
	calls, records := f.snapshot()
	require.Equal(t, []string{"scrub tank", "wait -t scrub tank", "scrub -p tank", "scrub tank"}, calls)
	require.False(t, records[len(records)-1].pause.Paused)
}

func TestCancelKeepsPausedWhenResumeFails(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	f := newFakeHost()
	f.pool = pausedScrub
	f.failResume = true
	f.setPSI(psiHigh)

	done := start(ctx, f.runner(api.SystemStoragePool{Name: "tank"}), scrubActivity)
	f.waitStarted(t)

	base := time.Unix(1000, 0)
	for i := range 8 {
		f.tick(t, base.Add(time.Duration(i)*10*time.Second))
	}

	cancel()
	require.ErrorIs(t, result(t, done), context.Canceled)

	_, records := f.snapshot()
	require.True(t, records[len(records)-1].pause.Paused, "the scrub is still paused on disk")
}

func TestTrimPartlySuspendedIsPaused(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	// One leaf had finished, so trim -s fails for it after suspending the others.
	f := newFakeHost()
	f.pool = suspendedTrim
	f.failPause = true
	f.trimLeaves, f.allSuspended = []string{"d2"}, false

	done := start(ctx, f.runner(api.SystemStoragePool{Name: "tank"}), trimActivity)
	f.waitStarted(t)

	pauseAndResume(t, f, done, time.Unix(1000, 0))

	f.waitStarted(t)
	close(f.finish)
	require.NoError(t, result(t, done))

	// Only the suspended leaf resumes, so the finished one isn't trimmed again.
	calls, records := f.snapshot()
	require.Equal(t, []string{
		"trim tank",
		"wait -t trim tank",
		"trim -s tank",
		"trim tank d2",
		"wait -t trim tank",
	}, calls)
	require.True(t, records[1].pause.Paused)
}

func TestScheduledRunAdoptsPausedScrub(t *testing.T) {
	t.Parallel()

	// A prior run paused this scrub and stopped before resuming it.
	f := newFakeHost()
	f.pausedBefore()
	close(f.finish)

	err := f.runner(pausedScrub).runOnAllPools(t.Context(), scrubActivity)
	require.NoError(t, err)

	calls, records := f.snapshot()
	require.Equal(t, []string{"scrub tank", "wait -t scrub tank"}, calls)
	require.Equal(t, []record{{"tank", "scrub", storage.PoolActivityPause{}}}, records, "watched as this run's own")
	require.Equal(t, 1, f.tickers)
}

func TestScrubResumedElsewhereIsNotResumedAgain(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	f := newFakeHost()
	f.pool = runningScrub

	done := start(ctx, f.runner(api.SystemStoragePool{Name: "tank"}), scrubActivity)
	f.waitStarted(t)

	f.setPSI(psiHigh)

	base := time.Unix(1000, 0)
	for i := range 8 {
		f.tick(t, base.Add(time.Duration(i)*10*time.Second))
	}

	f.resumedElsewhere()
	f.setPSI(psiQuiet)

	for i := range 31 {
		f.tick(t, base.Add(time.Duration(8+i)*10*time.Second))
	}

	f.waitStarted(t)
	close(f.finish)
	require.NoError(t, result(t, done))

	calls, _ := f.snapshot()
	require.Equal(t, []string{"scrub tank", "wait -t scrub tank", "scrub -p tank", "wait -t scrub tank"}, calls)
}

func TestStartScrubResumesAPausedScrub(t *testing.T) {
	t.Parallel()

	// The manual API scrub goes through startScrub too, so a paused scrub no longer gets a conflict.
	f := newFakeHost()

	require.NoError(t, startScrub(t.Context(), f.zpool, pausedScrub))
	require.ErrorIs(t, startScrub(t.Context(), f.zpool, runningScrub), storage.ErrScrubAlreadyInProgress)

	calls, _ := f.snapshot()
	require.Equal(t, []string{"scrub tank"}, calls)
}

func TestResumePausedScrubsAtStartup(t *testing.T) {
	t.Parallel()

	f := newFakeHost()

	paused := pausedScrub
	paused.Name = "a"
	running := runningScrub
	running.Name = "b"
	finished := api.SystemStoragePool{Name: "c", LastScrub: &api.SystemStoragePoolScrubStatus{State: api.SystemStoragePoolScrubFinished}}
	never := api.SystemStoragePool{Name: "d"}

	require.NoError(t, f.runner(paused, running, finished, never).resumePausedScrubs(t.Context()))

	calls, _ := f.snapshot()
	require.Equal(t, []string{"scrub a"}, calls)
}
