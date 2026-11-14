package update

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/subprocess"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/internal/providers"
	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/util"
	"github.com/lxc/incus-os/incus-osd/internal/util/smimetest"
)

// updateServer is a stub update server whose signed index can be changed between checks.
type updateServer struct {
	mu    sync.Mutex
	index []byte
	url   string
}

func (u *updateServer) serve(index []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.index = index
}

// trustProvider asks the stub server for its signed index and verifies it like the images
// provider does, under a test root and update CA.
type trustProvider struct {
	providers.Provider

	server   *updateServer
	rootCA   *x509.Certificate
	updateCA *x509.Certificate
}

func (*trustProvider) Type() string { return "images" }

func (p *trustProvider) GetOSUpdate(ctx context.Context) (providers.OSUpdate, error) {
	return nil, p.fetch(ctx)
}

func (p *trustProvider) GetApplicationUpdate(ctx context.Context, _ string) (providers.ApplicationUpdate, error) {
	return nil, p.fetch(ctx)
}

func (p *trustProvider) fetch(ctx context.Context) error {
	p.server.mu.Lock()
	serverURL := p.server.url
	p.server.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverURL+"/index.sjson", nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	verified, err := util.VerifySMIMEWithRoot(ctx, p.rootCA, []*x509.Certificate{p.updateCA}, body)
	if err != nil {
		return err
	}

	index := map[string]any{}

	err = json.NewDecoder(bytes.NewReader(verified.Bytes())).Decode(&index)
	if err != nil {
		return err
	}

	return providers.ErrNoUpdateAvailable
}

type trustFixture struct {
	server   *updateServer
	provider *trustProvider
	trusted  []byte
	foreign  []byte
	garbled  []byte
	updateCA string
	newCA    string
}

func newTrustFixture(t *testing.T) *trustFixture {
	t.Helper()
	smimetest.RequireOpenSSL(t)

	root := smimetest.NewIdentity(t, "Update root", 1, nil, true)
	updateCA := smimetest.NewIdentity(t, "Update CA", 2, root, true)
	leaf := smimetest.NewIdentity(t, "Update signer", 3, updateCA, false)
	newRoot := smimetest.NewIdentity(t, "Newer root", 1, nil, true)
	newCA := smimetest.NewIdentity(t, "Newer CA", 2, newRoot, true)
	newLeaf := smimetest.NewIdentity(t, "Newer signer", 3, newCA, false)

	server := &updateServer{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		server.mu.Lock()
		defer server.mu.Unlock()

		_, _ = w.Write(server.index)
	}))
	t.Cleanup(srv.Close)

	server.url = srv.URL

	return &trustFixture{
		server:   server,
		provider: &trustProvider{server: server, rootCA: root.Cert, updateCA: updateCA.Cert},
		trusted:  smimetest.Sign(t, `{"updates":[]}`, leaf, updateCA),
		foreign:  smimetest.Sign(t, `{"updates":[]}`, newLeaf, newCA),
		garbled:  smimetest.Sign(t, `{"updates":`, leaf, updateCA),
		updateCA: util.CertificateFingerprint(updateCA.Cert),
		newCA:    util.CertificateFingerprint(newCA.Cert),
	}
}

// unreachable points the provider at a closed port.
func (f *trustFixture) unreachable(t *testing.T) {
	t.Helper()

	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, l.Close())

	f.server.mu.Lock()
	defer f.server.mu.Unlock()

	f.server.url = "http://" + l.Addr().String()
}

