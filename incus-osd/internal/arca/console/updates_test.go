package console

import (
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/lxc/incus-os/incus-osd/internal/state"
	osupdate "github.com/lxc/incus-os/incus-osd/internal/update"
)

// fakeActions records the owner's actions and changes the fake updater like the real one.
type fakeActions struct {
	mu    sync.Mutex
	u     *Updates
	calls []string
}

func (f *fakeActions) SetAutomatic(on bool) {
	f.record("automatic", func(u *Updates) {
		u.Automatic = on
		if on {
			u.NeedsOwner = ""
		}
	})
}

func (f *fakeActions) ShiftWindow(hours int) {
	f.record("window", func(u *Updates) {
		if hours > 0 {
			u.Window = "04:00-06:00"
		}
	})
}

func (f *fakeActions) Retry(version string) {
	f.record("retry "+version, func(u *Updates) { u.Failed, u.FailedIDs = nil, nil })
}

func (f *fakeActions) CheckNow() { f.record("check", func(*Updates) {}) }

func (f *fakeActions) callsSoFar() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.calls...)
}

func (f *fakeActions) record(call string, change func(u *Updates)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, call)
	change(f.u)
}

func needsYou() Updates {
	return Updates{
		Automatic:  false,
		Window:     "03:00-05:00",
		Status:     "Needs you: after going back to 202611010000, 3 health checks failed",
		NextWindow: time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC),
		LastCheck:  time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC),
		Current:    "202611010000",
		Previous:   "202611010000",
		Good:       "202611010000",
		Failed:     []string{"202611020000: 3 health checks failed, last: the Incus API does not answer"},
		FailedIDs:  []string{"202611020000"},
		LastResult: "Needs you: after going back to 202611010000, 3 health checks failed",
		NeedsOwner: "after going back to 202611010000, 3 health checks failed",
	}
}

func TestUpdatesPage(t *testing.T) {
	t.Parallel()

	u := needsYou()
	act := &fakeActions{u: &u}

	src := staticSources()
	src.updates = func() Updates {
		act.mu.Lock()
		defer act.mu.Unlock()

		return u.clone()
	}
	src.actions = act

	c := newConsole(&fakeHost{}, src)
	c.readUpdates()

	// The dashboard's alert line says the owner is needed.
	snap := c.snap.clone()

	screen := draw(t, snap, 80, 25)
	if row := rowOf(screen, "✗ Needs you: after going back"); row < 0 {
		t.Fatalf("the alert line doesn't ask for the owner:\n%s", screenText(screen))
	}

	// Actions are refused while the page is closed.
	keyOf(c, tcell.KeyRune, 'o')

	if len(act.callsSoFar()) != 0 {
		t.Fatal("o acted outside the updates page")
	}

	keyOf(c, tcell.KeyRune, 'U')

	if pageOf(c) != pageUpdates {
		t.Fatal("U did not open the updates page")
	}

	c.mu.Lock()
	snap = c.snap.clone()
	c.mu.Unlock()
	golden(t, "updates-needs-you-80x25.txt", draw(t, snap, 80, 25))

	keyOf(c, tcell.KeyRune, 'r')
	waitFor(t, func() bool { return len(act.callsSoFar()) == 1 })
	keyOf(c, tcell.KeyRune, ']')
	waitFor(t, func() bool { return len(act.callsSoFar()) == 2 })
	keyOf(c, tcell.KeyRune, 'c')
	waitFor(t, func() bool { return len(act.callsSoFar()) == 3 })
	keyOf(c, tcell.KeyRune, 'o')
	waitFor(t, func() bool { return len(act.callsSoFar()) == 4 })

	want := []string{"retry 202611020000", "window", "check", "automatic"}
	if got := act.callsSoFar(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Fatalf("calls: got %v, want %v", got, want)
	}

	// The page shows the result: on, the new window and no failed version.
	waitFor(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()

		return c.snap.Updates.Automatic && c.snap.Updates.NeedsOwner == "" && c.snap.Updates.Window == "04:00-06:00"
	})

	c.mu.Lock()
	snap = c.snap.clone()
	c.mu.Unlock()

	screen = draw(t, snap, 80, 25)
	if rowOf(screen, "Automatic   On") < 0 || rowOf(screen, "Restarts    04:00-06:00") < 0 || rowOf(screen, "None") < 0 {
		t.Fatalf("the page doesn't show the owner's changes:\n%s", screenText(screen))
	}

	keyOf(c, tcell.KeyEsc, 0)

	if pageOf(c) != pageNone {
		t.Fatal("Esc did not close the updates page")
	}
}

