package zfs

import (
	"bytes"
	"context"
	"io/fs"
	"log/slog"
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

	ticks   chan time.Time
	waits   chan string
	finish  chan struct{}
	tickers int
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		psi:    psiQuiet,
		ticks:  make(chan time.Time),
		waits:  make(chan string, 16),
		finish: make(chan struct{}),
	}
}

func (f *fakeHost) zpool(ctx context.Context, args ...string) (string, error) {
	call := strings.Join(args, " ")

	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()

	if args[0] != "wait" {
		return "", nil
	}

	f.waits <- call

	select {
	case <-f.finish:
		return "", nil
	case <-ctx.Done():
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
func pauseAndResume(t *testing.T, f *fakeHost, base time.Time) time.Time {
	t.Helper()

	f.setPSI(psiHigh)

	// The runner reads PSI after taking a tick, so the eighth tick makes sure the seventh paused.
	for i := range 8 {
		f.tick(t, base.Add(time.Duration(i)*10*time.Second))
	}

	pausedAt := base.Add(60 * time.Second)

	f.setPSI(psiQuiet)

	quietStart := pausedAt.Add(20 * time.Second)
	for i := range 31 {
		f.tick(t, quietStart.Add(time.Duration(i)*10*time.Second))
	}

	return pausedAt
}

func TestScrubPausesAndResumesUnderPressure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	f := newFakeHost()
	f.pool = api.SystemStoragePool{Name: "tank", LastScrub: &api.SystemStoragePoolScrubStatus{State: api.SystemStoragePoolScrubInProgress}}

	done := start(ctx, f.runner(api.SystemStoragePool{Name: "tank"}), scrubActivity)
	f.waitStarted(t)

	pausedAt := pauseAndResume(t, f, time.Unix(1000, 0))

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
	f.pool = api.SystemStoragePool{Name: "tank", LastTrim: &api.SystemStoragePoolTrimStatus{State: api.SystemStoragePoolTrimSuspended}}

	done := start(ctx, f.runner(api.SystemStoragePool{Name: "tank"}), trimActivity)
	f.waitStarted(t)

	pauseAndResume(t, f, time.Unix(1000, 0))

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

	pauseAndResume(t, f, time.Unix(1000, 0))
	require.NoError(t, result(t, done))

	calls, _ := f.snapshot()
	require.Equal(t, []string{"scrub tank", "wait -t scrub tank", "scrub -p tank"}, calls)
}

func TestNoPauseWhenActivityWasNotOurs(t *testing.T) {
	t.Parallel()

	f := newFakeHost()
	f.setPSI(psiHigh)

	busy := api.SystemStoragePool{Name: "tank", LastScrub: &api.SystemStoragePoolScrubStatus{State: api.SystemStoragePoolScrubInProgress}}

	err := f.runner(busy).runOnAllPools(t.Context(), scrubActivity)
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
	f.pool = api.SystemStoragePool{Name: "tank", LastScrub: &api.SystemStoragePoolScrubStatus{State: api.SystemStoragePoolScrubInProgress}}
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
