package secureboot

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	pcrA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pcrB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	pcrC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// stubSealReaders replaces the PCR, EFI variable and clock readers used by RecordSealState.
func stubSealReaders(t *testing.T, pcrs map[string][]byte, efi map[string][]byte) {
	t.Helper()

	oldPCR, oldEFI, oldNow, oldPath, oldMarker := sealReadPCR, sealReadEFIVariable, sealNow, SealStatePath, SealStateBootMarker

	t.Cleanup(func() {
		sealReadPCR, sealReadEFIVariable, sealNow, SealStatePath, SealStateBootMarker = oldPCR, oldEFI, oldNow, oldPath, oldMarker
	})

	SealStateBootMarker = filepath.Join(t.TempDir(), "run", "tpm-seal-state.boot")

	sealReadPCR = func(index string) ([]byte, error) {
		value, ok := pcrs[index]
		if !ok {
			return nil, errors.New("no PCR " + index)
		}

		return value, nil
	}

	sealReadEFIVariable = func(name string) ([]byte, error) { return efi[name], nil }
	sealNow = func() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.FixedZone("x", 3600)) }
}

func TestRecordSealStateRoundTrip(t *testing.T) { //nolint:paralleltest // replaces package readers
	stubSealReaders(t, map[string][]byte{"7": bytes.Repeat([]byte{0xBB}, 32)}, map[string][]byte{"SecureBoot": {0}, "SetupMode": {1}})

	SealStatePath = filepath.Join(t.TempDir(), "arca-tpm-seal.json")

	RecordSealState("/boot/EFI/Linux/ArcaOS_202611141118+3-0.efi", bytes.Repeat([]byte{0xAA}, 32), nil, bytes.Repeat([]byte{0xCC}, 32))

	got, err := ReadSealState(SealStatePath)
	if err != nil {
		t.Fatal(err)
	}

	want := SealState{
		Version:           1,
		UKIVersion:        "202611141118",
		SecureBootEnabled: false,
		SetupMode:         true,
		PCR4:              pcrA,
		PCR7:              pcrB,
		PCR12:             pcrC,
		SealedAt:          "2026-11-14T11:00:00Z",
	}

	if *got != want {
		t.Fatalf("record:\n got %+v\nwant %+v", *got, want)
	}

	// The file holds exactly these fields and nothing else.
	raw, err := os.ReadFile(SealStatePath)
	if err != nil {
		t.Fatal(err)
	}

	var fields map[string]any

	err = json.Unmarshal(raw, &fields)
	if err != nil {
		t.Fatal(err)
	}

	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	wantKeys := []string{"pcr12", "pcr4", "pcr7", "sealed_at", "secure_boot_enabled", "setup_mode", "uki_version", "version"}
	if !slices.Equal(keys, wantKeys) {
		t.Fatalf("fields %v, want %v", keys, wantKeys)
	}
}

func TestRecordSealStateWriteFailure(t *testing.T) { //nolint:paralleltest // replaces package readers
	stubSealReaders(t, nil, nil)

	SealStatePath = filepath.Join(t.TempDir(), "missing", "arca-tpm-seal.json")

	// It must return, without a record, even though nothing could be read or written.
	RecordSealState("", nil, nil, nil)

	_, err := os.Stat(SealStatePath)
	if !os.IsNotExist(err) {
		t.Fatalf("record written to a missing directory: %v", err)
	}
}

func TestRepairSealState(t *testing.T) { //nolint:paralleltest // replaces package readers
	stubSealReaders(t, map[string][]byte{"4": bytes.Repeat([]byte{0xAA}, 32), "7": bytes.Repeat([]byte{0xBB}, 32), "12": bytes.Repeat([]byte{0xCC}, 32)}, map[string][]byte{"SecureBoot": {0}, "SetupMode": {1}})

	SealStatePath = filepath.Join(t.TempDir(), "arca-tpm-seal.json")
	matching := `{"version":1,"uki_version":"1","secure_boot_enabled":false,"setup_mode":true,"pcr4":"` + pcrA + `","pcr7":"` + pcrB + `","pcr12":"` + pcrC + `","sealed_at":"x"}`
	stale := strings.Replace(matching, pcrC, pcrA, 1)

	tests := []struct {
		name     string
		record   string
		unlocked bool
		rewrite  bool
	}{
		{"missing", "", true, true},
		{"unreadable", "{", true, true},
		{"stale", stale, true, true},
		{"matching", matching, true, false},
		{"stale but the TPM didn't unlock", stale, false, false},
	}

	for _, tt := range tests {
		// Each case is a new boot.
		_ = os.Remove(SealStatePath)
		_ = os.Remove(SealStateBootMarker)

		if tt.record != "" {
			err := os.WriteFile(SealStatePath, []byte(tt.record), 0o600)
			if err != nil {
				t.Fatal(err)
			}
		}

		RepairSealState(tt.unlocked)

		raw, _ := os.ReadFile(SealStatePath)
		rewritten := strings.Contains(string(raw), "2026-11-14T11:00:00Z")

		if rewritten != tt.rewrite {
			t.Fatalf("%s: rewritten %v, want %v: %s", tt.name, rewritten, tt.rewrite, raw)
		}
	}
}

