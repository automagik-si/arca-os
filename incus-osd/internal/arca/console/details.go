package console

import (
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// detailsFor returns the selected instance's details.
func detailsFor(s Snapshot, sel int) (InstanceDetails, bool) {
	if sel < 0 || sel >= len(s.Instances) || sel >= len(s.Details) {
		return InstanceDetails{}, false
	}

	want := s.Instances[sel]

	for _, d := range s.Details {
		if d.Name == want.Name && d.Project == want.Project {
			return d, true
		}
	}

	return InstanceDetails{}, false
}

// detailsLines is the details page's allowlist: name, type, project, image
// description, state, processes, CPU time, memory use, peak and limit, disk use
// per device, addresses and traffic per NIC, created and last started. No other
// configuration key, device or environment value is read anywhere on this panel.
func detailsLines(d InstanceDetails) []string {
	lines := []string{
		field("Name", printable(d.Name)),
		field("Type", printable(d.Type)),
		field("Project", printable(d.Project)),
		field("Image", printable(d.Description)),
		field("State", printable(d.Status)),
	}

	if d.Processes > 0 {
		lines = append(lines, field("Processes", fmt.Sprintf("%d", d.Processes)))
	}

	if d.CPUTime > 0 {
		lines = append(lines, field("CPU time", d.CPUTime.Round(time.Millisecond).String()))
	}

	if d.MemUsage > 0 {
		lines = append(lines, field("Memory", shortBytes(d.MemUsage)))
	}

	if d.MemPeak > 0 {
		lines = append(lines, field("Peak", shortBytes(d.MemPeak)))
	}

	if d.MemLimit > 0 {
		lines = append(lines, field("Limit", shortBytes(d.MemLimit)))
	} else {
		lines = append(lines, field("Limit", "host"))
	}

	for _, disk := range d.Disks {
		lines = append(lines, field("Disk "+printable(disk.Name), shortBytes(disk.Usage)))
	}

	for _, nic := range d.NICs {
		addrs := "—"
		if len(nic.Addresses) > 0 {
			quoted := make([]string, 0, len(nic.Addresses))
			for _, a := range nic.Addresses {
				quoted = append(quoted, printable(a))
			}

			addrs = strings.Join(quoted, " ")
		}

		lines = append(lines, field("Net "+printable(nic.Name),
			fmt.Sprintf("%s · ↓ %s/s ↑ %s/s", addrs, shortBytes(nic.Rx), shortBytes(nic.Tx))))
	}

	if !d.CreatedAt.IsZero() {
		lines = append(lines, field("Created", d.CreatedAt.Local().Format("2006-01-02 15:04")))
	}

	if !d.StartedAt.IsZero() {
		lines = append(lines, field("Started", d.StartedAt.Local().Format("2006-01-02 15:04")))
	}

	return lines
}

// field formats one label and value at a fixed column.
func field(label, value string) string {
	return fmt.Sprintf("%-11s %s", label, value)
}

// drawDetails draws the details page in r.
func drawDetails(screen tcell.Screen, s Snapshot, r rect) {
	box(screen, r, "Details")

	x, y, w, h := r.X+1, r.Y+1, r.W-2, r.H-2
	if w <= 0 || h <= 0 {
		return
	}

	d, ok := detailsFor(s, s.Selected)
	if !ok {
		tview.Print(screen, " "+tagDim+"Select an instance first"+tagReset, x, y, w, tview.AlignLeft, tcell.ColorWhite)

		return
	}

	for i, line := range detailsLines(d) {
		if i >= h {
			break
		}

		tview.Print(screen, " "+safe(line), x, y+i, w, tview.AlignLeft, tcell.ColorWhite)
	}
}

// box draws a bordered box with its title in the top border.
func box(screen tcell.Screen, r rect, title string) {
	if r.W < 4 || r.H < 2 {
		return
	}

	label := "─ " + title + " "
	top := "┌" + label + strings.Repeat("─", max(r.W-2-width(label), 0)) + "┐"
	tview.Print(screen, tagDim+top+tagReset, r.X, r.Y, r.W, tview.AlignLeft, tcell.ColorWhite)

	for y := r.Y + 1; y < r.Y+r.H-1; y++ {
		tview.Print(screen, tagDim+"│"+tagReset, r.X, y, 1, tview.AlignLeft, tcell.ColorWhite)
		tview.Print(screen, tagDim+"│"+tagReset, r.X+r.W-1, y, 1, tview.AlignLeft, tcell.ColorWhite)
	}

	tview.Print(screen, tagDim+"└"+strings.Repeat("─", r.W-2)+"┘"+tagReset, r.X, r.Y+r.H-1, r.W, tview.AlignLeft, tcell.ColorWhite)
}
