package console

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/lxc/incus-os/incus-osd/internal/passphrase"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// The words the owner re-types to confirm the passphrase, counted from 1.
const (
	confirmFirst  = 2
	confirmSecond = 5
)

// acknowledgedFor is how long the passphrase shows when the install seed acknowledged it.
const acknowledgedFor = time.Minute

// handOver is the recovery passphrase page. Phrase is empty once the page closed.
type handOver struct {
	Phrase  string
	Wait    bool      // the owner must re-type two words; false when the install seed acknowledged it
	Confirm bool      // the words are hidden and the owner types two of them
	Missed  bool      // the last confirmation did not match
	Typed   [2]string // the two words as typed
	Field   int       // the word being typed
}

// handOverReq is one hand-over: the passphrase, and what closing the page does.
type handOverReq struct {
	phrase    string
	wait      bool
	showFor   time.Duration
	done      func(confirmed bool)
	retrieved func() bool
}

// handOvers holds the running console, and a hand-over asked before it started.
var handOvers struct {
	mu         sync.Mutex
	c          *console
	pending    *handOverReq
	pendingCtx context.Context //nolint:containedctx
}

// HandOver shows the recovery passphrase, the first encryption recovery key of s, on
// the console and returns at once. Unless the install seed acknowledged it, the page
// stays until the owner re-types its 2nd and 5th words, and only then are the keys
// marked retrieved in s; acknowledged, it shows for a minute. The page also closes
// once the keys are marked retrieved through the API. Once the page closed, the
// hand-over is no longer pending in s.
func HandOver(ctx context.Context, s *state.State) {
	keys := s.System.Security.Config.EncryptionRecoveryKeys
	if len(keys) == 0 || !passphrase.Valid(keys[0]) {
		s.RecoveryPassphrasePending = false

		return
	}

	req := handOverReq{
		phrase:  keys[0],
		wait:    !s.RecoveryPassphraseAcknowledged,
		showFor: acknowledgedFor,
		done: func(confirmed bool) {
			if confirmed {
				s.System.Security.State.EncryptionRecoveryKeysRetrieved = true
			}

			s.RecoveryPassphrasePending = false

			err := s.Save()
			if err != nil {
				slog.Error("Console: state could not be saved after the recovery passphrase hand-over", "err", err)
			}
		},
		retrieved: func() bool { return s.System.Security.State.EncryptionRecoveryKeysRetrieved },
	}

	handOvers.mu.Lock()
	c := handOvers.c

	if c == nil {
		handOvers.pending, handOvers.pendingCtx = &req, ctx
		handOvers.mu.Unlock()

		return
	}

	handOvers.mu.Unlock()

	c.handOver(ctx, req)
}

// register makes c the console HandOver uses, and starts a hand-over asked before.
func (c *console) register() {
	handOvers.mu.Lock()
	handOvers.c = c
	req, ctx := handOvers.pending, handOvers.pendingCtx
	handOvers.pending, handOvers.pendingCtx = nil, nil
	handOvers.mu.Unlock()

	if req != nil {
		c.handOver(ctx, *req)
	}
}

// handOver puts the passphrase page in front. It must not run on the event loop.
func (c *console) handOver(ctx context.Context, req handOverReq) {
	c.mu.Lock()
	c.handOverDone = req.done
	c.snap.HandOver = handOver{Phrase: req.phrase, Wait: req.wait}
	c.snap.Page = pageRecovery
	c.mu.Unlock()

	c.h.HoldArcaConsole(true)
	c.publish()

	slog.InfoContext(ctx, "Console: recovery passphrase shown", "confirmation_required", req.wait)

	go c.watchHandOver(ctx, req)
}

// watchHandOver closes the page when an acknowledged hand-over has shown long enough,
// or when the keys were marked retrieved elsewhere.
func (c *console) watchHandOver(ctx context.Context, req handOverReq) {
	timer := time.NewTimer(req.showFor)
	defer timer.Stop()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if !req.wait {
				c.closeHandOver("shown for "+req.showFor.String(), false)

				return
			}
		case <-tick.C:
		}

		if !c.handOverOpen() {
			return
		}

		if req.retrieved != nil && req.retrieved() {
			c.closeHandOver("keys marked retrieved", false)

			return
		}
	}
}

// handOverOpen reports whether the passphrase page is open.
func (c *console) handOverOpen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.snap.HandOver.Phrase != ""
}

// closeHandOver closes the passphrase page, forgets the passphrase and runs the
// hand-over's done once, off the event loop.
func (c *console) closeHandOver(why string, confirmed bool) {
	var done func(bool)

	if !c.set(func(s *Snapshot) bool {
		if s.HandOver.Phrase == "" {
			return false
		}

		s.HandOver = handOver{}
		if s.Page == pageRecovery {
			s.Page = pageNone
		}

		done, c.handOverDone = c.handOverDone, nil

		return true
	}) {
		return
	}

	c.h.HoldArcaConsole(false)

	go func() {
		slog.Info("Console: recovery passphrase page closed", "reason", why)

		if done != nil {
			done(confirmed)
		}
	}()
}

// handOverKey takes every key while the passphrase page is open. It runs on the event loop.
func (c *console) handOverKey(ev *tcell.EventKey) bool {
	c.mu.Lock()
	h := c.snap.HandOver
	c.mu.Unlock()

	if h.Phrase == "" {
		return false
	}

	switch {
	case !h.Confirm && ev.Key() == tcell.KeyEnter && !h.Wait,
		!h.Confirm && ev.Key() == tcell.KeyEsc && !h.Wait:
		c.closeHandOver("closed by the owner", false)
	case !h.Confirm && ev.Key() == tcell.KeyEnter:
		c.editHandOver(func(h *handOver) { h.Confirm, h.Missed, h.Typed, h.Field = true, false, [2]string{}, 0 })
	case h.Confirm:
		c.confirmKey(ev, h)
	default:
	}

	return true
}