func TestRepairSealStateOncePerBootAndNeverAfterASeal(t *testing.T) { //nolint:paralleltest // replaces package readers
	stubSealReaders(t, map[string][]byte{"4": bytes.Repeat([]byte{0xAA}, 32), "7": bytes.Repeat([]byte{0xBB}, 32), "12": bytes.Repeat([]byte{0xCC}, 32)}, map[string][]byte{"SecureBoot": {0}, "SetupMode": {1}})

	SealStatePath = filepath.Join(t.TempDir(), "arca-tpm-seal.json")

	pcr4 := func() string {
		record, err := ReadSealState(SealStatePath)
		if err != nil {
			t.Fatal(err)
		}

		return record.PCR4
	}

	// An update seals the next boot to a new UKI (PCR4 C), then incus-osd (re)starts on the same boot.
	RecordSealState("", bytes.Repeat([]byte{0xCC}, 32), nil, nil)
	RepairSealState(true)

	if pcr4() != pcrC {
		t.Fatalf("the repair replaced the sealed next-boot PCR4 with %s", pcr4())
	}

	// A new boot: the first start repairs a stale record, a second start on that boot doesn't touch it.
	_ = os.Remove(SealStateBootMarker)

	RepairSealState(true)

	if pcr4() != pcrA {
		t.Fatalf("the first start didn't repair the stale record: %s", pcr4())
	}

	RecordSealState("", bytes.Repeat([]byte{0xCC}, 32), nil, nil)

	RepairSealState(true)

	if pcr4() != pcrC {
		t.Fatalf("a second start on the same boot rewrote the record: %s", pcr4())
	}
}

