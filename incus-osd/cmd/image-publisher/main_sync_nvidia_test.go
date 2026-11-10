package main

import (
	"archive/zip"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	apiupdate "github.com/lxc/incus-os/incus-osd/api/images"
)

func TestDownloadImageNVIDIA(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")

	zf, err := os.Create(zipPath)
	require.NoError(t, err)

	zw := zip.NewWriter(zf)

	for _, name := range []string{"nvidia.raw.gz", "nvidia-lts.raw.gz", "nvidia.manifest.json.gz", "nvidia-lts.manifest.json.gz", "unknown.raw.gz"} {
		w, err := zw.Create(name)
		require.NoError(t, err)

		_, err = w.Write([]byte(name))
		require.NoError(t, err)
	}

	require.NoError(t, zw.Close())
	require.NoError(t, zf.Close())

	files, err := (&cmdSync{}).downloadImage(t.Context(), "x86_64", &url.URL{Path: zipPath}, filepath.Join(dir, "out"))
	require.NoError(t, err)

	got := map[string][2]string{}
	for _, f := range files {
		got[f.Filename] = [2]string{f.Component.String(), string(f.Type)}
	}

	require.Equal(t, map[string][2]string{
		"x86_64/nvidia.raw.gz":               {"nvidia", string(apiupdate.UpdateFileTypeApplication)},
		"x86_64/nvidia-lts.raw.gz":           {"nvidia-lts", string(apiupdate.UpdateFileTypeApplication)},
		"x86_64/nvidia.manifest.json.gz":     {"nvidia", string(apiupdate.UpdateFileTypeImageManifest)},
		"x86_64/nvidia-lts.manifest.json.gz": {"nvidia-lts", string(apiupdate.UpdateFileTypeImageManifest)},
	}, got)
}
