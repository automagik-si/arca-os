package secureboot

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// stubCryptenroll puts a fake systemd-cryptenroll on PATH that logs its arguments, one call
// per line, prints stderr and exits with code on each call.
func stubCryptenroll(t *testing.T, stderr string, code int) func() []string {
	t.Helper()

	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\necho '" + stderr + "' >&2\nexit " + strconv.Itoa(code) + "\n"

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

	calls := stubCryptenroll(t, "New TPM2 token enrolled as key slot 2.", 0)

	err := enrollTPM2(context.Background(), "root", "/dev/root-luks", binding)
	if err != nil {
		t.Fatal(err)
	}

	if got := calls(); len(got) != 1 || got[0] != want {
		t.Fatalf("calls %q, want one %q", got, want)
	}

	// The same PCR set already enrolled: a throwaway policy first, then the real one again.
	calls = stubCryptenroll(t, "This PCR set is already enrolled, executing no operation.", 0)

	err = enrollTPM2(context.Background(), "root", "/dev/root-luks", binding)
	if err != nil {
		t.Fatal(err)
	}

	got := calls()
	if len(got) != 3 || got[0] != want || got[2] != want || got[1] == want || !strings.Contains(got[1], "--tpm2-pcrs=7:sha256=") {
		t.Fatalf("calls after an unchanged PCR set: %q", got)
	}
}

// stubPCRsAndVolumes points ReadPCR at a fake sysfs with distinct PCR 4, 7 and 12 values,
// and the LUKS volume list at root and swap.
func stubPCRsAndVolumes(t *testing.T) {
	t.Helper()

	dir := t.TempDir()

	for index, digit := range map[string]string{"4": "4", "7": "7", "12": "c"} {
		err := os.WriteFile(filepath.Join(dir, index), []byte(strings.Repeat(digit, 64)+"\n"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	savedDir, savedVolumes, savedSeal, savedMarker := pcrDir, luksVolumePartitions, SealStatePath, SealStateBootMarker

	t.Cleanup(func() {
		pcrDir, luksVolumePartitions, SealStatePath, SealStateBootMarker = savedDir, savedVolumes, savedSeal, savedMarker
	})

	pcrDir = dir + "/"
	SealStatePath = filepath.Join(dir, "arca-tpm-seal.json")
	SealStateBootMarker = filepath.Join(dir, "tpm-seal-state.boot")
	luksVolumePartitions = func(context.Context) (map[string]string, error) {
		return map[string]string{"root": "/dev/root-luks", "swap": "/dev/swap-luks"}, nil
	}
}

func TestUpdatePCR7Binding(t *testing.T) { //nolint:paralleltest // puts a fake systemd-cryptenroll on PATH
	stubPCRsAndVolumes(t)

	calls := stubCryptenroll(t, "New TPM2 token enrolled as key slot 2.", 0)

	// Secure Boot is on, and the drive was sealed to another PCR7 before the firmware changed.
	savedEFI := sealReadEFIVariable

	t.Cleanup(func() { sealReadEFIVariable = savedEFI })

	sealReadEFIVariable = func(name string) ([]byte, error) { return map[string][]byte{"SecureBoot": {1}}[name], nil }

	err := os.WriteFile(SealStatePath, []byte(`{"version":1,"secure_boot_enabled":true,"pcr7":"`+strings.Repeat("b", 64)+`"}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = UpdatePCR7Binding(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	pcrs := "--tpm2-pcrs=7:sha256=" + strings.Repeat("7", 64) + "+15:sha256=" + strings.Repeat("0", 64)
	want := []string{
		"--unlock-key-file=/var/lib/incus-os/keys/recovery.root.key --tpm2-device=auto --wipe-slot=tpm2 --tpm2-pcrlock= " + pcrs + " /dev/root-luks",
		"--unlock-key-file=/var/lib/incus-os/keys/recovery.swap.key --tpm2-device=auto --wipe-slot=tpm2 --tpm2-pcrlock= " + pcrs + " /dev/swap-luks",
	}

	got := calls()
	slices.Sort(got)

	if !slices.Equal(got, want) {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// The record now holds the re-sealed PCR7, so the next boot, unchanged, has nothing to say.
	record, err := ReadSealState(SealStatePath)
	if err != nil {
		t.Fatal(err)
	}

	next := Measurements{SecureBootEnabled: true, PCR7: strings.Repeat("7", 64)}
	if DiagnoseSeal(record, next) != SealMatch {
		t.Fatalf("after the PCR7 re-seal the next boot diagnoses %d, record %+v", DiagnoseSeal(record, next), record)
	}

	// A failed enrolment is returned, so the re-seal is recorded as failed.
	stubCryptenroll(t, "Failed to unlock disk.", 1)

	err = UpdatePCR7Binding(context.Background())
	if err == nil {
		t.Fatal("a failed systemd-cryptenroll was not returned")
	}
}

func TestUpdatePCR7BindingFailingPartway(t *testing.T) { //nolint:paralleltest // puts a fake systemd-cryptenroll on PATH
	savedEFI := sealReadEFIVariable

	t.Cleanup(func() { sealReadEFIVariable = savedEFI })

	sealReadEFIVariable = func(name string) ([]byte, error) { return map[string][]byte{"SecureBoot": {1}}[name], nil }
	next := Measurements{SecureBootEnabled: true, PCR7: strings.Repeat("7", 64)}
	rootFirst := false

	// Volumes are re-bound in map order, so repeat until root went first at least once.
	for i := 0; i < 64 && !rootFirst; i++ {
		for _, failing := range []string{"/dev/root-luks", "/dev/swap-luks"} {
			stubPCRsAndVolumes(t)

			dir := t.TempDir()
			log := filepath.Join(dir, "calls.log")
			script := "#!/bin/sh\necho \"$*\" >> " + log + "\ncase \"$*\" in *" + failing + ") exit 1;; esac\n"

			err := os.WriteFile(filepath.Join(dir, "systemd-cryptenroll"), []byte(script), 0o700) //nolint:gosec
			if err != nil {
				t.Fatal(err)
			}

			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

			err = os.WriteFile(SealStatePath, []byte(`{"version":1,"secure_boot_enabled":true,"pcr7":"`+strings.Repeat("b", 64)+`"}`), 0o600)
			if err != nil {
				t.Fatal(err)
			}

			err = UpdatePCR7Binding(context.Background())
			if err == nil {
				t.Fatalf("a failing %s didn't fail the re-seal", failing)
			}

			calls, err := os.ReadFile(log) //nolint:gosec
			if err != nil {
				t.Fatal(err)
			}

			rootBound := failing != "/dev/root-luks" && strings.Contains(string(calls), "/dev/root-luks")
			rootFirst = rootFirst || rootBound

			// The record follows root's binding, whichever volume failed.
			record, err := ReadSealState(SealStatePath)
			if err != nil {
				t.Fatal(err)
			}

			if (DiagnoseSeal(record, next) == SealMatch) != rootBound {
				t.Fatalf("%s failed, root re-bound %t, record %+v", failing, rootBound, record)
			}

			// The boot is marked before the first re-bind, so a restart can't repair the record.
			_, err = os.Stat(SealStateBootMarker)
			if err != nil {
				t.Fatalf("%s failed and the boot isn't marked: %v", failing, err)
			}
		}
	}

	if !rootFirst {
		t.Fatal("root was never re-bound before swap failed")
	}
}
