package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLUKS2 writes a minimal LUKS2 header holding the given keyslots JSON.
func writeLUKS2(t *testing.T, keyslots string) string {
	t.Helper()

	header := make([]byte, 16384)
	copy(header, "LUKS\xba\xbe")
	binary.BigEndian.PutUint16(header[6:8], 2)
	binary.BigEndian.PutUint64(header[8:16], 16384)
	copy(header[4096:], `{"keyslots":`+keyslots+`,"tokens":{}}`)

	path := filepath.Join(t.TempDir(), "root-luks")

	err := os.WriteFile(path, header, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

func TestDiagnoseTPMRecordMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missing := filepath.Join(dir, "arca-tpm-seal.json")

	tests := []struct {
		name   string
		device string
		want   string
	}{
		{"a fresh volume stays quiet", writeLUKS2(t, `{"0":{"type":"luks2"}}`), ""},
		{"a sealed volume gets the hint", writeLUKS2(t, `{"1":{},"2":{},"3":{}}`), "If asked, type the recovery passphrase (8 words) or older recovery key (dashes)"},
		{"an unreadable volume gets the hint", filepath.Join(dir, "none"), "If asked, type the recovery passphrase (8 words) or older recovery key (dashes)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer

			diagnoseTPM(&out, []string{missing, dir, dir, tt.device})

			if tt.want == "" && out.Len() != 0 || !strings.Contains(out.String(), tt.want) {
				t.Fatalf("output %q, want %q", out.String(), tt.want)
			}
		})
	}
}
