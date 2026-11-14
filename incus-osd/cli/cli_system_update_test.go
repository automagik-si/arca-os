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

	health := func(current, pending string) *api.SystemUpdateHealth {
		return &api.SystemUpdateHealth{CurrentVersion: current, PendingVersion: pending}
	}

	note := func(st api.SystemUpdateState) string {
		data, err := json.Marshal(api.SystemUpdate{State: st})
		if err != nil {
			t.Fatal(err)
		}

		var out bytes.Buffer

		printUpdateNote(&out, data)

		return out.String()
	}

	const line = "\nAvailable OS version: 202611150123 (as of the last valid answer from the update server)\n"

	for _, tc := range []struct {
		name string
		st   api.SystemUpdateState
		want string
	}{
		{"behind", api.SystemUpdateState{AvailableVersion: "202611150123", UpdateAvailable: true, Health: health("202611010000", "")}, "  Behind the channel\n"},
		{"restart", api.SystemUpdateState{AvailableVersion: "202611150123", NeedsReboot: true, Health: health("202611010000", "202611150123")}, "  Waiting for a restart\n"},
		{"latest", api.SystemUpdateState{AvailableVersion: "202611150123", Health: health("202611150123", "")}, "  Up to date\n"},
		{"secure boot restart", api.SystemUpdateState{AvailableVersion: "202611150123", NeedsReboot: true, Health: health("202611150123", "")}, "  Up to date\n"},
		{"held", api.SystemUpdateState{AvailableVersion: "202611150123", Health: health("202611010000", "")}, "  No update to apply\n"},
	} {
		out := note(tc.st)
		if !strings.Contains(out, line+tc.want) {
			t.Fatalf("%s: no %q in:\n%s", tc.name, line+tc.want, out)
		}
	}

	// A failed check keeps the version and says the check failed.
	out := note(api.SystemUpdateState{
		LastErrorKind: "network", LastError: "connection refused", AvailableVersion: "202611150123", UpdateAvailable: true,
		Health: health("202611010000", ""),
	})
	if !strings.Contains(out, "Last error (network)") || !strings.Contains(out, line+"  Behind the channel (the last check failed)\n") {
		t.Fatalf("failed check note:\n%s", out)
	}
}
