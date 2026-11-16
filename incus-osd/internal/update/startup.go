package update

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/lxc/incus-os/incus-osd/internal/providers"
	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/tui"
)

const initialNetworkMessage = "No network: connect Wi-Fi or plug in an Ethernet cable or USB Ethernet adapter. This screen continues by itself when the network is up."

type initialNetworkWaitKey struct{}

type initialNetworkWait struct {
	state    *state.State
	interval time.Duration
	notify   func(string) func()
}

// CheckInitial retries unreachable providers only during startup without a primary application.
func CheckInitial(ctx context.Context, s *state.State, noPrimary bool) error {
	return Check(initialCheckContext(ctx, s, noPrimary), s)
}

func initialCheckContext(ctx context.Context, s *state.State, noPrimary bool) context.Context {
	if !noPrimary {
		return ctx
	}

	wait := &initialNetworkWait{
		state:    s,
		interval: 5 * time.Second,
		notify: func(message string) func() {
			t, err := tui.GetTUI(nil)
			if err != nil {
				return func() {}
			}

			return showInitialNetworkWait(t, s.OS.Name, message)
		},
	}

	return context.WithValue(ctx, initialNetworkWaitKey{}, wait)
}

func initialNetworkNotice(err error) string {
	dnsErr, isDNS := errors.AsType[*net.DNSError](err)
	if isDNS && dnsErr.IsNotFound {
		return "Update server host not found: check the server address and DNS settings. This screen retries automatically."
	}

	if errors.Is(err, syscall.ECONNREFUSED) {
		return "Update server refused the connection: check the server address and service availability. This screen retries automatically."
	}

	return initialNetworkMessage
}

func showInitialNetworkWait(t *tui.TUI, title string, message string) func() {
	modal := t.AddModal(title, "initial-network")
	modal.Update(message)

	return modal.Done
}

func initialProviderCheck(ctx context.Context, check func() error) error {
	wait, ok := ctx.Value(initialNetworkWaitKey{}).(*initialNetworkWait)
	if !ok {
		return check()
	}

	return wait.check(ctx, check)
}

func loadInitialProvider(ctx context.Context, s *state.State, load func(context.Context, *state.State, bool) (providers.Provider, error)) (providers.Provider, error) {
	var p providers.Provider

	err := initialProviderCheck(ctx, func() error {
		var err error

		p, err = load(ctx, s, false)

		return err
	})

	return p, err
}

func (w *initialNetworkWait) check(ctx context.Context, check func() error) (err error) {
	var dismiss func()

	priorStatus := w.state.System.Update.State.Status
	lastError := ""

	defer func() {
		if dismiss != nil {
			dismiss()

			result := err
			if errors.Is(result, providers.ErrNoUpdateAvailable) {
				result = nil
			}

			recordCheck(ctx, w.state, result)

			w.state.System.Update.State.Status = priorStatus
			if result != nil {
				w.state.System.Update.State.Status = failedStatus(result, "Failed to contact update server")
			}
		}
	}()

	for {
		err = check()
		if !isUnreachableNetwork(err) || ctx.Err() != nil {
			return err
		}

		cause := shortError(err)
		if dismiss == nil || cause != lastError {
			recordCheck(ctx, w.state, err)
			w.state.System.Update.State.LastCheck = time.Now()
			w.state.System.Update.State.Status = "Waiting for update server: " + cause
			message := initialNetworkNotice(err)
			slog.WarnContext(ctx, message, "error", cause)

			if dismiss != nil {
				dismiss()
			}

			dismiss = w.notify(message)
			lastError = cause
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