func TestReadSealStateRejectsMalformed(t *testing.T) {
	t.Parallel()

	for name, record := range map[string]string{
		"escape in a pcr":     `{"version":1,"pcr7":"\u001b[2J` + pcrB[:60] + `"}`,
		"short pcr":           `{"version":1,"pcr4":"abcd"}`,
		"escape in a version": `{"version":1,"uki_version":"1\u001b[31m"}`,
		"no version":          `{"pcr4":"` + pcrA + `"}`,
	} {
		path := filepath.Join(t.TempDir(), "arca-tpm-seal.json")

		err := os.WriteFile(path, []byte(record), 0o600)
		if err != nil {
			t.Fatal(err)
		}

		_, err = ReadSealState(path)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestReadMeasurements(t *testing.T) {
	t.Parallel()

	efivars := t.TempDir()
	pcrs := t.TempDir()

	files := map[string]string{
		filepath.Join(efivars, "SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c"): "\x06\x00\x00\x00\x01",
		filepath.Join(efivars, "SetupMode-8be4df61-93ca-11d2-aa0d-00e098032b8c"):  "\x06\x00\x00\x00\x00",
		filepath.Join(pcrs, "4"):  strings.ToUpper(pcrA) + "\n",
		filepath.Join(pcrs, "7"):  pcrB + "\n",
		filepath.Join(pcrs, "12"): "not a pcr\n",
	}

	for path, content := range files {
		err := os.WriteFile(path, []byte(content), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	got := ReadMeasurements(efivars, pcrs)

	want := Measurements{SecureBootEnabled: true, PCR4: pcrA, PCR7: pcrB}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestDiagnoseSeal(t *testing.T) {
	t.Parallel()

	sbOff := &SealState{Version: 1, UKIVersion: "202611141118", PCR4: pcrA, PCR7: pcrB, PCR12: pcrC}
	sbOn := &SealState{Version: 1, SecureBootEnabled: true, PCR4: pcrA, PCR7: pcrB, PCR12: pcrC}
	setup := &SealState{Version: 1, SetupMode: true, PCR4: pcrA, PCR7: pcrB, PCR12: pcrC}
	same := Measurements{PCR4: pcrA, PCR7: pcrB, PCR12: pcrC}

	with := func(m Measurements, change func(*Measurements)) Measurements {
		change(&m)

		return m
	}

	tests := []struct {
		name    string
		record  *SealState
		current Measurements
		cause   SealCause
		wayBack string
		values  []string
	}{
		{"record missing", nil, same, SealRecordMissing, "If asked, type the recovery passphrase (8 words) or older recovery key (dashes)", nil},
		{"secure boot enabled to setup mode", sbOn, with(same, func(m *Measurements) { m.SetupMode = true }), SealSecureBootState, "Way back: set the firmware back to Secure Boot on, then boot again.", []string{"Sealed with Secure Boot on, now Secure Boot in Setup Mode."}},
		{"setup mode to secure boot enabled", setup, with(same, func(m *Measurements) { m.SecureBootEnabled = true }), SealSecureBootState, "Way back: set the firmware back to Secure Boot in Setup Mode, then boot again.", []string{"Sealed with Secure Boot in Setup Mode, now Secure Boot on."}},
		{"secure boot disabled to enabled", sbOff, with(same, func(m *Measurements) { m.SecureBootEnabled = true }), SealSecureBootState, "Way back: set the firmware back to Secure Boot off, then boot again.", []string{"Sealed with Secure Boot off, now Secure Boot on."}},
		{"pcr7 with the same secure boot state", sbOn, with(same, func(m *Measurements) { m.SecureBootEnabled = true; m.PCR7 = pcrA }), SealSecureBootKeys, "Way back: restore the firmware's Secure Boot keys, then boot again.", []string{"PCR7 sealed bbbbbbbbbbbbbbbb..., now aaaaaaaaaaaaaaaa..."}},
		{"pcr4", sbOff, with(same, func(m *Measurements) { m.PCR4 = pcrC }), SealImage, "Way back: pick version 202611141118 in the boot menu, then boot again.", []string{"PCR4 sealed aaaaaaaaaaaaaaaa..., now cccccccccccccccc..."}},
		{"pcr12", sbOff, with(same, func(m *Measurements) { m.PCR12 = pcrA }), SealBootParameters, "Way back: drop the extra command line, credentials or add-ons, then boot again.", []string{"PCR12 sealed cccccccccccccccc..., now aaaaaaaaaaaaaaaa..."}},
		{"all equal", sbOff, same, SealMatch, "", nil},
		{"all equal with secure boot", sbOn, with(same, func(m *Measurements) { m.SecureBootEnabled = true }), SealMatch, "", nil},
		{"pcr4 and pcr12 aren't bound with secure boot", sbOn, Measurements{SecureBootEnabled: true, PCR4: pcrC, PCR7: pcrB, PCR12: pcrA}, SealMatch, "", nil},
		{"an unreadable pcr is not a change", sbOff, Measurements{PCR7: pcrB}, SealMatch, "", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cause := DiagnoseSeal(tt.record, tt.current)
			if cause != tt.cause {
				t.Fatalf("cause %d, want %d", cause, tt.cause)
			}

			lines := SealMessage(cause, tt.record, tt.current)
			text := strings.Join(lines, "\n")

			if cause == SealMatch {
				if len(lines) != 0 {
					t.Fatalf("a match must print nothing, got %q", text)
				}

				return
			}

			// An 80-column console must not wrap a line.
			for _, line := range lines {
				if len(line) > 79 {
					t.Fatalf("line longer than 79 characters: %q", line)
				}
			}

			wants := append([]string{tt.wayBack}, tt.values...)

			if cause == SealRecordMissing && len(lines) != 1 {
				t.Fatalf("record missing: %d lines, want one", len(lines))
			}

			if cause != SealRecordMissing {
				if len(lines) != 5 {
					t.Fatalf("%d lines, want 5:\n%s", len(lines), text)
				}

				wants = append(wants, "Or type the recovery passphrase (8 words) or older recovery key (dashes) below.", "That re-seals the TPM to this boot: type it only if you made the change.")
			}

			for _, want := range wants {
				if !strings.Contains(text, want) {
					t.Fatalf("message lacks %q:\n%s", want, text)
				}
			}

			// No full measurement, let alone a secret, reaches the console.
			for _, full := range []string{pcrA, pcrB, pcrC} {
				if strings.Contains(text, full) {
					t.Fatalf("message carries a full PCR value:\n%s", text)
				}
			}
		})
	}
}
