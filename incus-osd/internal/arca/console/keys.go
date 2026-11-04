package console

import (
	"strings"

	"github.com/gdamore/tcell/v2"
)

// key handles one key press on the console and returns the event to pass on, or
// nil when the console used it. It runs on the TUI's event loop.
func (c *console) key(ev *tcell.EventKey) *tcell.EventKey {
	switch ev.Key() {
	case tcell.KeyEsc:
		if c.set(func(s *Snapshot) bool {
			if s.Page == pageNone {
				return false
			}

			s.Page = pageNone

			return true
		}) {
			return nil
		}

		return ev
	case tcell.KeyUp, tcell.KeyDown, tcell.KeyPgUp, tcell.KeyPgDn, tcell.KeyHome, tcell.KeyEnd:
		return c.verticalKey(ev.Key())
	case tcell.KeyTab, tcell.KeyBacktab:
		c.set(func(s *Snapshot) bool {
			if s.Page != pageNone {
				return false
			}

			if ev.Key() == tcell.KeyBacktab || c.view.kind != layoutWide {
				s.Focus = panelInstances
			} else {
				s.Focus = (s.Focus + 1) % 2
			}

			return true
		})

		return nil
	case tcell.KeyEnter:
		c.set(func(s *Snapshot) bool {
			if s.Page != pageNone || s.Selected < 0 || s.Selected >= len(s.Instances) {
				return false
			}

			s.Page = pageDetails

			return true
		})

		return nil
	case tcell.KeyRune:
		switch ev.Rune() {
		case 'j':
			return c.verticalKey(tcell.KeyDown)
		case 'k':
			return c.verticalKey(tcell.KeyUp)
		case '?':
			c.set(func(s *Snapshot) bool {
				if s.Page == pageHelp {
					s.Page = pageNone
				} else {
					s.Page = pageHelp
				}

				return true
			})

			return nil
		}
	default:
		return ev
	}

	return ev
}

// verticalKey scrolls a focused events log, and moves the instance selection otherwise.
func (c *console) verticalKey(key tcell.Key) *tcell.EventKey {
	if c.view.eventsFocused() {
		return c.scrollLog(key)
	}

	return c.moveKey(key)
}

// scrollLog moves the events log by one line, one page, or to either end.
func (c *console) scrollLog(key tcell.Key) *tcell.EventKey {
	v := c.view
	if v.log == nil {
		return nil
	}

	_, _, w, h := v.log.GetInnerRect()
	_ = w

	height := max(h, 1)
	lines := strings.Split(strings.TrimRight(v.log.GetText(false), "\n"), "\n")
	top := max(len(lines)-height, 0)

	row := v.logRow
	if !v.logScrolled {
		row = top
	}

	switch key {
	case tcell.KeyUp:
		row--
	case tcell.KeyDown:
		row++
	case tcell.KeyPgUp:
		row -= height
	case tcell.KeyPgDn:
		row += height
	case tcell.KeyHome:
		row = 0
	case tcell.KeyEnd:
		row = top
	default:
		return nil
	}

	row = min(max(row, 0), top)
	v.logRow, v.logScrolled = row, row != top

	if row == top {
		v.log.ScrollToEnd()
	} else {
		v.log.ScrollTo(row, 0)
	}

	return nil
}

// moveKey moves the selection, unless a page is in front.
func (c *console) moveKey(key tcell.Key) *tcell.EventKey {
	c.set(func(s *Snapshot) bool {
		if s.Page != pageNone || len(s.Instances) == 0 {
			return false
		}

		was := s.Selected

		switch key {
		case tcell.KeyUp:
			s.Selected = max(s.Selected-1, 0)
		case tcell.KeyDown:
			s.Selected = min(s.Selected+1, len(s.Instances)-1)
		case tcell.KeyPgUp, tcell.KeyHome:
			s.Selected = 0
		case tcell.KeyPgDn, tcell.KeyEnd:
			s.Selected = len(s.Instances) - 1
		default:
			return false
		}

		return s.Selected != was
	})

	return nil
}

// set changes the snapshot under its lock and republishes it when it changed. The
// redraw is queued from another goroutine: this runs on the event loop, where
// QueueUpdateDraw would wait on itself.
func (c *console) set(f func(*Snapshot) bool) bool {
	c.mu.Lock()
	changed := f(&c.snap)
	c.mu.Unlock()

	if changed {
		go c.publish()
	}

	return changed
}