func TestCheckOSTrustFailure(t *testing.T) {
	t.Parallel()

	f := newTrustFixture(t)
	s := &state.State{}
	st := &s.System.Update.State
	check := func() error { return checkOSWith(withCheckCycle(t.Context()), s, f.provider, nil, false) }

	// A tree signed under a CA this system doesn't trust.
	f.server.serve(f.foreign)

	err := check()
	require.Error(t, err)
	require.Equal(t, TrustStatus, st.Status)
	require.Equal(t, ErrorKindTrust, st.LastErrorKind)
	require.Equal(t, "unable to verify S/MIME message due to its use of a missing or unverifiable CA", st.LastError)
	require.Equal(t, f.updateCA, st.EmbeddedCAFingerprint)
	require.Equal(t, f.newCA, st.PresentedCAFingerprint)
	require.Equal(t, 1, st.TrustFailures)
	require.False(t, st.TrustEscalated)

	// The console's summary leads with the same phrase.
	sum := Summarize(s, time.Now())
	require.Equal(t, TrustStatus, sum.Status)
	require.Equal(t, f.updateCA, sum.EmbeddedCA)
	require.Equal(t, f.newCA, sum.PresentedCA)

	// Network and parse failures keep the count and the fingerprints.
	f.server.serve(f.garbled)

	require.Error(t, check())
	require.Equal(t, ErrorKindParse, st.LastErrorKind)
	require.Equal(t, "Failed to check for OS updates", st.Status)
	require.Equal(t, 1, st.TrustFailures)
	require.Equal(t, f.newCA, st.PresentedCAFingerprint)

	f.server.serve(f.foreign)
	require.Error(t, check())
	require.Equal(t, 2, st.TrustFailures)

	server := f.server.url

	f.unreachable(t)
	require.Error(t, check())
	require.Equal(t, ErrorKindNetwork, st.LastErrorKind)
	require.Equal(t, 2, st.TrustFailures)

	f.server.mu.Lock()
	f.server.url = server
	f.server.mu.Unlock()

	// The third trust failure warns the owner, and stays warned.
	require.Error(t, check())
	require.Equal(t, 3, st.TrustFailures)
	require.True(t, st.TrustEscalated)

	require.Error(t, check())
	require.Equal(t, 4, st.TrustFailures)
	require.True(t, st.TrustEscalated)

	// A successful check returns to normal.
	f.server.serve(f.trusted)

	require.NoError(t, check())
	require.Equal(t, "OS update check completed", st.Status)
	require.Empty(t, st.LastErrorKind)
	require.Empty(t, st.LastError)
	require.Empty(t, st.EmbeddedCAFingerprint)
	require.Empty(t, st.PresentedCAFingerprint)
	require.Zero(t, st.TrustFailures)
	require.False(t, st.TrustEscalated)
}

func TestTrustFailureCountsOncePerRun(t *testing.T) {
	t.Parallel()

	f := newTrustFixture(t)
	f.server.serve(f.foreign)

	s := &state.State{}
	ctx := withCheckCycle(t.Context())

	// Application and OS checks in one run ask the server several times.
	for _, app := range []string{"incus", "debug"} {
		_, err := checkAndDownloadUpdate(ctx, s, nil, f.provider, TypeApplication, app, false)
		require.Equal(t, ErrorKindTrust, errorKind(err))
	}

	require.Error(t, checkOSWith(ctx, s, f.provider, nil, false))
	require.Equal(t, TrustStatus, s.System.Update.State.Status)
	require.Equal(t, 1, s.System.Update.State.TrustFailures)
	require.Equal(t, f.newCA, s.System.Update.State.PresentedCAFingerprint)
}

func TestTrustEscalatesOnce(t *testing.T) {
	t.Parallel()

	f := newTrustFixture(t)
	f.server.serve(f.foreign)

	trustErr := f.provider.fetch(t.Context())
	require.Equal(t, ErrorKindTrust, errorKind(trustErr))

	s := &state.State{}

	escalated := []bool{}
	for range 5 {
		escalated = append(escalated, recordCheck(withCheckCycle(t.Context()), s, trustErr))
	}

	require.Equal(t, []bool{false, false, true, false, false}, escalated)

	// A new episode after a successful check warns again.
	require.False(t, recordCheck(t.Context(), s, nil))

	escalated = escalated[:0]
	for range 3 {
		escalated = append(escalated, recordCheck(withCheckCycle(t.Context()), s, trustErr))
	}

	require.Equal(t, []bool{false, false, true}, escalated)
}

