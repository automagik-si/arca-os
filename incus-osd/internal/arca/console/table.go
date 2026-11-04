package console

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// hostThreads and hostMemoryBytes are the limits an instance without a limit of
// its own is measured against. Tests replace them.
var (
	hostThreads     = runtime.NumCPU
	hostMemoryBytes = readMemTotal
)

// readMemTotal returns MemTotal from /proc/meminfo, or 0 when it cannot be read.
func readMemTotal() int64 {
	body, err := os.ReadFile(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return 0
	}

	for line := range strings.SplitSeq(string(body), "\n") {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				kb, _ := strconv.ParseInt(fields[0], 10, 64)

				return kb * 1024
			}
		}
	}

	return 0
}

// colID names one table column.
type colID int

const (
	colName colID = iota
	colType
	colState
	colCPU
	colRAM
	colDisk
	colIP
	colUp
)

// column is one table column: its title and how wide its text may be.
type column struct {
	id    colID
	title string
	w     int
}

// Column widths; the name column takes what is left.
const (
	nameMinWidth = 10
	nameMaxWidth = 24
	typeWidth    = 6
	stateWidth   = 9
	cpuNumWidth  = 7
	ramNumWidth  = 12
	ramBarWidth  = 18
	diskWidth    = 9
	ipWidth      = 14
	upWidth      = 7
	barCells     = 5
	barWidth     = barCells + 1
)

// tableColumns returns the columns that fit in w for the given layout. Columns
// drop from the right in this order: UP, then DISK, then the CPU and RAM
// mini-bars; the compact layout drops all three. The name column takes the rest.
func tableColumns(w int, kind layoutKind) ([]column, bool) {
	cols := []column{
		{colName, "NAME", 0},
		{colType, "TYPE", typeWidth},
		{colState, "STATE", stateWidth},
		{colCPU, "CPU", cpuNumWidth},
		{colRAM, "RAM", ramNumWidth},
		{colDisk, "DISK", diskWidth},
		{colIP, "IP", ipWidth},
		{colUp, "UP", upWidth},
	}

	bars := kind == layoutWide

	if kind == layoutCompact {
		cols = drop(cols, colUp)
		cols = drop(cols, colDisk)
	}

	fits := func() bool { return columnsWidth(cols, bars) <= w }

	if !fits() {
		cols = drop(cols, colUp)
	}

	if !fits() {
		cols = drop(cols, colDisk)
	}

	if !fits() {
		bars = false
	}

	if !fits() {
		cols = drop(cols, colCPU)
		cols = drop(cols, colRAM)
	}

	fixed := columnsWidth(cols, bars)

	limit := nameMaxWidth
	if kind == layoutCompact {
		limit = min(limit, 18)
	}

	for i := range cols {
		if cols[i].id == colName {
			cols[i].w = min(max(w-fixed+cols[i].w, nameMinWidth), limit)
		}
	}

	return cols, bars
}

// drop removes one column.
func drop(cols []column, id colID) []column {
	for i := range cols {
		if cols[i].id == id {
			return append(cols[:i], cols[i+1:]...)
		}
	}

	return cols
}

// columnsWidth is the width of the columns with one space between them.
func columnsWidth(cols []column, bars bool) int {
	total := max(len(cols)-1, 0)

	for _, c := range cols {
		total += columnWidth(c, bars)
	}

	return total
}

// columnWidth is one column's width with or without its mini-bar.
func columnWidth(c column, bars bool) int {
	switch {
	case c.id == colName && c.w == 0:
		return nameMinWidth
	case c.id == colCPU && bars:
		return cpuNumWidth + barWidth
	case c.id == colRAM && bars:
		return ramBarWidth
	default:
		return c.w
	}
}

// bar returns a barCells-cell bar for use against limit, full at the limit.
func bar(use, limit int64) string {
	if limit <= 0 {
		limit = 1
	}

	frac := float64(use) / float64(limit)
	filled := int(frac*barCells + 0.5)
	filled = max(min(filled, barCells), 0)

	return strings.Repeat("█", filled) + strings.Repeat("░", barCells-filled)
}

// barColor is the tag a bar draws in: yellow above 90% of the limit.
func barColor(use, limit int64) string {
	if limit > 0 && float64(use)/float64(limit) > 0.9 {
		return tagWarn
	}

	return tagBold
}

// percentText is a CPU share, or "—" while unknown.
func percentText(p float64) string {
	if p < 0 {
		return "—"
	}

	return fmt.Sprintf("%.1f%%", p)
}

