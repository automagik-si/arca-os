package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lxc/incus-os/incus-osd/api"
)

func TestPrintUpdateNote(t *testing.T) {
	t.Parallel()

	const (
		embedded  = "70:79:C3:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:01"
		presented = "0E:4A:16:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:02"
	)

	note := func(st api.SystemUpdateState) string {
		data, err := json.Marshal(api.SystemUpdate{State: st})
		if err != nil {
			t.Fatal(err)
		}

		var out bytes.Buffer

		printUpdateNote(&out, data)

		return out.String()
	}

	out := note(api.SystemUpdateState{
		Status:                 "Update server key is not trusted by this system",
		LastErrorKind:          "trust",
		LastError:              "unable to verify S/MIME message due to its use of a missing or unverifiable CA",
		EmbeddedCAFingerprint:  embedded,
		PresentedCAFingerprint: presented,
		TrustFailures:          2,
	})

	for _, want := range []string{
		"Update status: Update server key is not trusted by this system\n",
		"  Embedded CA:  " + embedded + "\n",
		"  Presented CA: " + presented + "\n",
		"  Last error (trust): unable to verify S/MIME message",
		"  Consecutive trust failures: 2\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("no %q in:\n%s", want, out)
		}
	}

	// A network failure has no CAs to show.
	out = note(api.SystemUpdateState{Status: "Failed to check for OS updates", LastErrorKind: "network", LastError: "connection refused"})
	if !strings.Contains(out, "Last error (network): connection refused") || strings.Contains(out, "CA:") {
		t.Fatalf("network failure note:\n%s", out)
	}

	// A successful check prints nothing more than today.
	out = note(api.SystemUpdateState{Status: "OS update check completed"})
	if out != "" {
		t.Fatalf("note after a successful check: %q", out)
	}
}

func TestPrintUpdateAvailableVersion(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		st   api.SystemUpdateState
		want string
	}{
		{api.SystemUpdateState{AvailableVersion: "202611150123", UpdateAvailable: true}, "  Behind the channel: an update is available\n"},
		{api.SystemUpdateState{AvailableVersion: "202611150123", UpdateAvailable: true, NeedsReboot: true}, "  An update is waiting for a restart\n"},
		{api.SystemUpdateState{AvailableVersion: "202611150123"}, "  No update to apply\n"},
	} {
		data, err := json.Marshal(api.SystemUpdate{State: tc.st})
		if err != nil {
			t.Fatal(err)
		}

		var out bytes.Buffer

		printUpdateNote(&out, data)

		for _, want := range []string{"\nAvailable OS version: 202611150123 (as of the last successful check)\n", tc.want} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("no %q in:\n%s", want, out.String())
			}
		}
	}

	// A failed check keeps the last version, next to the failure.
	data, err := json.Marshal(api.SystemUpdate{State: api.SystemUpdateState{
		LastErrorKind: "network", LastError: "connection refused", AvailableVersion: "202611150123", UpdateAvailable: true,
	}})
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer

	printUpdateNote(&out, data)

	if !strings.Contains(out.String(), "Last error (network)") || !strings.Contains(out.String(), "Available OS version: 202611150123") {
		t.Fatalf("failed check note:\n%s", out.String())
	}
}
