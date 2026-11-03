package tui

import (
	"log/slog"
	"sync"

	"github.com/gdamore/tcell/v2"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/rivo/tview"

	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// Upstream's page names on the TUI's pages.
const (
	framePage = "frame"
	modalPage = "modal"
)

// arcaConsole is the console that replaces the content of upstream's frame page.
type arcaConsole struct {
	mu      sync.Mutex
	root    tview.Primitive
	focus   tview.Primitive
	enabled bool
	hooked  bool // the L key and the refocus after a modal are installed
	calls   int  // redrawScreen calls so far, counted as upstream's redraw loop counts them
	onClear func()
}

// arcaConsoles holds each TUI's console; the daemon has one TUI, tests have several.
var arcaConsoles sync.Map

func (t *TUI) arcaConsole() *arcaConsole {
	if c, ok := arcaConsoles.Load(t); ok {
		return c.(*arcaConsole) //nolint:forcetypeassert
	}

	c, _ := arcaConsoles.LoadOrStore(t, &arcaConsole{})

	return c.(*arcaConsole) //nolint:forcetypeassert
}

// SetArcaConsole makes root the content of the frame page in place of upstream's
// header, log and footer, and enables it. Focus moves to focus once, and only when
// neither the overlay nor a modal is in front, so the Wi-Fi picker keeps it. It must
// not be called on the TUI's event loop.
func (t *TUI) SetArcaConsole(root tview.Primitive, focus tview.Primitive) {
	c := t.arcaConsole()

	c.mu.Lock()
	c.root, c.focus, c.enabled = root, focus, true
	hook := !c.hooked
	c.hooked = true
	c.mu.Unlock()

	if hook {
		t.arcaConsoleHooks()
	}

	t.app.QueueUpdateDraw(func() {
		if t.ArcaConsoleEnabled() {
			t.arcaConsoleShow(root)
			t.arcaConsoleFocus(focus)
		}
	})
}

// DisableArcaConsole turns the console off; upstream's screen shows again, its
// header and footer at the next periodic redraw. It returns at once and may be
// called from any goroutine, the event loop included.
func (t *TUI) DisableArcaConsole() {
	c := t.arcaConsole()

	c.mu.Lock()
	was := c.enabled && c.root != nil
	c.enabled = false
	c.mu.Unlock()

	if !was {
		return
	}

	go t.app.QueueUpdateDraw(func() {
		if !t.ArcaConsoleEnabled() {
			t.frame.SetPrimitive(t.textView)
		}
	})
}

// EnableArcaConsole turns a console set by SetArcaConsole back on, with the same
// focus rule. It must not be called on the TUI's event loop.
func (t *TUI) EnableArcaConsole() {
	c := t.arcaConsole()

	c.mu.Lock()
	if c.root == nil {
		c.mu.Unlock()

		return
	}

	c.enabled = true
	root, focus := c.root, c.focus
	c.mu.Unlock()

	t.app.QueueUpdateDraw(func() {
		if t.ArcaConsoleEnabled() {
			t.arcaConsoleShow(root)
			t.arcaConsoleFocus(focus)
		}
	})
}

// ArcaConsoleEnabled reports whether a console is set and enabled.
func (t *TUI) ArcaConsoleEnabled() bool {
	c := t.arcaConsole()

	c.mu.Lock()
	defer c.mu.Unlock()

	return c.root != nil && c.enabled
}

// OnArcaConsoleClear sets f to run on the redraw right after upstream's minute
// clear of the console devices, whether the console is enabled or not. It runs on
// the redraw goroutine, not on the event loop.
func (t *TUI) OnArcaConsoleClear(f func()) {
	c := t.arcaConsole()

	c.mu.Lock()
	c.onClear = f
	c.mu.Unlock()
}

// arcaConsoleFrame runs first in redrawScreen. While a console is set and enabled
// it keeps the console as the frame's only content and returns true, so upstream's
// header, log and footer are not drawn; otherwise it returns false. It counts its
// calls as upstream's redraw loop does: call i%12 == 1 comes right after upstream
// cleared the console devices, so it runs the clear callback, and with the console
// on, a full resync of the screen. It never sets focus.
func (t *TUI) arcaConsoleFrame() bool {
	if t.frame == nil {
		return false
	}

	c := t.arcaConsole()

	c.mu.Lock()
	i := c.calls
	c.calls++
	root, on, onClear := c.root, c.root != nil && c.enabled, c.onClear
	c.mu.Unlock()

	cleared := i%12 == 1
	if cleared && onClear != nil {
		onClear()
	}

	if !on {
		return false
	}

	t.app.QueueUpdateDraw(func() {
		if t.ArcaConsoleEnabled() {
			t.arcaConsoleShow(root)
		}
	})

	if cleared {
		t.app.Sync()
	}

	return true
}

// arcaConsoleShow makes root the frame's only content. It runs on the event loop.
func (t *TUI) arcaConsoleShow(root tview.Primitive) {
	t.frame.Clear()
	t.frame.SetPrimitive(root)
}

// arcaConsoleFocus gives focus to focus when the frame page is in front, that is
// neither the overlay nor a modal. It runs on the event loop.
func (t *TUI) arcaConsoleFocus(focus tview.Primitive) {
	if name, _ := t.pages.GetFrontPage(); name == framePage && focus != nil {
		t.app.SetFocus(focus)
	}
}

// arcaConsoleHooks installs the L key, which turns the console off and on while no
// overlay page exists (with the overlay, L is the picker's), and the refocus of the
// console once the frame page is in front again, after a modal or the overlay.
func (t *TUI) arcaConsoleHooks() {
	// The overlay's Ctrl-C capture is installed under the same lock, so neither is lost.
	arcaOverlay.mu.Lock()
	prev := t.app.GetInputCapture()
	t.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyRune && event.Rune() == 'L' && !t.pages.HasPage(arcaPage) && t.arcaConsoleToggle() {
			return nil
		}

		if prev != nil {
			return prev(event)
		}

		return event
	})
	arcaOverlay.mu.Unlock()

	// Pages calls this from whichever goroutine adds or removes a page, after the change.
	t.pages.SetChangedFunc(func() {
		if name, _ := t.pages.GetFrontPage(); name != framePage {
			return
		}

		c := t.arcaConsole()

		c.mu.Lock()
		focus, on := c.focus, c.root != nil && c.enabled
		c.mu.Unlock()

		if on && focus != nil && !focus.HasFocus() {
			t.app.SetFocus(focus)
		}
	})
}

