package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/lxc/incus-os/incus-osd/internal/state"
)

const (
	consoleWidth  = 80
	consoleHeight = 25
	upstreamTitle = "Example OS 202611030000"
)

// newConsoleTUI returns a running test TUI on an 80x25 simulated screen.
func newConsoleTUI(t *testing.T) (*TUI, tcell.SimulationScreen, func()) {
	t.Helper()

	s := &state.State{}
	s.OS.Name = "Example OS"
	s.OS.RunningRelease = "202611030000"

	screen := tcell.NewSimulationScreen("UTF-8")
	tt, redraw, stop := NewArcaTestTUI(s, screen)
	t.Cleanup(stop)

	screen.SetSize(consoleWidth, consoleHeight)

	return tt, screen, redraw
}

// rows returns the screen's rows, read on the event loop between two draws.
func rows(t *TUI, screen tcell.SimulationScreen) []string {
	out := make([]string, consoleHeight)

	t.app.QueueUpdate(func() {
		for y := range consoleHeight {
			var b strings.Builder

			for x := range consoleWidth {
				str, _, _ := screen.Get(x, y)
				b.WriteString(str)
			}

			out[y] = strings.TrimRight(b.String(), " ")
		}
	})

	return out
}

// onLoop returns f's result, computed on the event loop.
func onLoop[T any](t *TUI, f func() T) T {
	var v T

	t.app.QueueUpdate(func() { v = f() })

	return v
}

// markBox draws top on its first row and bottom on its last row.
type markBox struct {
	*tview.Box

	top, bottom string
}

func (m *markBox) Draw(screen tcell.Screen) {
	x, y, w, h := m.GetRect()
	tview.Print(screen, m.top, x, y, w, tview.AlignLeft, tcell.ColorWhite)
	tview.Print(screen, m.bottom, x, y+h-1, w, tview.AlignLeft, tcell.ColorWhite)
}

// keyBox records the runes of the keys it receives.
type keyBox struct {
	*tview.Box

	keys chan rune
}

func newKeyBox() *keyBox {
	return &keyBox{Box: tview.NewBox(), keys: make(chan rune, 16)}
}

func (k *keyBox) InputHandler() func(event *tcell.EventKey, setFocus func(p tview.Primitive)) {
	return k.WrapInputHandler(func(event *tcell.EventKey, _ func(p tview.Primitive)) {
		k.keys <- event.Rune()
	})
}

