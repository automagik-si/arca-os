package images_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api/images"
)

func TestUpdateFileComponentNVIDIA(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]images.UpdateFileComponent{
		"nvidia":     images.UpdateFileComponentNVIDIA,
		"nvidia-lts": images.UpdateFileComponentNVIDIALTS,
	} {
		require.Equal(t, name, c.String())
		require.Contains(t, images.UpdateFileComponents, c)

		// The component round-trips through its text form, as update.json carries it.
		var got images.UpdateFileComponent

		require.NoError(t, got.UnmarshalText([]byte(name)))
		require.Equal(t, c, got)
	}
}