// arcaConsoleToggle turns a set console off or on from the L key and reports
// whether one is set. It runs on the event loop, so it changes the frame directly
// (a queued update would wait on the event loop itself), and it logs from another
// goroutine, so the screen never waits on the journal.
func (t *TUI) arcaConsoleToggle() bool {
	c := t.arcaConsole()

	c.mu.Lock()
	if c.root == nil {
		c.mu.Unlock()

		return false
	}

	c.enabled = !c.enabled
	root, focus, on := c.root, c.focus, c.enabled
	c.mu.Unlock()

	if on {
		t.arcaConsoleShow(root)
		t.arcaConsoleFocus(focus)

		go slog.Info("Console: enabled by key")
	} else {
		t.frame.SetPrimitive(t.textView)

		go slog.Info("Console: disabled by key")
	}

	return true
}

// NewArcaTestTUI returns a TUI drawn on screen, for the tests of this package and
// of the Arca console. It is built as GetTUI builds the daemon's TUI, but it opens
// no console device and reads no hardware: its machine is one "Example CPU". Its
// event loop runs until stop is called. Run is not called, so nothing clears a
// console device and no periodic redraw runs; redraw runs one pass of it.
func NewArcaTestTUI(s *state.State, screen tcell.Screen) (t *TUI, redraw func(), stop func()) {
	t = &TUI{
		state:  s,
		screen: screen,
		systemResources: &api.Resources{
			CPU: api.ResourcesCPU{Architecture: "x86_64", Sockets: []api.ResourcesCPUSocket{{Name: "Example CPU"}}, Total: 1},
		},
	}

	t.textView = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(false).
		SetWordWrap(true).
		SetChangedFunc(func() {
			t.app.Draw()
		})
	t.textView.SetBorder(true)

	t.frame = tview.NewFrame(nil).SetBorders(0, 0, 1, 1, 0, 0)
	t.pages = tview.NewPages().AddPage(framePage, t.frame, true, true)
	t.app = tview.NewApplication().SetScreen(screen).SetRoot(t.pages, true)

	done := make(chan struct{})

	go func() {
		_ = t.app.Run()

		close(done)
	}()

	return t, t.redrawScreen, func() {
		t.app.Stop()
		<-done
		arcaConsoles.Delete(t)
	}
}
