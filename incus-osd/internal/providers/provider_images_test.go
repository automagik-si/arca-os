package providers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/util"
	"github.com/lxc/incus-os/incus-osd/internal/util/smimetest"
)

// stubIndex serves body as the signed index, or status when it isn't 200.
func stubIndex(t *testing.T, status int, body []byte) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/index.sjson" || status != http.StatusOK {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	return srv.URL
}

func TestImagesCheckTrust(t *testing.T) {
	t.Parallel()
	smimetest.RequireOpenSSL(t)

	root := smimetest.NewIdentity(t, "Update root", 1, nil, true)
	updateCA := smimetest.NewIdentity(t, "Update CA", 2, root, true)
	leaf := smimetest.NewIdentity(t, "Update signer", 3, updateCA, false)
	newRoot := smimetest.NewIdentity(t, "Newer root", 1, nil, true)
	newCA := smimetest.NewIdentity(t, "Newer CA", 2, newRoot, true)
	newLeaf := smimetest.NewIdentity(t, "Newer signer", 3, newCA, false)

	index, err := json.Marshal(map[string]any{"format": "1.0", "updates": []any{}})
	require.NoError(t, err)

	provider := func(serverURL string) *images {
		return &images{
			state:     &state.State{},
			serverURL: serverURL,
			updateCA:  updateCA.Cert,
			rootCA:    root.Cert,
			client:    &http.Client{},
		}
	}

	// A tree signed under a newer CA is a trust failure naming both CAs.
	p := provider(stubIndex(t, http.StatusOK, smimetest.Sign(t, string(index), newLeaf, newCA)))

	_, err = p.GetOSUpdate(t.Context())

	var trustErr *util.TrustError

	require.ErrorAs(t, err, &trustErr)
	require.Equal(t, util.CertificateFingerprint(updateCA.Cert), trustErr.ExpectedFingerprint)
	require.Equal(t, util.CertificateFingerprint(newCA.Cert), trustErr.PresentedFingerprint)
	require.Equal(t, util.CertificateFingerprint(root.Cert), trustErr.RootFingerprint)

	// A failure isn't cached: the next check asks again.
	require.True(t, p.lastCheck.IsZero())

	// A matching tree verifies; it holds no update.
	p = provider(stubIndex(t, http.StatusOK, smimetest.Sign(t, string(index), leaf, updateCA)))

	_, err = p.GetOSUpdate(t.Context())
	require.ErrorIs(t, err, ErrNoUpdateAvailable)

	// A matching signature over content that isn't an index.
	p = provider(stubIndex(t, http.StatusOK, smimetest.Sign(t, "not json", leaf, updateCA)))

	_, err = p.GetOSUpdate(t.Context())

	var syntaxErr *json.SyntaxError

	require.ErrorAs(t, err, &syntaxErr)

	// The server doesn't have the index.
	p = provider(stubIndex(t, http.StatusNotFound, nil))

	_, err = p.GetOSUpdate(t.Context())
	require.ErrorIs(t, err, ErrServerResponse)
	require.NotErrorAs(t, err, &trustErr)
}