func TestErrorKind(t *testing.T) {
	t.Parallel()

	f := newTrustFixture(t)

	f.server.serve(f.foreign)
	require.Equal(t, ErrorKindTrust, errorKind(f.provider.fetch(t.Context())))

	tampered := bytes.Replace(f.trusted, []byte(`{"updates":[]}`), []byte(`{"updates":[1]}`), 1)
	f.server.serve(tampered)
	require.Equal(t, ErrorKindSignature, errorKind(f.provider.fetch(t.Context())))

	f.server.serve(f.garbled)
	require.Equal(t, ErrorKindParse, errorKind(f.provider.fetch(t.Context())))

	f.server.serve([]byte("not S/MIME\n"))
	require.Equal(t, ErrorKindParse, errorKind(f.provider.fetch(t.Context())))

	f.unreachable(t)
	require.Equal(t, ErrorKindNetwork, errorKind(f.provider.fetch(t.Context())))

	require.Equal(t, ErrorKindNetwork, errorKind(providers.ErrServerResponse))
	require.Equal(t, ErrorKindOther, errorKind(errors.New("not enough disk space")))
	require.Empty(t, errorKind(nil))
}

func TestModalErrorLeadsWithTrust(t *testing.T) {
	t.Parallel()

	f := newTrustFixture(t)
	f.server.serve(f.foreign)

	err := f.provider.fetch(t.Context())
	text := modalErrorText(TrustStatus, err, "images")

	require.True(t, strings.HasPrefix(text, "[red]"+TrustStatus+"[white]"), text)
	require.Contains(t, text, f.updateCA)
	require.Contains(t, text, f.newCA)
	require.NotContains(t, text, "local issuer")

	// Any other failure keeps its text.
	require.Equal(t, "[red]Error[white] Failed to check for OS updates: boom (provider: images)",
		modalErrorText("Failed to check for OS updates", errors.New("boom"), "images"))
}

func TestLastErrorIsShortAndClean(t *testing.T) {
	t.Parallel()

	// A network failure on a provider that authenticates with a query parameter.
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, l.Close())

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+l.Addr().String()+"/os/index.sjson?token=SECRET-TOKEN", nil)
	require.NoError(t, err)

	resp, netErr := http.DefaultClient.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}

	require.Error(t, netErr)
	require.Contains(t, netErr.Error(), "SECRET-TOKEN")

	s := &state.State{}
	recordCheck(t.Context(), s, fmt.Errorf("http request timed out after five seconds: %w", netErr))
	require.Equal(t, ErrorKindNetwork, s.System.Update.State.LastErrorKind)
	require.NotContains(t, s.System.Update.State.LastError, "SECRET-TOKEN")
	require.NotContains(t, s.System.Update.State.LastError, "token=")
	require.Contains(t, s.System.Update.State.LastError, "/os/index.sjson")

	// A command that failed keeps neither its command line nor its output.
	runErr := subprocess.NewRunError("openssl", []string{"smime", "-verify", "-CAfile", "/tmp/123456"}, errors.New("exit status 4"), nil, bytes.NewBufferString("Verification failure\nline two"))
	recordCheck(t.Context(), s, runErr)
	require.NotContains(t, s.System.Update.State.LastError, "/tmp/123456")
	require.NotContains(t, s.System.Update.State.LastError, "-CAfile")
	require.Contains(t, s.System.Update.State.LastError, "exit status 4")

	// One line, of bounded length.
	recordCheck(t.Context(), s, errors.New(strings.Repeat("x", 500)+"\nsecond line"))
	require.NotContains(t, s.System.Update.State.LastError, "\n")
	require.LessOrEqual(t, len(s.System.Update.State.LastError), 200)
}

func TestErrorKindOfVerifierFailures(t *testing.T) {
	t.Parallel()

	require.Equal(t, ErrorKindParse, errorKind(fmt.Errorf("index: %w", util.ErrSMIMEDamaged)))
	require.Equal(t, ErrorKindOther, errorKind(context.Canceled))
	require.Equal(t, ErrorKindOther, errorKind(fmt.Errorf("unable to run openssl: %w", exec.ErrNotFound)))
}
