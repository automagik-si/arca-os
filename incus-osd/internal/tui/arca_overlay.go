package tui

import (
	"sync"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// arcaPage is the name of the overlay page on the TUI's pages.
const arcaPage = "arca"

// arcaOverlay is the overlay state of the singleton TUI.
var arcaOverlay struct {
	mu      sync.Mutex
	enabled bool
}

// ShowArcaOverlay puts root on top of the console as the overlay page and gives focus to focus.
func (t *TUI) ShowArcaOverlay(root tview.Primitive, focus tview.Primitive) {
	t.app.QueueUpdateDraw(func() {
		t.pages.AddPage(arcaPage, root, true, true)
		t.app.SetFocus(focus)
	})
}

// HideArcaOverlay removes the overlay page; the screen underneath shows again.
func (t *TUI) HideArcaOverlay() {
	t.app.QueueUpdateDraw(func() {
		t.pages.RemovePage(arcaPage)
	})
}

// EnableArcaOverlay installs an input capture that swallows Ctrl-C, which would
// otherwise stop the TUI for good, and passes every other key on. It is idempotent.
func (t *TUI) EnableArcaOverlay() {
	arcaOverlay.mu.Lock()
	defer arcaOverlay.mu.Unlock()

	if arcaOverlay.enabled {
		return
	}

	arcaOverlay.enabled = true
	prev := t.app.GetInputCapture()

	t.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyCtrlC {
			return nil
		}

		if prev != nil {
			return prev(event)
		}

		return event
	})
}

// QueueArcaUpdate runs f on the TUI's event loop and redraws the screen.
func (t *TUI) QueueArcaUpdate(f func()) {
	t.app.QueueUpdateDraw(f)
}
