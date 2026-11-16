package update

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/tui"
)

const initialNetworkMessage = "No network: plug in an Ethernet cable or a USB Ethernet adapter. This screen continues by itself when the network is up."

type initialNetworkWaitKey struct{}

type initialNetworkWait struct {
	noPrimary bool
	interval  time.Duration
	notify    func() func()
}

// CheckInitial retries unreachable providers only during startup without a primary application.
func CheckInitial(ctx context.Context, s *state.State, noPrimary bool) error {
	if !noPrimary {
		return Check(ctx, s)
	}

	wait := &initialNetworkWait{
		noPrimary: noPrimary,
		interval:  5 * time.Second,
		notify: func() func() {
			slog.InfoContext(ctx, initialNetworkMessage)

			t, err := tui.GetTUI(nil)
			if err != nil {
				return func() {}
			}

			return showInitialNetworkWait(t, s.OS.Name)
		},
	}

	return Check(context.WithValue(ctx, initialNetworkWaitKey{}, wait), s)
}

func showInitialNetworkWait(t *tui.TUI, title string) func() {
	modal := t.AddModal(title, "initial-network")
	modal.Update(initialNetworkMessage)

	return modal.Done
}

func initialProviderCheck(ctx context.Context, check func() error) error {
	wait, ok := ctx.Value(initialNetworkWaitKey{}).(*initialNetworkWait)
	if !ok {
		return check()
	}

	return wait.check(ctx, check)
}

func (w *initialNetworkWait) check(ctx context.Context, check func() error) error {
	var dismiss func()

	defer func() {
		if dismiss != nil {
			dismiss()
		}
	}()

	for {
		err := check()
		if !w.noPrimary || !isUnreachableNetwork(err) || ctx.Err() != nil {
			return err
		}

		if dismiss == nil {
			dismiss = w.notify()
		}

		timer := time.NewTimer(w.interval)

		select {
		case <-ctx.Done():
			timer.Stop()

			return ctx.Err()
		case <-timer.C:
		}
	}
}

// isUnreachableNetwork excludes HTTP, authentication, TLS and metadata validation failures.
func isUnreachableNetwork(err error) bool {
	var tlsErr *tls.CertificateVerificationError
	if errorKind(err) != ErrorKindNetwork || errors.As(err, &tlsErr) || errors.Is(err, context.Canceled) {
		return false
	}

	_, isDNS := errors.AsType[*net.DNSError](err)
	if isDNS {
		return true
	}

	opErr, isOp := errors.AsType[*net.OpError](err)
	if isOp {
		return opErr.Op == "dial" || opErr.Timeout()
	}

	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded)
}
