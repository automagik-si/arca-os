package update

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/internal/providers"
	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/tui"
	"github.com/lxc/incus-os/incus-osd/internal/util"
)

func TestIsUnreachableNetwork(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"dial", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, true},
		{"no route", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ENETUNREACH}, true},
		{"DNS", &net.DNSError{Err: "no such host", Name: "updates.example", IsNotFound: true}, true},
		{"timeout", &url.Error{Op: "Get", URL: "https://updates.example", Err: context.DeadlineExceeded}, true},
		{"canceled", &url.Error{Op: "Get", Err: context.Canceled}, false},
		{"HTTP", providers.ErrServerResponse, false},
		{"signature", &util.SignatureError{}, false},
		{"signature timeout", errors.Join(&util.SignatureError{}, context.DeadlineExceeded), false},
		{"trust", &util.TrustError{}, false},
		{"TLS certificate", &url.Error{Op: "Get", Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}, false},
		{"TLS protocol", &url.Error{Op: "Get", Err: tls.RecordHeaderError{}}, false},
		{"authentication", errors.New("authentication failed"), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.err
			if err != nil {
				err = fmt.Errorf("provider request: %w", err)
			}

			require.Equal(t, tt.want, isUnreachableNetwork(err))
		})
	}
}

type startupProvider struct {
	providers.Provider

	errs  []error
	calls int
}

func (p *startupProvider) GetSecureBootCertUpdate(context.Context) (providers.SecureBootCertUpdate, error) {
	err := p.errs[p.calls]
	p.calls++

	return nil, err
}

func TestInitialProviderWait(t *testing.T) {
	t.Parallel()

	networkErr := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ENETUNREACH}
	signatureErr := &util.SignatureError{}

	for _, tt := range []struct {
		name      string
		noPrimary bool
		errs      []error
		wantErr   error
		wantCalls int
		wantShows int
	}{
		{"network recovers", true, []error{networkErr, networkErr, providers.ErrNoUpdateAvailable}, nil, 3, 1},
		{"reachable", true, []error{providers.ErrNoUpdateAvailable}, nil, 1, 0},
		{"primary present", false, []error{networkErr}, networkErr, 1, 0},
		{"HTTP failure", true, []error{providers.ErrServerResponse}, providers.ErrServerResponse, 1, 0},
		{"signature failure", true, []error{signatureErr}, signatureErr, 1, 0},
		{"HTTP after network", true, []error{networkErr, providers.ErrServerResponse}, providers.ErrServerResponse, 2, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			shows, dismisses := 0, 0
			s := &state.State{}
			ctx := initialCheckContext(t.Context(), s, tt.noPrimary)
			wait, hasWait := ctx.Value(initialNetworkWaitKey{}).(*initialNetworkWait)
			require.Equal(t, tt.noPrimary, hasWait)

			if hasWait {
				wait.interval = time.Millisecond
				wait.notify = func(string) func() {
					shows++

					return func() { dismisses++ }
				}
			}

			p := &startupProvider{errs: tt.errs}
			start := time.Now()

			_, err := checkAndDownloadUpdate(ctx, s, nil, p, TypeSecureBoot, "", false)

			require.ErrorIs(t, err, tt.wantErr)
			require.Equal(t, tt.wantCalls, p.calls)
			require.Equal(t, tt.wantShows, shows)
			require.Equal(t, shows, dismisses)
			require.GreaterOrEqual(t, time.Since(start), time.Duration(p.calls-1)*time.Millisecond)
		})
	}
}

func TestInitialProviderWaitCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	dismissed := false
	wait := &initialNetworkWait{
		state:    &state.State{},
		interval: time.Hour,
		notify: func(string) func() {
			cancel()

			return func() { dismissed = true }
		},
	}

	err := wait.check(ctx, func() error { return &net.DNSError{Err: "offline"} })
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, dismissed)
}

