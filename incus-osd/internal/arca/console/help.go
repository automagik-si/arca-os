package console

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// helpLines is the help page's text: the keys, and what the console never does.
func helpLines() []string {
	return []string{
		"↑ ↓ j k     select an instance",
		"PgUp PgDn   first / last instance",
		"Tab         next panel (instances, events)",
		"Enter       details of the selected instance",
		"Esc         close this page",
		"U           updates: o on / off, [ ] restart window,",
		"            r try a failed version again, c check now",
		"?           this page",
		"L           upstream's screen, and back",
		"",
		"Ctrl-C is held by the console.",
		"Only the updates page changes this machine; other",
		"changes are made from the fleet with the owner's approval.",
	}
}

// drawHelp draws the help page in r.
func drawHelp(screen tcell.Screen, r rect) {
	box(screen, r, "Help")

	x, y, w, h := r.X+1, r.Y+1, r.W-2, r.H-2
	if w <= 0 || h <= 0 {
		return
	}

	for i, line := range helpLines() {
		if i >= h {
			break
		}

		tview.Print(screen, " "+safe(line), x, y+i, w, tview.AlignLeft, tcell.ColorWhite)
	}
}
