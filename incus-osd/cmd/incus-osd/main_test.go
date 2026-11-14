package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lxc/incus-os/incus-osd/internal/state"
)

func TestPrepareRecoveryHandOver(t *testing.T) {
	t.Parallel()

	for _, acknowledged := range []bool{false, true} {
		dir := t.TempDir()
		flag := filepath.Join(dir, "recovery-passphrase-acknowledged")

		if acknowledged {
			err := os.WriteFile(flag, nil, 0o600)
			if err != nil {
				t.Fatal(err)
			}
		}

		s, err := state.LoadOrCreate(filepath.Join(dir, "state.txt"))
		if err != nil {
			t.Fatal(err)
		}

		s.System.Security.Config.EncryptionRecoveryKeys = []string{"abandon ability able about above absent absorb abstract"}

		err = prepareRecoveryHandOver(s, flag)
		if err != nil {
			t.Fatal(err)
		}

		// The keys are kept and not retrieved; the seed's flag is carried into state once.
		saved, err := state.LoadOrCreate(filepath.Join(dir, "state.txt"))
		if err != nil {
			t.Fatal(err)
		}

		if len(saved.System.Security.Config.EncryptionRecoveryKeys) != 1 || saved.System.Security.State.EncryptionRecoveryKeysRetrieved {
			t.Fatalf("acknowledged %v: keys %d, retrieved %v", acknowledged, len(saved.System.Security.Config.EncryptionRecoveryKeys), saved.System.Security.State.EncryptionRecoveryKeysRetrieved)
		}

		if !saved.RecoveryPassphrasePending || saved.RecoveryPassphraseAcknowledged != acknowledged {
			t.Fatalf("acknowledged %v: state says pending %v, acknowledged %v", acknowledged, saved.RecoveryPassphrasePending, saved.RecoveryPassphraseAcknowledged)
		}

		_, err = os.Stat(flag)
		if !os.IsNotExist(err) {
			t.Fatalf("acknowledged %v: the flag file is still there", acknowledged)
		}
	}
}
