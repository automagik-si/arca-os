package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckPartitionUUIDs(t *testing.T) {
	t.Parallel()

	const uuid = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	tests := []struct {
		name   string
		uevent map[string]string
		want   string
	}{
		{
			name: "duplicate",
			uevent: map[string]string{
				"sda1": "DEVTYPE=partition\nPARTUUID=" + uuid + "\n",
				"sdb1": "DEVTYPE=partition\nPARTUUID=AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE\n",
				"sdc1": "PARTUUID=\n",
				"sda":  "DEVTYPE=disk\n",
			},
			want: "WARNING: duplicate PARTUUID " + uuid + " on /dev/sda1, /dev/sdb1; continuing boot.\n",
		},
		{
			name: "distinct",
			uevent: map[string]string{
				"sda1": "PARTUUID=" + uuid + "\n",
				"sdb1": "PARTUUID=11111111-2222-3333-4444-555555555555\n",
			},
		},
		{name: "single", uevent: map[string]string{"sda1": "PARTUUID=" + uuid + "\n"}},
		{name: "empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			for device, contents := range tt.uevent {
				dir := filepath.Join(root, device)
				require.NoError(t, os.Mkdir(dir, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "uevent"), []byte(contents), 0o600))
			}

			var out bytes.Buffer

			checkPartitionUUIDs(&out, root)
			require.Equal(t, tt.want, out.String())
		})
	}
}
