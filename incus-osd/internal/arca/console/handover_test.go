package console

import (
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// testPhrase is a passphrase whose 2nd and 5th words are "ability" and "above".
const testPhrase = "abandon ability able about above absent absorb abstract"

// typeWords types each rune of s on the console.
func typeWords(c *console, s string) {
	for _, r := range s {
		keyOf(c, tcell.KeyRune, r)
	}
}

// handOverOf reads the console's passphrase page.
func handOverOf(c *console) handOver {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.snap.HandOver
}

// screenOf draws the console's snapshot on an 80×24 screen.
func screenOf(t *testing.T, c *console) tcell.SimulationScreen {
	t.Helper()

	c.mu.Lock()
	snap := c.snap.clone()
	c.mu.Unlock()

	return draw(t, snap, 80, 24)
}

// closes counts how the passphrase page closed.
type closes struct {
	confirmed, other atomic.Int32
}

// startHandOver opens the passphrase page on a new console.
func startHandOver(t *testing.T, wait bool, showFor time.Duration) (*console, *fakeHost, *closes) {
	t.Helper()

	h := &fakeHost{}
	c := newConsole(h, staticSources())
	n := &closes{}

	c.handOver(t.Context(), handOverReq{
		phrase:  testPhrase,
		wait:    wait,
		showFor: showFor,
		done: func(confirmed bool) {
			if confirmed {
				n.confirmed.Add(1)
			} else {
				n.other.Add(1)
			}
		},
	})

	return c, h, n
}

func TestHandOverShowsTheWords(t *testing.T) {
	t.Parallel()

	c, h, _ := startHandOver(t, true, time.Hour)

	if pageOf(c) != pageRecovery || !h.held {
		t.Fatal("the passphrase page is not in front, or the console is not held")
	}

	screen := screenOf(t, c)
	golden(t, "handover-80x24.txt", screen)

	for i, w := range strings.Fields(testPhrase) {
		if rowOf(screen, w) < 0 {
			t.Fatalf("word %d (%s) is not on the screen:\n%s", i+1, w, screenText(screen))
		}
	}

	for _, s := range []string{"TPM refuses", "Arca keeps a copy", "the disk and", "type words 2 and 5"} {
		if rowOf(screen, s) < 0 {
			t.Fatalf("the page doesn't say %q:\n%s", s, screenText(screen))
		}
	}

	// Esc and the dashboard's keys don't leave the page while a confirmation is owed.
	for _, k := range []*tcell.EventKey{tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone), tcell.NewEventKey(tcell.KeyRune, 'U', tcell.ModNone), tcell.NewEventKey(tcell.KeyRune, '?', tcell.ModNone)} {
		if c.key(k) != nil || pageOf(c) != pageRecovery {
			t.Fatalf("%v left the passphrase page", k.Name())
		}
	}
}

func TestHandOverWrongWords(t *testing.T) {
	t.Parallel()

	c, h, n := startHandOver(t, true, time.Hour)

	keyOf(c, tcell.KeyEnter, 0)

	if !handOverOf(c).Confirm || rowOf(screenOf(t, c), "ability") >= 0 {
		t.Fatalf("Enter did not hide the words for the confirmation:\n%s", screenText(screenOf(t, c)))
	}

	golden(t, "handover-confirm-80x24.txt", screenOf(t, c))

	// Word 5 typed in place of word 2.
	typeWords(c, "above")
	keyOf(c, tcell.KeyEnter, 0)
	typeWords(c, "ability")
	keyOf(c, tcell.KeyEnter, 0)

	ho := handOverOf(c)
	if ho.Confirm || !ho.Missed || ho.Typed != [2]string{} || pageOf(c) != pageRecovery || !h.held {
		t.Fatalf("a wrong answer did not show the passphrase again: %+v", ho)
	}

	screen := screenOf(t, c)
	if rowOf(screen, "did not match") < 0 || rowOf(screen, "ability") < 0 {
		t.Fatalf("the page doesn't show the passphrase again:\n%s", screenText(screen))
	}

	time.Sleep(50 * time.Millisecond)

	if n.confirmed.Load() != 0 || n.other.Load() != 0 {
		t.Fatal("a wrong answer closed the page or confirmed the passphrase")
	}
}

