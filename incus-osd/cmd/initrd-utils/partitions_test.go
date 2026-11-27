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
				"sda1": "DEVTYPE=partition\nPARTNAME=esp\nPARTUUID=" + uuid + "\n",
				"sdb1": "DEVTYPE=partition\nPARTUUID=AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE\nPARTNAME=esp\n",
				"sdc2": "PARTNAME=seed-data\nPARTUUID=" + uuid + "\n",
				"sdc1": "PARTUUID=\n",
				"sda":  "DEVTYPE=disk\n",
			},
			want: "WARNING: duplicate PARTUUID " + uuid + " on /dev/sda1, /dev/sdb1; continuing boot.\n",
		},
		{
			name: "distinct",
			uevent: map[string]string{
				"sda1": "PARTNAME=esp\nPARTUUID=" + uuid + "\n",
				"sdb1": "PARTNAME=esp\nPARTUUID=11111111-2222-3333-4444-555555555555\n",
			},
		},
		{
			name: "expected non-ESP duplicates stay silent",
			uevent: map[string]string{
				"sda5": "PARTNAME=IncusOS_202601010000\nPARTUUID=" + uuid + "\n",
				"sdb5": "PARTNAME=IncusOS_202601010000\nPARTUUID=" + uuid + "\n",
			},
		},
		{
			name: "only one ESP shares the UUID",
			uevent: map[string]string{
				"sda1": "PARTNAME=esp\nPARTUUID=" + uuid + "\n",
				"sdb2": "PARTNAME=seed-data\nPARTUUID=" + uuid + "\n",
			},
		},
		{name: "single", uevent: map[string]string{"sda1": "PARTNAME=esp\nPARTUUID=" + uuid + "\n"}},
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
