package update

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
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
			wait := &initialNetworkWait{
				noPrimary: tt.noPrimary,
				interval:  time.Millisecond,
				notify: func() func() {
					shows++

					return func() { dismisses++ }
				},
			}
			ctx := context.WithValue(t.Context(), initialNetworkWaitKey{}, wait)
			p := &startupProvider{errs: tt.errs}
			start := time.Now()

			_, err := checkAndDownloadUpdate(ctx, &state.State{}, nil, p, TypeSecureBoot, "", false)

			require.ErrorIs(t, err, tt.wantErr)
			require.Equal(t, tt.wantCalls, p.calls)
			require.Equal(t, tt.wantShows, shows)
			require.Equal(t, shows, dismisses)
			require.GreaterOrEqual(t, time.Since(start), time.Duration(p.calls-1)*wait.interval)
		})
	}
}

func TestInitialProviderWaitCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	dismissed := false
	wait := &initialNetworkWait{
		noPrimary: true,
		interval:  time.Hour,
		notify: func() func() {
			cancel()

			return func() { dismissed = true }
		},
	}

	err := wait.check(ctx, func() error { return &net.DNSError{Err: "offline"} })
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, dismissed)
}

func TestInitialNetworkMessage(t *testing.T) {
	t.Parallel()

	screen := tcell.NewSimulationScreen("UTF-8")
	s := &state.State{}
	s.OS.Name = "Example OS"
	tt, redraw, stop := tui.NewArcaTestTUI(s, screen)
	t.Cleanup(stop)
	screen.SetSize(80, 25)
	redraw()

	dismiss := showInitialNetworkWait(tt, s.OS.Name)

	var rendered strings.Builder

	tt.QueueArcaUpdate(func() {
		for y := range 25 {
			for x := range 80 {
				str, _, _ := screen.Get(x, y)
				rendered.WriteString(str)
			}
		}
	})

	text := strings.Join(strings.Fields(rendered.String()), " ")
	text = strings.ReplaceAll(text, "│", " ")
	text = strings.Join(strings.Fields(text), " ")
	require.Contains(t, text, initialNetworkMessage)
	require.NotContains(t, text, "Error")

	dismiss()
	require.Nil(t, tt.GetModal("initial-network"))
}