func TestHandOverRightWords(t *testing.T) {
	t.Parallel()

	c, h, n := startHandOver(t, true, time.Hour)

	keyOf(c, tcell.KeyEnter, 0)
	typeWords(c, "ABILx")
	keyOf(c, tcell.KeyBackspace2, 0)
	typeWords(c, "ity above")
	keyOf(c, tcell.KeyEnter, 0)

	waitFor(t, func() bool { return n.confirmed.Load() == 1 })

	if n.other.Load() != 0 || pageOf(c) != pageNone || handOverOf(c) != (handOver{}) || h.held {
		t.Fatalf("the page is still open after the right words: %+v", handOverOf(c))
	}

	if rowOf(screenOf(t, c), "ability") >= 0 {
		t.Fatal("the passphrase is still on the screen")
	}
}

func TestHandOverAcknowledged(t *testing.T) {
	t.Parallel()

	// Acknowledged by the install seed: shown without asking for words, then closed.
	c, h, n := startHandOver(t, false, 100*time.Millisecond)

	screen := screenOf(t, c)
	if rowOf(screen, "ability") < 0 || rowOf(screen, "skips the confirmation") < 0 || rowOf(screen, "type words 2 and 5") >= 0 {
		t.Fatalf("the acknowledged page is wrong:\n%s", screenText(screen))
	}

	waitFor(t, func() bool { return pageOf(c) == pageNone })

	waitFor(t, func() bool { return n.other.Load() == 1 })

	if h.held || n.confirmed.Load() != 0 {
		t.Fatal("the acknowledged page held the console or confirmed the passphrase")
	}

	// Enter closes it at once, again without a confirmation.
	c, _, n = startHandOver(t, false, time.Hour)
	keyOf(c, tcell.KeyEnter, 0)

	waitFor(t, func() bool { return n.other.Load() == 1 })

	if pageOf(c) != pageNone || n.confirmed.Load() != 0 {
		t.Fatal("Enter did not close the acknowledged page")
	}
}

// TestHandOverState runs the first boot's hand-over on a state: asked before the
// console starts, retrieved stays false until the owner types the right words.
func TestHandOverState(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	t.Cleanup(func() {
		handOvers.mu.Lock()
		handOvers.c, handOvers.pending, handOvers.pendingCtx = nil, nil, nil
		handOvers.mu.Unlock()
	})

	path := filepath.Join(t.TempDir(), "state.txt")

	s, err := state.LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}

	s.System.Security.Config.EncryptionRecoveryKeys = []string{testPhrase}
	s.RecoveryPassphrasePending = true

	saved := func() *state.State {
		saved, err := state.LoadOrCreate(path)
		if err != nil {
			t.Fatal(err)
		}

		return saved
	}

	HandOver(ctx, s)

	c := newConsole(&fakeHost{}, staticSources())
	c.register()

	if pageOf(c) != pageRecovery {
		t.Fatal("the hand-over asked before the console started is not shown")
	}

	keyOf(c, tcell.KeyEnter, 0)
	typeWords(c, "able about")
	keyOf(c, tcell.KeyEnter, 0)
	time.Sleep(50 * time.Millisecond)

	if s.System.Security.State.EncryptionRecoveryKeysRetrieved || !s.RecoveryPassphrasePending {
		t.Fatal("wrong words marked the keys retrieved or ended the hand-over")
	}

	keyOf(c, tcell.KeyEnter, 0)
	typeWords(c, "ability above")
	keyOf(c, tcell.KeyEnter, 0)

	waitFor(t, func() bool {
		st := saved()

		return st.System.Security.State.EncryptionRecoveryKeysRetrieved && !st.RecoveryPassphrasePending
	})

	// Marked retrieved through the API, an open page closes by itself, without confirming.
	s.System.Security.State.EncryptionRecoveryKeysRetrieved = false
	s.RecoveryPassphrasePending = true
	HandOver(ctx, s)

	if pageOf(c) != pageRecovery {
		t.Fatal("the second hand-over is not shown")
	}

	s.System.Security.State.EncryptionRecoveryKeysRetrieved = true

	waitFor(t, func() bool { return pageOf(c) == pageNone && !saved().RecoveryPassphrasePending })

	// A first key that is no passphrase is never shown, and nothing is pending.
	s.System.Security.Config.EncryptionRecoveryKeys = []string{"cbdefghi-jklnrtuv"}
	s.RecoveryPassphrasePending = true
	HandOver(ctx, s)

	if pageOf(c) != pageNone || s.RecoveryPassphrasePending {
		t.Fatal("a key that is no passphrase was handed over")
	}
}
