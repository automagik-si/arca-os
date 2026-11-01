package images_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api/images"
)

func TestUpdateFileComponentArca(t *testing.T) {
	t.Parallel()

	require.Equal(t, "arca", images.UpdateFileComponentArca.String())
	require.Contains(t, images.UpdateFileComponents, images.UpdateFileComponentArca)

	// The component round-trips through its text form, as update.json carries it.
	var c images.UpdateFileComponent

	require.NoError(t, c.UnmarshalText([]byte("arca")))
	require.Equal(t, images.UpdateFileComponentArca, c)
}
