package applications

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
)

// queryFixture is a checked, active driver with its own gate and a counted nvidia-smi.
func queryFixture(t *testing.T) (*nvidia, *nvidiaGate, *int) {
	t.Helper()

	n, _, _ := newNVIDIAFixture(t, nvidiaBootTest{})
	n.state.Applications.NVIDIA.State.ActiveDriver = "nvidia"
	n.markChecked(context.Background())

	calls := 0
	n.runCommand = func(_ context.Context, name string, _ ...string) (string, error) {
		require.Equal(t, "/usr/bin/nvidia-smi", name)

		calls++

		return "answer", nil
	}

	return n, n.gate(), &calls
}

func TestNVIDIAQueryNeedsACheckedActiveDriver(t *testing.T) {
	t.Parallel()

	n, gate, calls := queryFixture(t)

	out, err := n.query(context.Background(), gate, "--query-gpu=name")
	require.NoError(t, err)
	require.Equal(t, "answer", out)

	// A driver the state calls active from an earlier boot has not passed this boot's check.
	require.NoError(t, os.Remove(n.path(nvidiaCheckedFile)))

	_, err = n.query(context.Background(), gate)
	require.ErrorIs(t, err, ErrNVIDIAUnchecked)
	require.ErrorIs(t, err, ErrNVIDIANotReady)

	// A switch clears the active driver before it unloads the modules.
	n.markChecked(context.Background())

	for _, driver := range []string{"", "nouveau"} {
		n.state.Applications.NVIDIA.State.ActiveDriver = driver

		_, err = n.query(context.Background(), gate)
		require.ErrorIs(t, err, ErrNVIDIAInactive)
		require.ErrorIs(t, err, ErrNVIDIANotReady)
	}

	require.Equal(t, 1, *calls)
}

func TestNVIDIAQueryAndSwitchExcludeEachOther(t *testing.T) {
	t.Parallel()

	n, gate, calls := queryFixture(t)

	// While a switch holds the gate no query starts, and the reader is told why.
	require.True(t, gate.hold(time.Second, ErrNVIDIASwitching))

	_, err := n.query(context.Background(), gate)
	require.ErrorIs(t, err, ErrNVIDIASwitching)
	require.ErrorIs(t, err, ErrNVIDIANotReady)
	require.Zero(t, *calls)

	gate.leave()

	// While a query runs a second one is refused and a switch waits, bounded.
	started, release := make(chan struct{}), make(chan struct{})
	n.runCommand = func(context.Context, string, ...string) (string, error) {
		close(started)
		<-release

		return "", nil
	}

	done := make(chan error, 1)

	go func() {
		_, err := n.query(context.Background(), gate)
		done <- err
	}()

	<-started

	_, err = n.query(context.Background(), gate)
	require.ErrorIs(t, err, ErrNVIDIANotReady)
	require.NotErrorIs(t, err, ErrNVIDIASwitching)
	require.False(t, gate.hold(20*time.Millisecond, ErrNVIDIASwitching))

	close(release)
	require.NoError(t, <-done)
	require.True(t, gate.hold(time.Second, ErrNVIDIASwitching))
	gate.leave()

	// A gate that was left lets the next reader in.
	n.runCommand = func(context.Context, string, ...string) (string, error) { return "", nil }

	_, err = n.query(context.Background(), gate)
	require.NoError(t, err)
}

// TestNVIDIAStartKeepsReadersOut checks that startup decides the active driver with readers held off.
func TestNVIDIAStartKeepsReadersOut(t *testing.T) {
	t.Parallel()

	n, _, _ := newNVIDIAFixture(t, nvidiaBootTest{})
	require.NoError(t, n.prepare(t.Context(), true))

	// The state still names the driver of the boot before this one.
	n.state.Applications.NVIDIA.State.ActiveDriver = "nvidia"

	_, err := n.query(t.Context(), n.gate())
	require.ErrorIs(t, err, ErrNVIDIAUnchecked)

	original := n.runCommand
	during := []error{}
	n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "/usr/bin/nvidia-smi" && len(during) == 0 {
			// The health check is running: a reader is refused and told the driver is starting.
			_, err := n.query(ctx, n.gate())
			during = append(during, err)
		}

		return original(ctx, name, args...)
	}

	// A reader that keeps asking while startup runs: under -race this is the pair that must not race.
	stop, asked := make(chan struct{}), make(chan struct{})

	go func() {
		defer close(asked)

		reader := &nvidia{common: common{state: n.state}, sysroot: n.sysroot, readers: n.gate()}
		reader.runCommand = func(context.Context, string, ...string) (string, error) { return "", nil }

		for {
			select {
			case <-stop:
				return
			default:
				_, _ = reader.query(context.Background(), reader.gate())
			}
		}
	}()

	require.NoError(t, n.Start(t.Context()))
	close(stop)
	<-asked
	require.Len(t, during, 1)
	require.ErrorIs(t, during[0], ErrNVIDIAStarting)

	// Startup is over and the check passed: the same reader is answered.
	_, err = n.query(t.Context(), n.gate())
	require.NoError(t, err)
}

func TestNVIDIAHealthMarksTheDriverChecked(t *testing.T) {
	t.Parallel()

	n, _, _ := newNVIDIAFixture(t, nvidiaBootTest{})

	_, err := os.Stat(n.path(nvidiaCheckedFile))
	require.ErrorIs(t, err, os.ErrNotExist)

	n.runCommand = func(context.Context, string, ...string) (string, error) {
		return "", os.ErrDeadlineExceeded
	}

	require.Error(t, n.health(context.Background(), []string{"0000:01:00.0"}))

	_, err = os.Stat(n.path(nvidiaCheckedFile))
	require.ErrorIs(t, err, os.ErrNotExist)

	n.runCommand = func(context.Context, string, ...string) (string, error) {
		return "00000000:01:00.0\n", nil
	}

	require.NoError(t, n.health(context.Background(), []string{"0000:01:00.0"}))

	_, err = os.Stat(n.path(nvidiaCheckedFile))
	require.NoError(t, err)
}

func TestNVIDIASwitchWaitsForAQuery(t *testing.T) { //nolint:paralleltest // shortens the package's wait for a reader
	n, commands, _ := switchFixture(t, nvidiaStable)
	n.SetVersions("002", []string{"001", "002"})
	n.state.Applications.NVIDIA.State.RequestedBranch = nvidiaLTS

	oldWait := nvidiaReaderWait

	t.Cleanup(func() { nvidiaReaderWait = oldWait })

	nvidiaReaderWait = 20 * time.Millisecond

	require.True(t, n.gate().hold(time.Second, nil))

	before := len(*commands)

	require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}))
	n.gate().leave()

	s := n.state.Applications.NVIDIA.State
	require.Equal(t, "NVIDIA devices are in use; the driver switch will run at the next boot", s.Reason)
	require.True(t, s.NeedsReboot)
	require.Equal(t, nvidiaStable, s.ActiveBranch)
	require.Len(t, *commands, before, "a switch ran commands while a query held the driver")

	// While startup holds the gate the switch waits for the next boot too, and says why.
	require.True(t, n.gate().hold(time.Second, ErrNVIDIAStarting))
	require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}))
	n.gate().leave()
	require.Equal(t, "the NVIDIA application is still starting; the driver switch will run at the next boot", n.state.Applications.NVIDIA.State.Reason)
	require.Len(t, *commands, before, "a switch ran commands while startup held the driver")

	// With the gate free the same switch runs live.
	require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}))
	require.Equal(t, nvidiaLTS, n.state.Applications.NVIDIA.State.ActiveBranch)
}