// shortBytes is a byte count in the shortest unit, one decimal below 100.
// Zero is "0" so a caller can tell it from a missing value.
func shortBytes(n int64) string {
	if n <= 0 {
		return "0"
	}

	units := []struct {
		limit int64
		suf   string
	}{{1 << 40, "T"}, {1 << 30, "G"}, {1 << 20, "M"}, {1 << 10, "K"}}

	for _, u := range units {
		if n >= u.limit {
			v := float64(n) / float64(u.limit)
			if v >= 100 {
				return fmt.Sprintf("%.0f%s", v, u.suf)
			}

			return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0") + u.suf
		}
	}

	return strconv.FormatInt(n, 10) + "B"
}

// memText is use against limit, "host" when the instance has no limit of its own.
func memText(use, limit int64) string {
	if use <= 0 {
		return "—"
	}

	if limit <= 0 {
		return shortBytes(use) + " / host"
	}

	return shortBytes(use) + " / " + shortBytes(limit)
}

// upText is how long an instance has been up, or "—" when it is not running.
func upText(inst Instance, now time.Time) string {
	if inst.Status != "Running" || inst.StartedAt.IsZero() {
		return "—"
	}

	d := now.Sub(inst.StartedAt)
	if d < 0 {
		return "—"
	}

	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60

	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %02dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}

// instanceTable applies reads of the instance list to a snapshot.
type instanceTable struct {
	downSince time.Time
}

// apply records one read made at now. After a failure the last rows stay, marked
// stale, and the error names the time of the first failure in a row.
func (it *instanceTable) apply(s *Snapshot, res instancesResult, err error, now time.Time) {
	if err == nil {
		s.Instances, s.Details = res.Rows, res.Details
		s.InstancesErr, s.InstancesAt = "", now
		it.downSince = time.Time{}
		s.Selected = min(s.Selected, max(len(s.Instances)-1, 0))

		return
	}

	if it.downSince.IsZero() {
		it.downSince = now
	}

	for i := range s.Instances {
		s.Instances[i].Stale = true
	}

	s.InstancesErr = "Incus is not running since " + it.downSince.Format("15:04")
}

// gutter is the two cells the selection marker takes before the first column.
const gutter = 2

// drawTable draws the instance table in r and returns the rows it used.
func drawTable(screen tcell.Screen, s Snapshot, r rect, kind layoutKind) int {
	if r.empty() {
		return 0
	}

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

	title += strings.Repeat("─", max(r.W-width(title), 0))
	tview.Print(screen, tagDim+title+tagReset, r.X, r.Y, r.W, tview.AlignLeft, tcell.ColorWhite)

	if r.H < 2 {
		return 1
	}

	cols, bars := tableColumns(r.W-gutter, kind)

	x := r.X + gutter

	for i, c := range cols {
		if i > 0 {
			x++
		}

		tview.Print(screen, tagHead+c.title+tagReset, x, r.Y+1, columnWidth(c, bars), tview.AlignLeft, tcell.ColorWhite)
		x += columnWidth(c, bars)
	}

	row := r.Y + 2
	end := r.Y + r.H

	if s.InstancesErr != "" && row < end {
		tview.Print(screen, " "+tagWarn+safe(fit(printable(s.InstancesErr), r.W-2))+tagReset, r.X, row, r.W, tview.AlignLeft, tcell.ColorWhite)
		row++
	}

	switch {
	case !read && s.InstancesErr == "":
		tview.Print(screen, " "+tagDim+"Reading instances…"+tagReset, r.X, row, r.W, tview.AlignLeft, tcell.ColorWhite)

		return row - r.Y
	case read && len(s.Instances) == 0:
		tview.Print(screen, " "+tagDim+"No instances"+tagReset, r.X, row, r.W, tview.AlignLeft, tcell.ColorWhite)

		return row - r.Y
	}

	for i, inst := range s.Instances {
		if row >= end {
			break
		}

		if row == end-1 && i < len(s.Instances)-1 {
			tview.Print(screen, fmt.Sprintf(" %s… %d more%s", tagDim, len(s.Instances)-i, tagReset), r.X, row, r.W, tview.AlignLeft, tcell.ColorWhite)

			return row - r.Y + 1
		}

		drawRow(screen, s, inst, i == s.Selected, row, r.X, cols, bars)
		row++
	}

	return row - r.Y
}

