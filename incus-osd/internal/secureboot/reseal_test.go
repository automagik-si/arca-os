package secureboot

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/subprocess"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/util"
)

// resealCalls records what the stubbed re-seal was asked to do.
type resealCalls struct {
	pcr4 []string
	pcr7 int
}

// stubReseal replaces the re-seal's calls for one test and returns the state it acts on.
func stubReseal(t *testing.T, sbEnabled bool, fail error) (*state.State, *resealCalls) {
	t.Helper()

	dir := t.TempDir()
	calls := &resealCalls{}

	marker, sbEnabledFn, ukisFn, pcr4Fn, pcr7Fn, nowFn := resealMarker, resealSecureBootEnabled, resealUKIVersions, resealPCR4Binding, resealPCR7Binding, resealNow

	t.Cleanup(func() {
		resealMarker, resealSecureBootEnabled, resealUKIVersions, resealPCR4Binding, resealPCR7Binding, resealNow = marker, sbEnabledFn, ukisFn, pcr4Fn, pcr7Fn, nowFn
	})

	resealMarker = filepath.Join(dir, "run", "tpm-resealed")
	resealSecureBootEnabled = func() (bool, error) { return sbEnabled, nil }
	resealUKIVersions = func() (util.UKIVersions, error) {
		return util.UKIVersions{
			CurrentVersion:  "202611140000",
			CurrentFilepath: "/boot/EFI/Linux/ArcaOS_202611140000.efi",
			OtherVersion:    "202611150000",
			OtherFilepath:   "/boot/EFI/Linux/ArcaOS_202611150000.efi",
		}, nil
	}
	resealPCR4Binding = func(_ context.Context, uki string) error {
		calls.pcr4 = append(calls.pcr4, uki)

		return fail
	}
	resealPCR7Binding = func(context.Context) error {
		calls.pcr7++

		return fail
	}
	resealNow = func() time.Time { return time.Date(2026, 11, 14, 12, 30, 0, 0, time.UTC) }

	s, err := state.LoadOrCreate(filepath.Join(dir, "state.txt"))
	if err != nil {
		t.Fatal(err)
	}

	s.OS.RunningRelease = "202611140000"
	s.OS.NextRelease = "202611140000"

	return s, calls
}

func volumes(states ...string) []api.SystemSecurityEncryptedVolume {
	names := []string{"root", "swap"}
	ret := make([]api.SystemSecurityEncryptedVolume, 0, len(states))

	for i, st := range states {
		ret = append(ret, api.SystemSecurityEncryptedVolume{Volume: names[i], State: st})
	}

	return ret
}

func TestResealOnlyAfterRecoveryUnlock(t *testing.T) { //nolint:paralleltest // replaces the package's re-seal calls
	for _, tc := range []struct { //nolint:paralleltest // the subtests replace the package's re-seal calls
		name    string
		volumes []api.SystemSecurityEncryptedVolume
		want    int
	}{
		{"recovery passphrase", volumes(api.EncryptedVolumeUnlockedRecovery, api.EncryptedVolumeUnlockedRecovery), 1},
		{"recovery passphrase without swap", volumes(api.EncryptedVolumeUnlockedRecovery), 1},
		{"TPM", volumes(api.EncryptedVolumeUnlockedTPM, api.EncryptedVolumeUnlockedTPM), 0},
		{"PCR update pending", volumes("unlocked (TPM; PCR update pending)", "unlocked (TPM; PCR update pending)"), 0},
		{"root locked", volumes("locked"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) { //nolint:paralleltest // replaces the package's re-seal calls
			s, calls := stubReseal(t, false, nil)
			s.System.Security.State.EncryptedVolumes = tc.volumes

			ran := ResealAfterRecoveryUnlock(context.Background(), s)
			if ran != (tc.want == 1) || len(calls.pcr4) != tc.want || calls.pcr7 != 0 {
				t.Fatalf("ran %v, PCR4 re-seals %v, PCR7 re-seals %d; want %d re-seal", ran, calls.pcr4, calls.pcr7, tc.want)
			}

			sec := s.System.Security.State
			if tc.want == 0 && (sec.LastRecoveryUnlock != "" || sec.ResealStatus != "") {
				t.Fatalf("a TPM boot recorded a recovery unlock: %+v", sec)
			}

			// A second start in the same boot does nothing.
			if ResealAfterRecoveryUnlock(context.Background(), s) || len(calls.pcr4) != tc.want {
				t.Fatalf("re-sealed again in the same boot: %v", calls.pcr4)
			}
		})
	}
}

