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

// The instance table's columns: where each starts and how wide its text may be.
const (
	colName   = 2
	colType   = 17
	colState  = 24
	colIP     = 36
	nameWidth = colType - colName - 1
)

// typeTags colour the TYPE column.
var typeTags = map[string]string{
	"CT":    "[dodgerblue]",
	"VM":    "[mediumpurple]",
	"PHONE": "[darkcyan]",
}

// view draws a snapshot: the header, the instance table and the key line.
type view struct {
	*tview.Box

	snap Snapshot
	sel  int
}

func newView() *view {
	return &view{Box: tview.NewBox()}
}

// update replaces the snapshot the view draws. It runs on the event loop.
func (v *view) update(s Snapshot) {
	v.snap = s
	v.sel = min(v.sel, max(len(s.Instances)-1, 0))
}

// Draw draws the view in its rectangle.
func (v *view) Draw(screen tcell.Screen) {
	v.DrawForSubclass(screen, v)

	x, y, w, h := v.GetInnerRect()
	if w <= 0 || h <= 0 {
		return
	}

	drawHeader(screen, v.snap, x, y, w, min(h, 2))

	if h <= 2 {
		return
	}

	if w < minWidth || h < minHeight {
		tview.Print(screen, fmt.Sprintf(" %sScreen too small (%d×%d)%s", tagWarn, w, h, tagReset), x, y+2, w, tview.AlignLeft, tcell.ColorWhite)

		return
	}

	v.drawInstances(screen, x, y+2, w, h-3)
	tview.Print(screen, " "+tagKey+"?"+tagReset+tagDim+" help"+tagReset, x, y+h-1, w, tview.AlignLeft, tcell.ColorWhite)
}

// drawInstances draws the table's title, its column header and its rows in h rows at (x, y).
func (v *view) drawInstances(screen tcell.Screen, x, y, w, h int) {
	s := v.snap
	read := !s.InstancesAt.IsZero()

	title := "─ Instances "
	if read {
		running := 0

		for _, inst := range s.Instances {
			if inst.Status == "Running" {
				running++
			}
		}

		title += fmt.Sprintf("%d · %d running ", len(s.Instances), running)
	}

	title += strings.Repeat("─", max(w-width(title), 0))
	tview.Print(screen, tagDim+title+tagReset, x, y, w, tview.AlignLeft, tcell.ColorWhite)

	if h < 2 {
		return
	}

	tview.Print(screen, tagHead+"NAME"+tagReset, x+colName, y+1, w-colName, tview.AlignLeft, tcell.ColorWhite)
	tview.Print(screen, tagHead+"TYPE"+tagReset, x+colType, y+1, w-colType, tview.AlignLeft, tcell.ColorWhite)
	tview.Print(screen, tagHead+"STATE"+tagReset, x+colState, y+1, w-colState, tview.AlignLeft, tcell.ColorWhite)
	tview.Print(screen, tagHead+"IP"+tagReset, x+colIP, y+1, w-colIP, tview.AlignLeft, tcell.ColorWhite)

	row := y + 2
	end := y + h

	if s.InstancesErr != "" && row < end {
		tview.Print(screen, " "+tagWarn+safe(fit(printable(s.InstancesErr), w-2))+tagReset, x, row, w, tview.AlignLeft, tcell.ColorWhite)
		row++
	}

	switch {
	case !read && s.InstancesErr == "":
		tview.Print(screen, " "+tagDim+"Reading instances…"+tagReset, x, row, w, tview.AlignLeft, tcell.ColorWhite)

		return
	case read && len(s.Instances) == 0:
		tview.Print(screen, " "+tagDim+"No instances"+tagReset, x, row, w, tview.AlignLeft, tcell.ColorWhite)

		return
	}

	for i, inst := range s.Instances {
		if row >= end {
			return
		}

		if row == end-1 && i < len(s.Instances)-1 {
			tview.Print(screen, fmt.Sprintf(" %s… %d more%s", tagDim, len(s.Instances)-i, tagReset), x, row, w, tview.AlignLeft, tcell.ColorWhite)

			return
		}

		v.drawRow(screen, inst, i == v.sel, x, row, w)
		row++
	}
}

// drawRow draws one instance at row y. A stale row is grey throughout.
func (v *view) drawRow(screen tcell.Screen, inst Instance, selected bool, x, y, w int) {
	name, kind := printable(inst.Name), printable(inst.Type)
	status := strings.ToUpper(printable(inst.Status))

	nameTag, kindTag, stateTag := tagBold, typeTags[kind], tagWarn

	switch status {
	case "RUNNING":
		stateTag = tagOK
	case "STOPPED":
		stateTag = tagDim
	}

	ip, ipTag := printable(inst.IPv4), ""
	if ip == "" {
		ip, ipTag = "—", tagDim
	}

	if inst.Stale {
		nameTag, kindTag, stateTag, ipTag = tagDim, tagDim, tagDim, tagDim
	}

	if selected {
		tview.Print(screen, tagSel+"▶"+tagReset, x, y, 1, tview.AlignLeft, tcell.ColorWhite)
	}

	cell := func(col, n int, tag, text string) {
		tview.Print(screen, tag+safe(fit(text, n))+tagReset, x+col, y, n, tview.AlignLeft, tcell.ColorWhite)
	}

	cell(colName, nameWidth, nameTag, name)
	cell(colType, colState-colType-1, kindTag, kind)
	cell(colState, colIP-colState-1, stateTag, status)
	cell(colIP, w-colIP, ipTag, ip)
}
