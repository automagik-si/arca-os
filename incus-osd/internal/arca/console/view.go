package console

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// Style tags of the console's text.
const (
	tagReset = "[-:-:-]"
	tagBrand = "[black:teal:b]"
	tagBold  = "[white::b]"
	tagDim   = "[gray]"
	tagHead  = "[gray::b]"
	tagOK    = "[green]"
	tagBad   = "[red]"
	tagWarn  = "[yellow]"
	tagKey   = "[teal::b]"
	tagSel   = "[teal::b]"
)

// The smallest screen the console lays out; below it only the header shows.
const (
	minWidth  = 80
	minHeight = 24
)

// typeTags colour the TYPE column.
var typeTags = map[string]string{
	"CT":    "[dodgerblue]",
	"VM":    "[mediumpurple]",
	"PHONE": "[darkcyan]",
}

// view draws a snapshot: the header, the dashboard and the pages in front of it.
type view struct {
	*tview.Box

	snap        Snapshot
	kind        layoutKind
	log         *tview.TextView
	logRow      int
	logScrolled bool
	lastLog     func() string
	key         func(*tcell.EventKey) *tcell.EventKey
}

func newView() *view {
	return &view{Box: tview.NewBox()}
}

// Draw draws the view in its rectangle.
func (v *view) Draw(screen tcell.Screen) {
	v.DrawForSubclass(screen, v)

	x, y, w, h := v.GetInnerRect()
	if w <= 0 || h <= 0 {
		return
	}

	drawHeader(screen, v.snap, x, y, w, min(h, headerRows))

	if h <= headerRows {
		return
	}

	l := chooseLayout(w, h)
	if l.kind != v.kind {
		v.kind = l.kind
		v.followLog()
	}

	if l.kind == layoutTooSmall {
		tview.Print(screen, fmt.Sprintf(" %sScreen too small (%d×%d)%s", tagWarn, w, h, tagReset),
			x, y+headerRows, w, tview.AlignLeft, tcell.ColorWhite)

		return
	}

	content := rect{x, y + headerRows, w, h - headerRows}

	switch {
	case v.snap.Page == pageDetails:
		drawDetails(screen, v.snap, content)
	case v.snap.Page == pageHelp:
		drawHelp(screen, content)
	default:
		v.drawDashboard(screen, l, x, y)
	}
}

// InputHandler routes the console's keys to the key handler.
func (v *view) InputHandler() func(event *tcell.EventKey, setFocus func(p tview.Primitive)) {
	return func(event *tcell.EventKey, setFocus func(p tview.Primitive)) {
		if v.key == nil {
			return
		}

		if ev := v.key(event); ev != nil {
			if h := v.Box.InputHandler(); h != nil {
				h(ev, setFocus)
			}
		}
	}
}

// update replaces the snapshot the view draws. It runs on the event loop.
func (v *view) update(s Snapshot) {
	focus := v.snap.Focus

	v.snap = s
	v.snap.Selected = min(max(s.Selected, 0), max(len(s.Instances)-1, 0))

	if v.snap.Focus != focus {
		v.followLog()
	}
}

// eventsFocused reports whether the events panel has the focus and can be scrolled.
func (v *view) eventsFocused() bool {
	return v.log != nil && v.kind == layoutWide && v.snap.Focus == panelEvents
}

// followLog keeps the log on its tail unless the events panel has the focus.
func (v *view) followLog() {
	if v.log == nil {
		return
	}

	if !v.eventsFocused() {
		v.log.SetScrollable(false)

		return
	}

	v.log.SetScrollable(true)
	v.log.ScrollToEnd()
	v.logScrolled = false
}

// drawDashboard draws the layout's boxes.
func (v *view) drawDashboard(screen tcell.Screen, l layout, x, y int) {
	if l.kind == layoutWide {
		drawHardwareBox(screen, shift(l.Hardware, x, y))
		drawTable(screen, v.snap, shift(l.Instances, x, y), l.kind)
		v.drawEvents(screen, shift(l.Events, x, y), v.snap.Focus)
	} else {
		drawHardwareStrip(screen, shift(l.Hardware, x, y))
		drawTable(screen, v.snap, shift(l.Instances, x, y), l.kind)
		drawSummary(screen, v.snap, shift(l.Summary, x, y))
	}

	drawAlert(screen, shift(l.Alert, x, y))

	if l.kind != layoutWide {
		v.drawLastLog(screen, shift(l.Log, x, y))
	}

	drawKeys(screen, shift(l.Keys, x, y))
}