// drawRow draws one instance at row y.
func drawRow(screen tcell.Screen, s Snapshot, inst Instance, selected bool, y, x int, cols []column, bars bool) {
	status := strings.ToUpper(printable(inst.Status))

	nameTag, typeTag, dimTag := tagBold, typeTags[printable(inst.Type)], tagDim

	var stateTag string

	switch status {
	case "RUNNING":
		stateTag = tagOK
	case "STOPPED":
		stateTag = tagDim
	default:
		stateTag = tagWarn
	}

	if inst.Stale {
		nameTag, typeTag, stateTag, dimTag = tagDim, tagDim, tagDim, tagDim
	}

	if selected {
		tview.Print(screen, tagSel+"▶"+tagReset, x, y, 1, tview.AlignLeft, tcell.ColorWhite)
	}

	cx := x + gutter

	for i, c := range cols {
		if i > 0 {
			cx++
		}

		w := columnWidth(c, bars)

		switch c.id {
		case colName:
			tview.Print(screen, nameTag+safe(fit(printable(inst.Name), w))+tagReset, cx, y, w, tview.AlignLeft, tcell.ColorWhite)
		case colType:
			tview.Print(screen, typeTag+safe(fit(printable(inst.Type), w))+tagReset, cx, y, w, tview.AlignLeft, tcell.ColorWhite)
		case colState:
			tview.Print(screen, stateTag+safe(fit(status, w))+tagReset, cx, y, w, tview.AlignLeft, tcell.ColorWhite)
		case colCPU:
			cpuCell(screen, inst, cx, y, w, bars, dimTag)
		case colRAM:
			tview.Print(screen, ramCell(inst, bars, dimTag), cx, y, w, tview.AlignLeft, tcell.ColorWhite)
		case colDisk:
			tview.Print(screen, dimTag+diskCell(inst)+tagReset, cx, y, w, tview.AlignLeft, tcell.ColorWhite)
		case colIP:
			ip, tag := printable(inst.IPv4), tagBold
			if ip == "" || inst.Status != "Running" {
				ip, tag = "—", dimTag
			}

			tview.Print(screen, tag+safe(fit(ip, w))+tagReset, cx, y, w, tview.AlignLeft, tcell.ColorWhite)
		case colUp:
			tview.Print(screen, dimTag+upText(inst, s.Now)+tagReset, cx, y, w, tview.AlignLeft, tcell.ColorWhite)
		default:
		}

		cx += w
	}
}

// cpuUse is an instance's CPU share against its own limit, as a percentage of the
// host: limits.cpu names threads, so a two-thread limit on an eight-thread host is
// 25%. Without a limit the instance is measured against the whole host.
func cpuUse(inst Instance) (float64, float64) {
	threads := float64(hostThreads())
	if threads <= 0 {
		threads = 1
	}

	limit := 100.0
	if inst.CPULimit > 0 {
		limit = inst.CPULimit / threads * 100
	}

	return inst.CPUPercent, limit
}

// memUse is an instance's memory use against its own limit, or the host's memory.
func memUse(inst Instance, hostMem int64) (int64, int64) {
	limit := inst.MemLimit
	if limit <= 0 {
		limit = hostMem
	}

	return inst.MemUsage, limit
}

// cpuCell draws the CPU share, with its bar when the layout has one.
func cpuCell(screen tcell.Screen, inst Instance, x, y, w int, bars bool, dimTag string) {
	if !bars {
		tview.Print(screen, tagBold+percentText(inst.CPUPercent)+tagReset, x, y, w, tview.AlignLeft, tcell.ColorWhite)

		return
	}

	if inst.CPUPercent < 0 || inst.Status != "Running" {
		tview.Print(screen, dimTag+"—"+tagReset, x, y, w, tview.AlignLeft, tcell.ColorWhite)

		return
	}

	use, limit := cpuUse(inst)
	tag := barColor(int64(use*100), int64(limit*100))

	tview.Print(screen, tag+bar(int64(use*100), int64(limit*100))+" "+percentText(inst.CPUPercent)+tagReset,
		x, y, w, tview.AlignLeft, tcell.ColorWhite)
}

// ramCell is the RAM text, with its bar when the layout has one.
func ramCell(inst Instance, bars bool, dimTag string) string {
	if inst.Status != "Running" || inst.MemUsage <= 0 {
		return dimTag + "—" + tagReset
	}

	if !bars {
		return tagBold + memText(inst.MemUsage, inst.MemLimit) + tagReset
	}

	use, limit := memUse(inst, hostMemoryBytes())

	return barColor(use, limit) + bar(use, limit) + " " + memText(inst.MemUsage, inst.MemLimit) + tagReset
}

// diskCell is the root disk's use, or "—" while unknown.
func diskCell(inst Instance) string {
	if inst.DiskUsage <= 0 {
		return "—"
	}

	return shortBytes(inst.DiskUsage)
}