// untrusted is an updater whose update server's key isn't trusted, after three checks.
func untrusted() Updates {
	return Updates{
		Automatic:       true,
		Window:          "03:00-05:00",
		Status:          osupdate.TrustStatus,
		LastCheck:       time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC),
		Current:         "202611010000",
		Available:       "202611030000",
		UpdateAvailable: true,
		Good:            "202611010000",
		ErrorKind:       osupdate.ErrorKindTrust,
		LastError:       "unable to verify S/MIME message due to its use of a missing or unverifiable CA",
		EmbeddedCA:      "70:79:C3:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:01",
		PresentedCA:     "0E:4A:16:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:02",
		TrustFailures:   3,
		TrustEscalated:  true,
	}
}

func TestUpdatesPageUntrusted(t *testing.T) {
	t.Parallel()

	u := untrusted()

	src := staticSources()
	src.updates = u.clone
	src.actions = &fakeActions{u: &u}

	c := newConsole(&fakeHost{}, src)
	c.readUpdates()

	// The alert line warns once three checks failed.
	screen := draw(t, c.snap.clone(), 80, 30)
	if rowOf(screen, "✗ No updates for 3 checks: the server's signing key is not trusted") < 0 {
		t.Fatalf("the alert line doesn't warn about the update server:\n%s", screenText(screen))
	}

	keyOf(c, tcell.KeyRune, 'U')

	c.mu.Lock()
	snap := c.snap.clone()
	c.mu.Unlock()

	screen = draw(t, snap, 80, 30)
	golden(t, "updates-untrusted-80x30.txt", screen)

	for _, want := range []string{
		"Status      " + osupdate.TrustStatus,
		"Embedded CA 70:79:C3:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:",
		"            DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:01",
		"Server CA   0E:4A:16:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:",
		"            DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:02",
		"Error       unable to verify S/MIME message due to its use of a missing",
		"Available   202611030000   Behind the channel (the last check failed)",
	} {
		if rowOf(screen, want) < 0 {
			t.Fatalf("the page has no %q:\n%s", want, screenText(screen))
		}
	}

	// The failure is in the colour of failures.
	for _, sub := range []string{osupdate.TrustStatus, "70:79:C3", "0E:4A:16"} {
		fg, _, _ := cellStyle(t, screen, rowOf(screen, sub), sub)
		if fg != tcell.ColorRed {
			t.Fatalf("%q is %v, not red", sub, fg)
		}
	}
}

func TestUpdatesTrustBeforeEscalation(t *testing.T) {
	t.Parallel()

	u := untrusted()
	u.TrustFailures = 1
	u.TrustEscalated = false

	// The page shows the mismatch from the first failure, without the alert.
	tag, text := updatesAlert(u)
	if tag != "" || text != "" {
		t.Fatalf("alert before three failures: %q", text)
	}

	page := strings.Join(updatesLines(u, 0), "\n")
	for _, want := range []string{osupdate.TrustStatus, "70:79:C3", "0E:4A:16"} {
		if !strings.Contains(page, want) {
			t.Fatalf("the page has no %q:\n%s", want, page)
		}
	}

	// Another failure kind shows no fingerprints.
	u.ErrorKind = osupdate.ErrorKindNetwork
	u.Status = "Failed to check for OS updates"

	page = strings.Join(updatesLines(u, 0), "\n")
	if strings.Contains(page, "Embedded CA") {
		t.Fatalf("fingerprints shown for a network failure:\n%s", page)
	}
}

// styleTag is a console style tag such as [gray] or [-:-:-].
var styleTag = regexp.MustCompile(`\[[a-z:-]*\]`)

func stripTags(text string) string {
	return styleTag.ReplaceAllString(text, "")
}

func TestUpdatesAvailableVersion(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		u    Updates
		want string
	}{
		{"unknown", Updates{Current: "202611010000"}, "Available   —"},
		{"behind", Updates{Current: "202611010000", Available: "202611150123", UpdateAvailable: true}, "Available   202611150123   Behind the channel"},
		{"latest", Updates{Current: "202611150123", Available: "202611150123"}, "Available   202611150123   Up to date"},
		{"restart", Updates{Current: "202611010000", Pending: "202611150123", Available: "202611150123", UpdateAvailable: true}, "Available   202611150123   Waiting for a restart"},
		{"held", Updates{Current: "202611010000", Available: "202611150123"}, "Available   202611150123   No update to apply"},
		{"stale", Updates{Current: "202611010000", Available: "202611150123", UpdateAvailable: true, ErrorKind: "network"}, "Available   202611150123   Behind the channel (the last check failed)"},
	} {
		page := stripTags(strings.Join(updatesLines(tc.u, 0), "\n"))
		if !strings.Contains(page, tc.want+"\n") {
			t.Fatalf("%s: no %q in:\n%s", tc.name, tc.want, page)
		}
	}
}

func TestLiveUpdatesAvailableVersion(t *testing.T) {
	t.Parallel()

	s := &state.State{}
	s.System.Update.State.AvailableVersion = "202611150123"
	s.System.Update.State.UpdateAvailable = true

	u := liveUpdates(s)
	if u.Available != "202611150123" || !u.UpdateAvailable {
		t.Fatalf("live updates: available %q, update available %v", u.Available, u.UpdateAvailable)
	}
}