// confirmKey types into the confirmation, and checks it on the second Enter.
func (c *console) confirmKey(ev *tcell.EventKey, h handOver) {
	switch ev.Key() { //nolint:exhaustive // tcell's key set is open; the default covers every other key
	case tcell.KeyEsc:
		c.editHandOver(func(h *handOver) { h.Confirm, h.Typed, h.Field = false, [2]string{}, 0 })
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		c.editHandOver(func(h *handOver) {
			if h.Typed[h.Field] == "" {
				h.Field = 0
			} else {
				h.Typed[h.Field] = h.Typed[h.Field][:len(h.Typed[h.Field])-1]
			}
		})
	case tcell.KeyTab, tcell.KeyBacktab:
		c.editHandOver(func(h *handOver) { h.Field = 1 - h.Field })
	case tcell.KeyEnter:
		if h.Field == 0 {
			c.editHandOver(func(h *handOver) { h.Field = 1 })

			return
		}

		if h.Typed[0] == passphrase.Word(h.Phrase, confirmFirst) && h.Typed[1] == passphrase.Word(h.Phrase, confirmSecond) {
			c.closeHandOver("confirmed by the owner", true)

			return
		}

		go slog.Warn("Console: recovery passphrase confirmation did not match, showing it again")

		c.editHandOver(func(h *handOver) { h.Confirm, h.Missed, h.Typed, h.Field = false, true, [2]string{}, 0 })
	case tcell.KeyRune:
		r := ev.Rune()
		if r == ' ' {
			c.editHandOver(func(h *handOver) { h.Field = 1 })

			return
		}

		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}

		if r < 'a' || r > 'z' {
			return
		}

		c.editHandOver(func(h *handOver) {
			if len(h.Typed[h.Field]) < 12 {
				h.Typed[h.Field] += string(r)
			}
		})
	default:
	}
}

// editHandOver changes the open passphrase page.
func (c *console) editHandOver(f func(*handOver)) {
	c.set(func(s *Snapshot) bool {
		if s.HandOver.Phrase == "" {
			return false
		}

		f(&s.HandOver)

		return true
	})
}

// handOverLines is the passphrase page's text.
func handOverLines(h handOver) []string {
	if h.Confirm {
		return confirmLines(h)
	}

	lines := []string{}

	if h.Missed {
		lines = append(lines, tagBad+"Those words did not match. Here is the passphrase again."+tagReset, "")
	}

	lines = append(lines, tagBold+"Write these 8 words down, in this order, and keep them safe:"+tagReset, "")

	words := strings.Fields(h.Phrase)
	for row := 0; row < len(words); row += 4 {
		cells := []string{"   "}

		for i := row; i < min(row+4, len(words)); i++ {
			cells = append(cells, fmt.Sprintf("%s%d%s %s%-9s%s", tagDim, i+1, tagReset, tagSel, safe(words[i]), tagReset))
		}

		lines = append(lines, strings.Join(cells, " "))
	}

	lines = append(lines, "",
		"This is the recovery passphrase of this machine's disk. It is asked for at",
		"start-up when the TPM refuses to unlock the disk, for example after a",
		"firmware, Secure Boot or hardware change: type the 8 words, separated by",
		"single spaces, then press Enter.",
		"",
		"Arca keeps a copy in its encrypted settings, readable only once the disk",
		"is unlocked. "+tagWarn+"If you lose this passphrase and the TPM refuses, the disk and"+tagReset,
		tagWarn+"everything on it are lost."+tagReset,
		"")

	if h.Wait {
		return append(lines, tagKey+"Enter"+tagReset+" when written down, then type words 2 and 5 to confirm.")
	}

	return append(lines, tagDim+"This install skips the confirmation: the page closes in a minute."+tagReset,
		tagKey+"Enter"+tagReset+tagDim+" closes it now."+tagReset)
}

// confirmLines is the confirmation step's text, with the words hidden.
func confirmLines(h handOver) []string {
	lines := []string{tagBold + "Confirm the passphrase: type its 2nd and 5th words." + tagReset, ""}

	for i, n := range []int{confirmFirst, confirmSecond} {
		mark, cursor := "  ", ""
		if i == h.Field {
			mark, cursor = tagSel+"▶ "+tagReset, tagSel+"_"+tagReset
		}

		lines = append(lines, fmt.Sprintf("%sWord %d: %s%s", mark, n, safe(h.Typed[i]), cursor))
	}

	return append(lines, "",
		tagDim+"Lowercase letters. Enter moves on, then checks · Backspace erases"+tagReset,
		tagDim+"Esc shows the passphrase again"+tagReset)
}

// drawHandOver draws the passphrase page in r.
func drawHandOver(screen tcell.Screen, s Snapshot, r rect) {
	box(screen, r, "Recovery passphrase")

	x, y, w, h := r.X+1, r.Y+1, r.W-2, r.H-2
	if w <= 0 || h <= 0 {
		return
	}

	for i, line := range handOverLines(s.HandOver) {
		if i >= h {
			break
		}

		tview.Print(screen, " "+line, x, y+i, w, tview.AlignLeft, tcell.ColorWhite)
	}
}
