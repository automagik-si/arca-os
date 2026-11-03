package console

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/tui"
)

// fakeHost is a console host whose redraws run at once.
type fakeHost struct {
	enabled bool
}

func (f *fakeHost) EnableArcaOverlay()                                         {}
func (f *fakeHost) SetArcaConsole(root tview.Primitive, focus tview.Primitive) { f.enabled = true }
func (f *fakeHost) DisableArcaConsole()                                        { f.enabled = false }
func (f *fakeHost) ArcaConsoleEnabled() bool                                   { return f.enabled }
func (f *fakeHost) QueueArcaUpdate(fn func())                                  { fn() }
func (f *fakeHost) ArcaLogView() *tview.TextView                               { return nil }
func (f *fakeHost) ArcaLastLog() string                                        { return "" }

// keyOf types one key on the console.
func keyOf(c *console, key tcell.Key, r rune) *tcell.EventKey {
	return c.key(tcell.NewEventKey(key, r, tcell.ModNone))
}

// selected reads the console's selection.
func selected(c *console) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.snap.Selected
}

// pageOf reads the console's page.
func pageOf(c *console) page {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.snap.Page
}

// focusOf reads the console's focused panel.
func focusOf(c *console) panel {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.snap.Focus
}

func TestKeysNavigate(t *testing.T) {
	c := newConsole(&fakeHost{}, staticSources())
	c.snap.Instances = testRows()
	c.snap.Selected = 0

	if ev := keyOf(c, tcell.KeyDown, 0); ev != nil {
		t.Error("down was passed on")
	}

	if got := selected(c); got != 1 {
		t.Errorf("after down the selection is %d, want 1", got)
	}

	keyOf(c, tcell.KeyRune, 'j')
	keyOf(c, tcell.KeyRune, 'j')

	if got := selected(c); got != 3 {
		t.Errorf("after two j the selection is %d, want 3", got)
	}

	keyOf(c, tcell.KeyDown, 0)

	if got := selected(c); got != 3 {
		t.Errorf("down past the last row is %d, want 3", got)
	}

	keyOf(c, tcell.KeyRune, 'k')

	if got := selected(c); got != 2 {
		t.Errorf("after k the selection is %d, want 2", got)
	}

	keyOf(c, tcell.KeyPgUp, 0)

	if got := selected(c); got != 0 {
		t.Errorf("PgUp is %d, want 0", got)
	}

	keyOf(c, tcell.KeyPgDn, 0)

	if got := selected(c); got != 3 {
		t.Errorf("PgDn is %d, want 3", got)
	}

	// Enter opens the details page, Esc closes it.
	keyOf(c, tcell.KeyEnter, 0)

	if got := pageOf(c); got != pageDetails {
		t.Errorf("Enter opened page %v, want details", got)
	}

	// Movement is refused while a page is in front.
	keyOf(c, tcell.KeyDown, 0)

	if got := selected(c); got != 3 {
		t.Errorf("a key moved the selection under a page: %d", got)
	}

	keyOf(c, tcell.KeyEsc, 0)

	if got := pageOf(c); got != pageNone {
		t.Errorf("Esc left page %v", got)
	}

	// ? opens help, and ? or Esc closes it.
	keyOf(c, tcell.KeyRune, '?')

	if got := pageOf(c); got != pageHelp {
		t.Errorf("? opened page %v, want help", got)
	}

	keyOf(c, tcell.KeyRune, '?')

	if got := pageOf(c); got != pageNone {
		t.Errorf("? did not close help: %v", got)
	}

	// Tab moves the focus from the instances to the events, and back.
	keyOf(c, tcell.KeyTab, 0)

	if got := focusOf(c); got != panelEvents {
		t.Errorf("Tab focused panel %v, want events", got)
	}

	keyOf(c, tcell.KeyTab, 0)

	if got := focusOf(c); got != panelInstances {
		t.Errorf("a second Tab focused panel %v, want instances", got)
	}

	keyOf(c, tcell.KeyBacktab, 0)

	if got := focusOf(c); got != panelInstances {
		t.Errorf("Shift-Tab from the instances is panel %v", got)
	}

	// The help page is drawn from the same snapshot.
	snap := tracerSnapshot()
	snap.Page = pageHelp
	golden(t, "help-80x25.txt", draw(t, snap, 80, 25))
}

// waitFor fails after five seconds unless cond holds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("the condition never held")
}

func TestToggleUpstream(t *testing.T) {
	s := &state.State{}
	s.OS.Name = "Example OS"
	s.OS.RunningRelease = "202611030000"

	screen := tcell.NewSimulationScreen("UTF-8")
	tt, redraw, stop := tui.NewArcaTestTUI(s, screen)
	t.Cleanup(stop)
	screen.SetSize(80, 25)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c := newConsole(tt, staticSources())
	c.begin(ctx)

	waitFor(t, tt.ArcaConsoleEnabled)

	// L shows upstream's screen and logs it.
	screen.InjectKey(tcell.KeyRune, 'l', tcell.ModNone)
	waitFor(t, func() bool { return !tt.ArcaConsoleEnabled() })

	redraw()

	var text string

	tt.QueueArcaUpdate(func() { text = screenText(screen) })

	if !strings.Contains(text, "Example OS 202611030000") {
		t.Errorf("upstream's header is not on the screen after L:\n%s", text)
	}

	if !strings.Contains(text, "Network configuration:") {
		t.Errorf("upstream's footer is not on the screen after L:\n%s", text)
	}

	// L brings the console back.
	screen.InjectKey(tcell.KeyRune, 'l', tcell.ModNone)
	waitFor(t, tt.ArcaConsoleEnabled)

	tt.QueueArcaUpdate(func() { text = screenText(screen) })

	if strings.Contains(text, "Network configuration:") {
		t.Errorf("upstream's footer is still on the screen after L:\n%s", text)
	}
}
