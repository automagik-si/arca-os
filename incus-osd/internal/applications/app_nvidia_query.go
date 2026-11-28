package applications

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// nvidiaCheckedFile records that the driver passed its health check in this boot.
const nvidiaCheckedFile = "/run/incus-os/nvidia-checked"

// nvidiaReaderWait is how long a driver switch waits for a reader's nvidia-smi to end.
var nvidiaReaderWait = 10 * time.Second

// ErrNVIDIANotReady is returned when nvidia-smi must not run for a reader.
var ErrNVIDIANotReady = errors.New("the NVIDIA driver is not ready for a query")

// nvidiaGate keeps a reader's nvidia-smi and a driver switch apart: it has one holder at a time.
type nvidiaGate chan struct{}

// nvidiaReaders is the gate of the running system.
var nvidiaReaders = make(nvidiaGate, 1)

// enter takes the gate when it is free.
func (g nvidiaGate) enter() bool {
	select {
	case g <- struct{}{}:
		return true
	default:
		return false
	}
}

// hold takes the gate, waiting at most wait for its holder to leave.
func (g nvidiaGate) hold(wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case g <- struct{}{}:
		return true
	case <-timer.C:
		return false
	}
}

func (g nvidiaGate) leave() {
	<-g
}

// NVIDIAQuery runs nvidia-smi for a reader outside the application, such as the console.
// It refuses until the driver passed its check in this boot, during a driver switch and
// while another query runs, so a reader can never load, hold or probe an unchecked driver.
func NVIDIAQuery(ctx context.Context, s *state.State, args ...string) (string, error) {
	n := &nvidia{sysroot: "/"}
	n.state = s

	return n.query(ctx, nvidiaReaders, args...)
}

func (n *nvidia) query(ctx context.Context, gate nvidiaGate, args ...string) (string, error) {
	if !gate.enter() {
		return "", ErrNVIDIANotReady
	}

	defer gate.leave()

	if n.state.Applications.NVIDIA.State.ActiveDriver != "nvidia" {
		return "", ErrNVIDIANotReady
	}

	_, err := os.Lstat(n.path(nvidiaCheckedFile))
	if err != nil {
		return "", ErrNVIDIANotReady
	}

	return n.command(ctx, "/usr/bin/nvidia-smi", args...)
}

// markChecked records this boot's passed health check; without it readers only see dashes.
func (n *nvidia) markChecked(ctx context.Context) {
	path := n.path(nvidiaCheckedFile)

	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		err = os.WriteFile(path, nil, 0o600)
	}

	if err != nil {
		slog.WarnContext(ctx, "NVIDIA check could not be recorded; the console shows no GPU figures", "error", err)
	}
}
