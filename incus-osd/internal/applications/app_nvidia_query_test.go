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
func queryFixture(t *testing.T) (*nvidia, nvidiaGate, *int) {
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

	return n, make(nvidiaGate, 1), &calls
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
	require.ErrorIs(t, err, ErrNVIDIANotReady)

	// A switch clears the active driver before it unloads the modules.
	n.markChecked(context.Background())

	for _, driver := range []string{"", "nouveau"} {
		n.state.Applications.NVIDIA.State.ActiveDriver = driver

		_, err = n.query(context.Background(), gate)
		require.ErrorIs(t, err, ErrNVIDIANotReady)
	}

	require.Equal(t, 1, *calls)
}

func TestNVIDIAQueryAndSwitchExcludeEachOther(t *testing.T) {
	t.Parallel()

	n, gate, calls := queryFixture(t)

	// While a switch holds the gate no query starts.
	require.True(t, gate.hold(time.Second))

	_, err := n.query(context.Background(), gate)
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
	require.False(t, gate.hold(20*time.Millisecond))

	close(release)
	require.NoError(t, <-done)
	require.True(t, gate.hold(time.Second))
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

func TestNVIDIASwitchWaitsForAQuery(t *testing.T) { //nolint:paralleltest // holds the package gate and shortens its wait
	n, commands, _ := switchFixture(t, nvidiaStable)
	n.SetVersions("002", []string{"001", "002"})
	n.state.Applications.NVIDIA.State.RequestedBranch = nvidiaLTS

	oldWait := nvidiaReaderWait

	t.Cleanup(func() { nvidiaReaderWait = oldWait })

	nvidiaReaderWait = 20 * time.Millisecond

	require.True(t, nvidiaReaders.hold(time.Second))

	before := len(*commands)

	require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}))
	nvidiaReaders.leave()

	s := n.state.Applications.NVIDIA.State
	require.Equal(t, "NVIDIA devices are in use; the driver switch will run at the next boot", s.Reason)
	require.True(t, s.NeedsReboot)
	require.Equal(t, nvidiaStable, s.ActiveBranch)
	require.Len(t, *commands, before, "a switch ran commands while a query held the driver")

	// With the gate free the same switch runs live.
	require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}))
	require.Equal(t, nvidiaLTS, n.state.Applications.NVIDIA.State.ActiveBranch)
}
