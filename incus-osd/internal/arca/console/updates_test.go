package console

import (
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
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
