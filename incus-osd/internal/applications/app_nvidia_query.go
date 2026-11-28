package applications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// nvidiaCheckedFile records that the driver passed its health check in this boot.
const nvidiaCheckedFile = "/run/incus-os/nvidia-checked"

// nvidiaReaderWait is how long a driver switch waits for a reader's nvidia-smi to end.
var nvidiaReaderWait = 10 * time.Second

// ErrNVIDIANotReady is returned when nvidia-smi must not run for a reader. The
// errors below wrap it with the one reason a reader can show.
var (
	ErrNVIDIANotReady  = errors.New("the NVIDIA driver is not ready for a query")
	ErrNVIDIAStarting  = fmt.Errorf("%w: it is starting", ErrNVIDIANotReady)
	ErrNVIDIASwitching = fmt.Errorf("%w: it is being switched", ErrNVIDIANotReady)
	ErrNVIDIAInactive  = fmt.Errorf("%w: it is not the active driver", ErrNVIDIANotReady)
	ErrNVIDIAUnchecked = fmt.Errorf("%w: it has not passed this boot's check", ErrNVIDIANotReady)
)

// nvidiaGate keeps a reader's nvidia-smi apart from the driver's startup and from
// a driver switch. It has one holder at a time, and every write of the active
// driver by startup or a switch and every reader's read of it happen while holding it.
type nvidiaGate struct {
	slot chan struct{}
	// why is what a holder that is not a reader tells a refused reader.
	why atomic.Pointer[error]
}

func newNVIDIAGate() *nvidiaGate {
	return &nvidiaGate{slot: make(chan struct{}, 1)}
}

// nvidiaReaders is the gate of the running system.
var nvidiaReaders = newNVIDIAGate()

// gate returns the gate this application shares with its readers.
func (n *nvidia) gate() *nvidiaGate {
	if n.readers != nil {
		return n.readers
	}

	return nvidiaReaders
}

// enter takes the gate for a reader when it is free, and otherwise returns its holder's reason.
func (g *nvidiaGate) enter() error {
	select {
	case g.slot <- struct{}{}:
		return nil
	default:
	}

	why := g.why.Load()
	if why == nil {
		return ErrNVIDIANotReady
	}

	return *why
}

// hold takes the gate for the driver's own work, waiting at most wait for its holder to leave.
func (g *nvidiaGate) hold(wait time.Duration, why error) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case g.slot <- struct{}{}:
	case <-timer.C:
		return false
	}

	if why != nil {
		g.why.Store(&why)
	}

	return true
}

// heldFor reports whether the gate's holder keeps readers out for the reason why.
func (g *nvidiaGate) heldFor(why error) bool {
	held := g.why.Load()

	return held != nil && errors.Is(*held, why)
}

func (g *nvidiaGate) leave() {
	g.why.Store(nil)
	<-g.slot
}

// NVIDIAQuery runs nvidia-smi for a reader outside the application, such as the console.
// It refuses until the driver passed its check in this boot, while the driver starts or
// is switched and while another query runs, so a reader can never load, hold or probe an
// unchecked driver. A refusal wraps ErrNVIDIANotReady.
func NVIDIAQuery(ctx context.Context, s *state.State, args ...string) (string, error) {
	n := &nvidia{sysroot: "/"}
	n.state = s

	return n.query(ctx, n.gate(), args...)
}

func (n *nvidia) query(ctx context.Context, gate *nvidiaGate, args ...string) (string, error) {
	err := gate.enter()
	if err != nil {
		return "", err
	}

	defer gate.leave()

	// Startup and a switch write the active driver while holding the gate, as this read does.
	if n.state.Applications.NVIDIA.State.ActiveDriver != "nvidia" {
		return "", ErrNVIDIAInactive
	}

	_, err = os.Lstat(n.path(nvidiaCheckedFile))
	if err != nil {
		return "", ErrNVIDIAUnchecked
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