// expectKey types r and fails unless want receives it.
func expectKey(t *testing.T, screen tcell.SimulationScreen, r rune, want *keyBox, name string) {
	t.Helper()

	screen.InjectKey(tcell.KeyRune, r, tcell.ModNone)

	select {
	case got := <-want.keys:
		if got != r {
			t.Fatalf("%s received %q, want %q", name, got, r)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%q never reached %s", r, name)
	}
}

func TestArcaConsoleFrame(t *testing.T) {
	t.Parallel()

	tt, screen, redraw := newConsoleTUI(t)

	root := &markBox{Box: tview.NewBox(), top: "CONSOLE TOP", bottom: "CONSOLE BOTTOM"}
	tt.SetArcaConsole(root, root)
	redraw()

	if !tt.ArcaConsoleEnabled() {
		t.Fatal("the console is not enabled after SetArcaConsole")
	}

	if got := onLoop(tt, tt.frame.GetPrimitive); got != root {
		t.Fatalf("the frame's primitive is %T, not the console root", got)
	}

	lines := rows(tt, screen)
	if lines[0] != "CONSOLE TOP" {
		t.Errorf("row 1 is %q: a header is drawn above the console", lines[0])
	}

	if lines[consoleHeight-1] != "CONSOLE BOTTOM" {
		t.Errorf("the last row is %q: a footer is drawn below the console", lines[consoleHeight-1])
	}

	for y, line := range lines {
		if strings.Contains(line, upstreamTitle) || strings.Contains(line, "Network configuration") {
			t.Errorf("row %d shows upstream's frame: %q", y+1, line)
		}
	}
}

func TestArcaConsoleDisable(t *testing.T) {
	t.Parallel()

	tt, screen, redraw := newConsoleTUI(t)

	root := &markBox{Box: tview.NewBox(), top: "CONSOLE TOP", bottom: "CONSOLE BOTTOM"}
	tt.SetArcaConsole(root, root)
	redraw()

	upstream := func(when string) {
		t.Helper()

		lines := rows(tt, screen)
		if !strings.Contains(lines[0], upstreamTitle) {
			t.Errorf("%s: row 1 is %q, not upstream's header", when, lines[0])
		}

		if !strings.Contains(strings.Join(lines, "\n"), "Network configuration:") {
			t.Errorf("%s: upstream's footer is not on the screen", when)
		}

		if got := onLoop(tt, tt.frame.GetPrimitive); got != tt.textView {
			t.Errorf("%s: the frame's primitive is %T, not upstream's log view", when, got)
		}
	}

	tt.DisableArcaConsole()

	if tt.ArcaConsoleEnabled() {
		t.Fatal("the console is still enabled after DisableArcaConsole")
	}

	redraw()
	upstream("after DisableArcaConsole")

	tt.EnableArcaConsole()
	redraw()

	if lines := rows(tt, screen); lines[0] != "CONSOLE TOP" {
		t.Errorf("after EnableArcaConsole row 1 is %q", lines[0])
	}

	// L, with no overlay, turns the console off and on again.
	screen.InjectKey(tcell.KeyRune, 'L', tcell.ModNone)

	deadline := time.Now().Add(5 * time.Second)
	for tt.ArcaConsoleEnabled() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if tt.ArcaConsoleEnabled() {
		t.Fatal("L did not turn the console off")
	}

	redraw()
	upstream("after L")

	screen.InjectKey(tcell.KeyRune, 'L', tcell.ModNone)

	deadline = time.Now().Add(5 * time.Second)
	for !tt.ArcaConsoleEnabled() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	redraw()

	if lines := rows(tt, screen); lines[0] != "CONSOLE TOP" {
		t.Errorf("after a second L row 1 is %q", lines[0])
	}
}

func TestFocusAfterModal(t *testing.T) {
	t.Parallel()

	tt, screen, redraw := newConsoleTUI(t)

	// As at boot: upstream's frame is drawn, then a modal shows before the console starts.
	redraw()
	tt.renderModal("Example modal", "Working", 0)

	console := newKeyBox()
	tt.SetArcaConsole(tview.NewFlex().AddItem(console, 0, 1, true), console)

	if onLoop(tt, console.HasFocus) {
		t.Fatal("the console took focus while a modal was in front")
	}

	// Upstream's modal loop removes a finished modal from its own goroutine.
	tt.pages.RemovePage(modalPage)
	expectKey(t, screen, 'k', console, "the console after the first modal")

	// A modal that comes and goes while the console has focus.
	tt.renderModal("Example modal", "Working", 0)

	if onLoop(tt, console.HasFocus) {
		t.Fatal("the console kept focus under a modal")
	}

	tt.pages.RemovePage(modalPage)
	expectKey(t, screen, 'j', console, "the console after the second modal")
}

func TestFocusNotStolenFromPicker(t *testing.T) {
	t.Parallel()

	tt, screen, redraw := newConsoleTUI(t)

	picker := newKeyBox()
	tt.ShowArcaOverlay(picker, picker)

	pickerFocused := func(when string) {
		t.Helper()

		if got := onLoop(tt, tt.app.GetFocus); got != picker {
			t.Fatalf("%s: focus is on %T, not the picker", when, got)
		}
	}

	pickerFocused("after ShowArcaOverlay")

	console := newKeyBox()
	tt.SetArcaConsole(tview.NewFlex().AddItem(console, 0, 1, true), console)
	pickerFocused("after SetArcaConsole")

	redraw()
	pickerFocused("after a redraw")

	tt.renderModal("Example modal", "Working", 0)
	tt.pages.RemovePage(modalPage)
	pickerFocused("after a modal was removed")

	expectKey(t, screen, 'L', picker, "the picker")

	if !tt.ArcaConsoleEnabled() {
		t.Fatal("L under the picker turned the console off")
	}

	select {
	case r := <-console.keys:
		t.Fatalf("the console received %q while the picker was in front", r)
	default:
	}
}