// shift moves a layout rectangle into the view's rectangle.
func shift(r rect, x, y int) rect {
	r.X += x
	r.Y += y

	return r
}

// drawHardwareBox draws the wide layout's hardware box. The hardware strip fills
// it in a later change; its five rows are reserved here.
func drawHardwareBox(screen tcell.Screen, r rect) {
	box(screen, r, "Hardware")
}

// drawHardwareStrip draws the compact layout's hardware strip. The values arrive
// with the hardware strip in a later change.
func drawHardwareStrip(screen tcell.Screen, r rect) {
	if r.empty() {
		return
	}

	tview.Print(screen, " "+tagDim+"Hardware —"+tagReset, r.X, r.Y, r.W, tview.AlignLeft, tcell.ColorWhite)
}

// drawEvents draws the wide layout's events panel, with upstream's log view in it.
func (v *view) drawEvents(screen tcell.Screen, r rect, focus panel) {
	title := "Events (system log)"
	if focus == panelEvents {
		title = "Events (system log) · focused"
	}

	box(screen, r, title)

	if v.log == nil {
		tview.Print(screen, " "+tagDim+"The system log shows here."+tagReset, r.X+1, r.Y+1, max(r.W-2, 1), tview.AlignLeft, tcell.ColorWhite)

		return
	}

	inner := rect{r.X + 1, r.Y + 1, r.W - 2, r.H - 2}
	if inner.empty() {
		return
	}

	v.log.SetRect(inner.X, inner.Y, inner.W, inner.H)
	v.log.Draw(screen)
}

// drawLastLog draws the compact layout's last log line.
func (v *view) drawLastLog(screen tcell.Screen, r rect) {
	if r.empty() {
		return
	}

	line := ""
	if v.lastLog != nil {
		line = v.lastLog()
	}

	if line == "" {
		tview.Print(screen, " "+tagDim+"—"+tagReset, r.X, r.Y, r.W, tview.AlignLeft, tcell.ColorWhite)

		return
	}

	tview.Print(screen, " "+tagDim+safe(fit(line, r.W-2))+tagReset, r.X, r.Y, r.W, tview.AlignLeft, tcell.ColorWhite)
}

// drawSummary draws the compact layout's selected instance line.
func drawSummary(screen tcell.Screen, s Snapshot, r rect) {
	if r.empty() {
		return
	}

	if len(s.Instances) == 0 {
		tview.Print(screen, " "+tagDim+"No instance selected"+tagReset, r.X, r.Y, r.W, tview.AlignLeft, tcell.ColorWhite)

		return
	}

	inst := s.Instances[s.Selected]
	parts := []string{tagSel + "▶ " + safe(fit(printable(inst.Name), 20)) + tagReset, tagBold + printable(inst.Type) + tagReset}

	if d, ok := detailsFor(s, s.Selected); ok {
		if d.Description != "" {
			parts = append(parts, tagDim+safe(fit(printable(d.Description), 20))+tagReset)
		}

		if inst.DiskUsage > 0 {
			parts = append(parts, "disk "+shortBytes(inst.DiskUsage))
		}

		if up := upText(inst, s.Now); up != "—" {
			parts = append(parts, "up "+up)
		}
	}

	tview.Print(screen, " "+strings.Join(parts, " · "), r.X, r.Y, r.W, tview.AlignLeft, tcell.ColorWhite)
}

// drawAlert draws the alert line. Alerts arrive with the live instance state; the
// line shows that there is none.
func drawAlert(screen tcell.Screen, r rect) {
	if r.empty() {
		return
	}

	tview.Print(screen, " "+tagOK+"✓ No alerts"+tagReset, r.X, r.Y, r.W, tview.AlignLeft, tcell.ColorWhite)
}

// drawKeys draws the key line.
func drawKeys(screen tcell.Screen, r rect) {
	if r.empty() {
		return
	}

	keys := []string{
		"↑↓ select", "Tab next panel", "Enter details", "? help", "L upstream",
	}

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		key, rest, _ := strings.Cut(k, " ")
		parts = append(parts, tagKey+key+tagReset+tagDim+" "+rest+tagReset)
	}

	tview.Print(screen, " "+strings.Join(parts, "  "), r.X, r.Y, r.W, tview.AlignLeft, tcell.ColorWhite)
}
