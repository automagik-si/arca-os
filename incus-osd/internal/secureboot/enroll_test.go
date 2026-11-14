package secureboot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubCryptenroll puts a fake systemd-cryptenroll on PATH that logs its arguments, one call
// per line, and prints stderr on each call.
func stubCryptenroll(t *testing.T, stderr string) func() []string {
	t.Helper()

	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\necho '" + stderr + "' >&2\n"

	err := os.WriteFile(filepath.Join(dir, "systemd-cryptenroll"), []byte(script), 0o700) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return func() []string {
		data, err := os.ReadFile(log) //nolint:gosec
		if err != nil {
			t.Fatal(err)
		}

		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}

func TestEnrollTPM2(t *testing.T) { //nolint:paralleltest // puts a fake systemd-cryptenroll on PATH
	pcr7 := make([]byte, 32)
	binding := pcrBindingArgument(nil, pcr7, nil)
	want := "--unlock-key-file=/var/lib/incus-os/keys/recovery.root.key --tpm2-device=auto --wipe-slot=tpm2 --tpm2-pcrlock= " + binding + " /dev/root-luks"

	calls := stubCryptenroll(t, "New TPM2 token enrolled as key slot 2.")

	err := enrollTPM2(context.Background(), "root", "/dev/root-luks", binding)
	if err != nil {
		t.Fatal(err)
	}

	if got := calls(); len(got) != 1 || got[0] != want {
		t.Fatalf("calls %q, want one %q", got, want)
	}

	// The same PCR set already enrolled: a throwaway policy first, then the real one again.
	calls = stubCryptenroll(t, "This PCR set is already enrolled, executing no operation.")

	err = enrollTPM2(context.Background(), "root", "/dev/root-luks", binding)
	if err != nil {
		t.Fatal(err)
	}

	got := calls()
	if len(got) != 3 || got[0] != want || got[2] != want || got[1] == want || !strings.Contains(got[1], "--tpm2-pcrs=7:sha256=") {
		t.Fatalf("calls after an unchanged PCR set: %q", got)
	}
}