func TestResealPerSecureBootState(t *testing.T) { //nolint:paralleltest // replaces the package's re-seal calls
	// Secure Boot enabled: a fresh PCR7 binding, no PCR4.
	s, calls := stubReseal(t, true, nil)
	s.System.Security.State.EncryptedVolumes = volumes(api.EncryptedVolumeUnlockedRecovery, api.EncryptedVolumeUnlockedRecovery)

	ResealAfterRecoveryUnlock(context.Background(), s)

	if calls.pcr7 != 1 || len(calls.pcr4) != 0 {
		t.Fatalf("Secure Boot on: PCR7 re-seals %d, PCR4 re-seals %v", calls.pcr7, calls.pcr4)
	}

	// Secure Boot disabled: UpdatePCR4Binding with the running UKI.
	s, calls = stubReseal(t, false, nil)
	s.System.Security.State.EncryptedVolumes = volumes(api.EncryptedVolumeUnlockedRecovery, api.EncryptedVolumeUnlockedRecovery)

	ResealAfterRecoveryUnlock(context.Background(), s)

	if calls.pcr7 != 0 || len(calls.pcr4) != 1 || calls.pcr4[0] != "/boot/EFI/Linux/ArcaOS_202611140000.efi" {
		t.Fatalf("Secure Boot off: PCR7 re-seals %d, PCR4 re-seals %v", calls.pcr7, calls.pcr4)
	}

	sec := s.System.Security.State
	if sec.ResealStatus != api.ResealStatusResealed || sec.ResealReason != "" || sec.LastRecoveryUnlock != "2026-11-14T12:30:00Z" {
		t.Fatalf("security state after a re-seal: %+v", sec)
	}

	// Booted from the backup image, the newest image boots next and is the one sealed to.
	s, calls = stubReseal(t, false, nil)
	s.System.Security.State.EncryptedVolumes = volumes(api.EncryptedVolumeUnlockedRecovery)
	s.OS.NextRelease = "202611150000"

	ResealAfterRecoveryUnlock(context.Background(), s)

	if len(calls.pcr4) != 1 || calls.pcr4[0] != "/boot/EFI/Linux/ArcaOS_202611150000.efi" {
		t.Fatalf("running from backup: PCR4 re-seals %v", calls.pcr4)
	}
}

func TestResealFailure(t *testing.T) { //nolint:paralleltest // replaces the package's re-seal calls
	runErr := subprocess.NewRunError("systemd-cryptenroll",
		[]string{"--unlock-key-file=/var/lib/incus-os/keys/recovery.root.key", "--tpm2-device=auto", "/dev/disk/by-partlabel/root-x86-64"},
		errors.New("exit status 1"), &bytes.Buffer{}, bytes.NewBufferString("Failed to enroll new TPM2 token\nsecond line"))

	s, calls := stubReseal(t, false, runErr)
	s.System.Security.State.EncryptedVolumes = volumes(api.EncryptedVolumeUnlockedRecovery, api.EncryptedVolumeUnlockedRecovery)

	if !ResealAfterRecoveryUnlock(context.Background(), s) {
		t.Fatal("no re-seal was attempted")
	}

	sec := s.System.Security.State
	if sec.ResealStatus != api.ResealStatusFailed || sec.ResealReason != "command failed: exit status 1: Failed to enroll new TPM2 token" {
		t.Fatalf("security state after a failed re-seal: %+v", sec)
	}

	// No retry in the same boot, and the result is kept.
	if ResealAfterRecoveryUnlock(context.Background(), s) || len(calls.pcr4) != 1 {
		t.Fatalf("retried in the same boot: %v", calls.pcr4)
	}

	// The result is saved to state.
	saved, err := state.LoadOrCreate(filepath.Join(filepath.Dir(filepath.Dir(resealMarker)), "state.txt"))
	if err != nil {
		t.Fatal(err)
	}

	got := saved.System.Security.State
	if got.ResealStatus != api.ResealStatusFailed || got.ResealReason != sec.ResealReason || got.LastRecoveryUnlock != sec.LastRecoveryUnlock {
		t.Fatalf("saved security state: %+v", got)
	}
}

func TestResealReason(t *testing.T) {
	t.Parallel()

	long := errors.New(strings.Repeat("x", 300) + "\nsecond line")

	got := resealReason(long)
	if len(got) > resealReasonMax || strings.Contains(got, "second line") || !strings.HasSuffix(got, "…") {
		t.Fatalf("long reason: %q", got)
	}

	if got := resealReason(errors.New("open /sys/class/tpm/tpm0/pcr-sha256/7: no such file or directory")); got != "open /sys/class/tpm/tpm0/pcr-sha256/7: no such file or directory" {
		t.Fatalf("plain reason: %q", got)
	}
}
