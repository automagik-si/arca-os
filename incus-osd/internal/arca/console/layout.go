package console

// rect is a screen rectangle in cells.
type rect struct {
	X, Y, W, H int
}

// empty reports whether r has no cell to draw in.
func (r rect) empty() bool {
	return r.W <= 0 || r.H <= 0
}

// layoutKind is the console's layout for one screen size.
type layoutKind int

const (
	layoutTooSmall layoutKind = iota
	layoutCompact
	layoutWide
)

// The smallest screen each layout needs.
const (
	wideWidth  = 120
	wideHeight = 36
)

// The console's fixed rows and the wide layout's right column.
const (
	headerRows   = 2
	hardwareWide = 5
	hardwareRows = 2 // the compact strip
	rightWidth   = 48
	alertRows    = 1
	keyRows      = 1
	summaryRows  = 1
	logRows      = 1 // the compact last-log line
	minEvents    = 6
	minTable     = 6
)

// layout is how the console fills one screen: the header at the top, then the
// dashboard's boxes.
type layout struct {
	kind                layoutKind
	W, H                int
	Header, Hardware    rect
	Instances, Right    rect
	Events              rect
	Summary, Alert, Log rect
	Keys                rect
}

// chooseLayout returns the layout for a w×h screen: wide at 120×36 or more,
// compact down to 80×24, and too small below that.
func chooseLayout(w, h int) layout {
	l := layout{W: w, H: h}
	l.Header = rect{0, 0, w, min(h, headerRows)}

	switch {
	case w < minWidth || h < minHeight:
		l.kind = layoutTooSmall

		return l
	case w >= wideWidth && h >= wideHeight:
		l.kind = layoutWide
	default:
		l.kind = layoutCompact
	}

	if l.kind == layoutWide {
		chooseWide(&l)
	} else {
		chooseCompact(&l)
	}

	return l
}

// chooseWide fills the wide layout: a five-row hardware box, then the instances
// beside a 48-wide right column, the events under the instances, the alert line
// and the key line. Extra rows go to the instances and the events.
func chooseWide(l *layout) {
	y := headerRows

	l.Hardware = rect{0, y, l.W, hardwareWide}
	y += hardwareWide

	body := l.H - y - alertRows - keyRows
	if body < minTable+minEvents {
		body = minTable + minEvents
	}

	events := body / 3
	if events < minEvents {
		events = minEvents
	}

	if body-events < minTable {
		events = max(body-minTable, minEvents)
	}

	table := body - events

	l.Instances = rect{0, y, l.W - rightWidth, table}
	l.Right = rect{l.W - rightWidth, y, rightWidth, body}
	l.Events = rect{0, y + table, l.W - rightWidth, events}

	y += body
	l.Summary = rect{}
	l.Alert = rect{0, y, l.W, alertRows}
	l.Log = rect{}
	l.Keys = rect{0, l.H - keyRows, l.W, keyRows}
}

// chooseCompact fills the compact layout: a two-row hardware strip, the instance
// table, the selected instance's summary, the alert line, the system log's last
// line and the key line.
func chooseCompact(l *layout) {
	y := headerRows

	l.Hardware = rect{0, y, l.W, hardwareRows}
	y += hardwareRows

	tail := summaryRows + alertRows + logRows + keyRows
	table := l.H - y - tail
	if table < minTable {
		table = minTable
	}

	l.Instances = rect{0, y, l.W, table}
	y += table

	l.Summary = rect{0, y, l.W, summaryRows}
	l.Alert = rect{0, y + summaryRows, l.W, alertRows}
	l.Log = rect{0, y + summaryRows + alertRows, l.W, logRows}
	l.Keys = rect{0, l.H - keyRows, l.W, keyRows}
}
