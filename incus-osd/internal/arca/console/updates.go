package console

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/lxc/incus-os/incus-osd/internal/state"
	osupdate "github.com/lxc/incus-os/incus-osd/internal/update"
)

// updatesEvery is how often the updates page is read.
const updatesEvery = 5 * time.Second

// Updates is the updates page's data, from the automatic updater.
type Updates struct {
	Automatic                        bool
	Window, Status                   string
	NextWindow, LastCheck            time.Time
	Current, Previous, Good, Pending string

	// Failed are "<version>: <reason>" lines; FailedIDs what "try again" passes, in the same order.
	Failed    []string
	FailedIDs []string

	LastResult, LastReason, NeedsOwner string
}

// clone returns a copy of u that shares no slice with it.
func (u Updates) clone() Updates {
	c := u
	c.Failed = slices.Clone(u.Failed)
	c.FailedIDs = slices.Clone(u.FailedIDs)

	return c
}

// updateActions are the owner's actions on the updates page. They may block; the keys run them
// off the event loop.
type updateActions interface {
	SetAutomatic(on bool)
	ShiftWindow(hours int)
	Retry(version string)
	CheckNow()
}

// liveUpdates reads the automatic updater's summary.
func liveUpdates(s *state.State) Updates {
	sum := osupdate.Summarize(s, time.Now())

	u := Updates{
		Automatic:  sum.Automatic,
		Window:     sum.Window,
		Status:     sum.Status,
		NextWindow: sum.NextWindow,
		LastCheck:  sum.LastCheck,
		Current:    sum.Current,
		Previous:   sum.Previous,
		Good:       sum.Good,
		Pending:    sum.Pending,
		Failed:     sum.Failed,
		LastResult: sum.LastResult,
		LastReason: sum.LastReason,
		NeedsOwner: sum.NeedsOwner,
	}

	for _, line := range sum.Failed {
		id, _, _ := strings.Cut(line, ": ")
		u.FailedIDs = append(u.FailedIDs, strings.Replace(id, " ", ":", 1))
	}

	return u
}

// liveActions changes the running system.
type liveActions struct {
	s *state.State
}

func (l liveActions) SetAutomatic(on bool) {
	err := osupdate.SetAutomatic(context.Background(), l.s, on)
	if err != nil {
		slog.Error("Console: automatic updates could not be switched", "err", err)
	}
}

func (l liveActions) ShiftWindow(hours int) {
	err := osupdate.ShiftRestartWindow(context.Background(), l.s, hours)
	if err != nil {
		slog.Error("Console: the restart window could not be moved", "err", err)
	}
}

func (l liveActions) Retry(version string) {
	err := osupdate.RetryVersion(context.Background(), l.s, version)
	if err != nil {
		slog.Error("Console: the version could not be tried again", "err", err)
	}
}

func (l liveActions) CheckNow() {
	slog.Info("Console: update check asked by the owner")
	osupdate.CheckNow(l.s)
}

// updatesKey handles a key on the updates page and reports whether it used it.
func (c *console) updatesKey(ev *tcell.EventKey) bool {
	c.mu.Lock()
	on := c.snap.Page == pageUpdates
	u := c.snap.Updates.clone()
	sel := c.snap.UpdateSel
	c.mu.Unlock()

	if !on || c.src.actions == nil {
		return false
	}

	var act func()

	switch ev.Key() { //nolint:exhaustive // tcell's key set is open; the default covers every other key
	case tcell.KeyUp:
		return c.moveUpdateSel(-1)
	case tcell.KeyDown:
		return c.moveUpdateSel(1)
	case tcell.KeyRune:
		switch ev.Rune() {
		case 'k':
			return c.moveUpdateSel(-1)
		case 'j':
			return c.moveUpdateSel(1)
		case 'o', 'O':
			act = func() { c.src.actions.SetAutomatic(!u.Automatic) }
		case '[':
			act = func() { c.src.actions.ShiftWindow(-1) }
		case ']':
			act = func() { c.src.actions.ShiftWindow(1) }
		case 'c', 'C':
			act = c.src.actions.CheckNow
		case 'r', 'R':
			if sel < 0 || sel >= len(u.FailedIDs) {
				return true
			}

			version := u.FailedIDs[sel]
			act = func() { c.src.actions.Retry(version) }
		default:
			return false
		}
	default:
		return false
	}

	go func() {
		act()
		c.readUpdates()
		c.publish()
	}()

	return true
}