func TestInitialNetworkDiagnostics(t *testing.T) { //nolint:paralleltest // Captures the process-wide logger.
	var logs bytes.Buffer

	previousLogger := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	networkErr := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ENETUNREACH}
	dnsErr := &net.DNSError{Err: "no such host", Name: "updates.example", IsNotFound: true}
	refusedErr := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	errs := []error{networkErr, networkErr, dnsErr, refusedErr, nil}
	s := &state.State{}
	s.System.Update.State.Status = "Checking for updates"
	calls, dismisses := 0, 0
	messages := []string{}

	wait := &initialNetworkWait{
		state:    s,
		interval: time.Millisecond,
		notify: func(message string) func() {
			cause := shortError(errs[calls-1])
			require.Equal(t, cause, s.System.Update.State.LastError)
			require.Equal(t, ErrorKindNetwork, s.System.Update.State.LastErrorKind)
			require.Contains(t, s.System.Update.State.Status, cause)
			require.False(t, s.System.Update.State.LastCheck.IsZero())

			messages = append(messages, message)

			return func() { dismisses++ }
		},
	}

	err := wait.check(t.Context(), func() error {
		err := errs[calls]
		calls++

		return err
	})

	require.NoError(t, err)
	require.Len(t, messages, 3)
	require.Equal(t, initialNetworkMessage, messages[0])
	require.Contains(t, messages[1], "host not found")
	require.Contains(t, messages[2], "refused the connection")
	require.Equal(t, len(messages), dismisses)
	require.Equal(t, "Checking for updates", s.System.Update.State.Status)
	require.Empty(t, s.System.Update.State.LastError)
	require.Empty(t, s.System.Update.State.LastErrorKind)
	require.Equal(t, 3, strings.Count(logs.String(), "level=WARN"))

	for _, cause := range []error{networkErr, dnsErr, refusedErr} {
		require.Contains(t, logs.String(), shortError(cause))
	}
}

func TestLoadInitialProviderWait(t *testing.T) {
	t.Parallel()

	s := &state.State{}
	ctx := initialCheckContext(t.Context(), s, true)
	wait, ok := ctx.Value(initialNetworkWaitKey{}).(*initialNetworkWait)
	require.True(t, ok)

	wait.interval = time.Millisecond
	shows := 0
	wait.notify = func(string) func() {
		shows++

		require.Equal(t, ErrorKindNetwork, s.System.Update.State.LastErrorKind)

		return func() {}
	}

	want := &startupProvider{}
	calls := 0
	p, err := loadInitialProvider(ctx, s, func(_ context.Context, gotState *state.State, ignoreSignedJSON bool) (providers.Provider, error) {
		require.Same(t, s, gotState)
		require.False(t, ignoreSignedJSON)

		calls++
		if calls < 3 {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ENETUNREACH}
		}

		return want, nil
	})

	require.NoError(t, err)
	require.Same(t, want, p)
	require.Equal(t, 3, calls)
	require.Equal(t, 1, shows)
	require.Empty(t, s.System.Update.State.LastError)
}

func TestInitialNetworkMessage(t *testing.T) {
	t.Parallel()

	screen := tcell.NewSimulationScreen("UTF-8")
	s := &state.State{}
	s.OS.Name = "Example OS"
	tt, _, stop := tui.NewArcaTestTUI(s, screen)
	t.Cleanup(stop)
	screen.SetSize(80, 25)
	tt.QueueArcaUpdate(func() {})

	dismiss := showInitialNetworkWait(tt, s.OS.Name, initialNetworkMessage)

	var rendered strings.Builder

	tt.QueueArcaUpdate(func() {
		for y := range 25 {
			for x := range 80 {
				str, _, _ := screen.Get(x, y)
				rendered.WriteString(str)
			}
		}
	})

	text := strings.NewReplacer("│", " ", "║", " ").Replace(rendered.String())
	text = strings.Join(strings.Fields(text), " ")
	require.Contains(t, text, initialNetworkMessage)
	require.NotContains(t, text, "Error")

	dismiss()
	require.Nil(t, tt.GetModal("initial-network"))
}
