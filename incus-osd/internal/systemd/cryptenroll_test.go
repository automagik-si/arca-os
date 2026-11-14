package systemd

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lxc/incus-os/incus-osd/internal/passphrase"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// stubTools puts fake systemd-cryptenroll and cryptsetup on PATH, each logging one
// line per call: its name, its arguments, then NEWPASSWORD or its stdin after a "|".
func stubTools(t *testing.T) (string, func() []string) {
	t.Helper()

	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")

	for _, name := range []string{"systemd-cryptenroll", "cryptsetup"} {
		script := "#!/bin/sh\nsecret=\"$NEWPASSWORD\"\n[ \"$0\" = \"" + filepath.Join(dir, "cryptsetup") + "\" ] && secret=\"$(cat)\"\n" +
			"echo \"$(basename \"$0\") $*|$secret\" >> " + log + "\n"

		err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700) //nolint:gosec
		if err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	keys := t.TempDir()
	oldKeys, oldVolumes := keysDir, luksVolumePartitions
	keysDir = keys
	luksVolumePartitions = func(context.Context) (map[string]string, error) {
		return map[string]string{"root": "/dev/test-root", "swap": "/dev/test-swap"}, nil
	}

	t.Cleanup(func() { keysDir, luksVolumePartitions = oldKeys, oldVolumes })

	return keys, func() []string {
		body, err := os.ReadFile(log) //nolint:gosec
		if err != nil {
			t.Fatal(err)
		}

		return strings.Split(strings.TrimSpace(string(body)), "\n")
	}
}

func TestGenerateRecoveryKeys(t *testing.T) { //nolint:paralleltest // sets PATH and the keys directory
	keys, calls := stubTools(t)

	s := &state.State{}
	s.System.Security.State.EncryptionRecoveryKeysRetrieved = true

	err := GenerateRecoveryKeys(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}

	if len(s.System.Security.Config.EncryptionRecoveryKeys) != 1 || s.System.Security.State.EncryptionRecoveryKeysRetrieved {
		t.Fatalf("state: %d keys, retrieved %v", len(s.System.Security.Config.EncryptionRecoveryKeys), s.System.Security.State.EncryptionRecoveryKeysRetrieved)
	}

	common := s.System.Security.Config.EncryptionRecoveryKeys[0]
	if !passphrase.Valid(common) {
		t.Fatalf("common key %q is not a passphrase", common)
	}

	log := calls()

	for _, c := range log {
		if strings.Contains(c, "--recovery-key") {
			t.Fatalf("a recovery-key slot was enrolled: %q", c)
		}
	}

	for _, vol := range []string{"root", "swap"} {
		dev := "/dev/test-" + vol

		own, err := os.ReadFile(filepath.Join(keys, "recovery."+vol+".key")) //nolint:gosec
		if err != nil {
			t.Fatal(err)
		}

		if !passphrase.Valid(string(own)) || string(own) == common {
			t.Fatalf("%s key file holds %q", vol, own)
		}

		info, _ := os.Stat(filepath.Join(keys, "recovery."+vol+".key"))
		if info.Mode().Perm() != 0o400 {
			t.Fatalf("%s key file mode %v", vol, info.Mode())
		}

		wipe := slices.Index(log, "systemd-cryptenroll --unlock-tpm2-device=auto --wipe-slot=recovery,password "+dev+"|")
		ownSlot := slices.Index(log, "systemd-cryptenroll --unlock-tpm2-device=auto --password "+dev+"|"+string(own))
		commonSlot := slices.Index(log, "systemd-cryptenroll --unlock-tpm2-device=auto --password "+dev+"|"+common)

		if wipe < 0 || ownSlot < 0 || commonSlot < 0 || wipe > ownSlot || wipe > commonSlot {
			t.Fatalf("%s: wipe at %d, own password slot at %d, common at %d in %q", vol, wipe, ownSlot, commonSlot, log)
		}
	}

	if len(log) != 6 {
		t.Fatalf("%d calls, want 6: %q", len(log), log)
	}
}

func TestWipeAllRecoveryKeys(t *testing.T) { //nolint:paralleltest // sets PATH and the keys directory
	keys, calls := stubTools(t)

	own := "abandon ability able about above absent absorb abstract"

	err := os.WriteFile(filepath.Join(keys, "recovery.root.key"), []byte(own), 0o400)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(keys, "recovery.swap.key"), []byte("cbdefghi-jklnrtuv"), 0o400)
	if err != nil {
		t.Fatal(err)
	}

	// A passphrase volume keeps its own slot: only the given keys are removed.
	err = WipeAllRecoveryKeys(context.Background(), "root", "/dev/test-root", []string{"user-key!", own})
	if err != nil {
		t.Fatal(err)
	}

	// A volume with a recovery-type key still has its password slots wiped.
	err = WipeAllRecoveryKeys(context.Background(), "swap", "/dev/test-swap", []string{"user-key!"})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"cryptsetup luksRemoveKey --batch-mode --key-file=- /dev/test-root|user-key!",
		"systemd-cryptenroll --unlock-key-file=" + filepath.Join(keys, "recovery.swap.key") + " --wipe-slot=password /dev/test-swap|",
	}

	if got := calls(); !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
}