// moveUpdateSel moves the selected failed version.
func (c *console) moveUpdateSel(by int) bool {
	c.set(func(s *Snapshot) bool {
		was := s.UpdateSel
		s.UpdateSel = min(max(s.UpdateSel+by, 0), max(len(s.Updates.Failed)-1, 0))

		return s.UpdateSel != was
	})

	return true
}

// readUpdates reads the updates source into the snapshot.
func (c *console) readUpdates() {
	if c.src.updates == nil {
		return
	}

	u := c.src.updates()

	c.mu.Lock()
	defer c.mu.Unlock()

	c.snap.Updates = u
	c.snap.UpdateSel = min(max(c.snap.UpdateSel, 0), max(len(u.Failed)-1, 0))
}

// updatesLines is the updates page's text.
func updatesLines(u Updates, sel int) []string {
	lines := []string{}

	if u.NeedsOwner != "" {
		lines = append(lines, tagBad+"Needs you: "+safe(printable(u.NeedsOwner))+tagReset,
			tagDim+"Automatic updates are paused; o turns them back on."+tagReset, "")
	}

	auto := tagWarn + "Off" + tagReset
	if u.Automatic {
		auto = tagOK + "On" + tagReset
	}

	lines = append(lines,
		field("Automatic", auto+tagDim+"   o switches on / off"+tagReset),
		field("Restarts", safe(u.Window)+timeText(", next at ", u.NextWindow)+tagDim+"   [ ] move by an hour"+tagReset),
		field("Status", safe(printable(u.Status))),
		field("Current", orDash(u.Current)),
		field("Previous", orDash(u.Previous)),
		field("Good", orDash(u.Good)),
		field("Pending", orDash(u.Pending)),
		field("Last check", orDash(timeText("", u.LastCheck))),
		field("Last result", orDash(safe(printable(u.LastResult)))),
	)

	if u.LastReason != "" {
		lines = append(lines, field("Reason", safe(printable(u.LastReason))))
	}

	lines = append(lines, "", tagHead+"Failed versions"+tagReset+tagDim+"   r tries the selected one again"+tagReset)

	if len(u.Failed) == 0 {
		lines = append(lines, tagDim+"None"+tagReset)
	}

	for i, f := range u.Failed {
		if i == sel {
			lines = append(lines, tagSel+"▶ "+safe(printable(f))+tagReset)
		} else {
			lines = append(lines, "  "+safe(printable(f)))
		}
	}

	return append(lines, "", tagDim+"c checks for updates now · Esc closes this page"+tagReset)
}

func orDash(v string) string {
	if v == "" {
		return "—"
	}

	return v
}

func timeText(prefix string, t time.Time) string {
	if t.IsZero() {
		return ""
	}

	return prefix + t.Format("2006-01-02 15:04")
}

// drawUpdates draws the updates page in r.
func drawUpdates(screen tcell.Screen, s Snapshot, r rect) {
	box(screen, r, "Updates")

	x, y, w, h := r.X+1, r.Y+1, r.W-2, r.H-2
	if w <= 0 || h <= 0 {
		return
	}

	for i, line := range updatesLines(s.Updates, s.UpdateSel) {
		if i >= h {
			break
		}

		tview.Print(screen, " "+line, x, y+i, w, tview.AlignLeft, tcell.ColorWhite)
	}
}

// updatesAlert returns the alert line's update notice and its tag, or "" when there is none.
func updatesAlert(u Updates) (string, string) {
	switch {
	case u.NeedsOwner != "":
		return tagBad, "✗ Needs you: " + u.NeedsOwner + " (U updates)"
	case u.Pending != "" || strings.HasPrefix(u.Status, "Checking") || strings.HasPrefix(u.Status, "Went back"):
		return tagWarn, "● " + u.Status
	default:
		return "", ""
	}
}
