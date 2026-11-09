package wifiui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/arca/wifi"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

func TestCommitWaitsOnGuards(t *testing.T) { //nolint:paralleltest // replaces the package's apply function
	savedApply, savedTick := applyFunc, guardTick

	t.Cleanup(func() { applyFunc, guardTick = savedApply, savedTick })

	saved := api.SystemNetworkWirelessNetwork{SSID: "Casa", Security: api.SystemNetworkWirelessSecurityOpen}
	chosen := api.SystemNetworkWirelessNetwork{SSID: "Velho", Security: api.SystemNetworkWirelessSecurityWPAPersonal, Passphrase: "abcdefgh"}

	for _, guard := range []string{"in process", "pending"} {
		s := &state.State{}
		s.System.Network.Config = &api.SystemNetworkConfig{Wireless: &api.SystemNetworkWireless{
			Addresses: []string{"dhcp4"},
			Networks:  []api.SystemNetworkWirelessNetwork{saved},
		}}

		set := func(v bool) {
			if guard == "pending" {
				s.NetworkConfigurationPending = v
			} else {
				s.System.Network.State.ConfigurationInProcess = v
			}
		}

		set(true)

		calls, ticks := 0, 0
		statuses := []string{}

		var applied *api.SystemNetworkConfig

		applyFunc = func(_ context.Context, _ *state.State, cfg *api.SystemNetworkConfig) error {
			calls++
			applied = cfg

			return nil
		}

		guardTick = func(context.Context) error {
			ticks++

			if calls != 0 {
				t.Errorf("%s: the apply ran while the guard was set", guard)
			}

			if ticks == 3 {
				set(false)
			}

			return nil
		}

		err := commit(context.Background(), s, chosen, func(text string) { statuses = append(statuses, text) })
		if err != nil {
			t.Fatal(err)
		}

		if ticks != 3 || calls != 1 {
			t.Errorf("%s: %d ticks and %d apply calls, want 3 and 1", guard, ticks, calls)
		}

		if len(statuses) == 0 || statuses[0] != "Waiting for another network change" {
			t.Errorf("%s: the status read %q", guard, statuses)
		}

		if applied == nil || len(applied.Wireless.Networks) != 2 || applied.Wireless.Networks[0] != chosen || applied.Wireless.Networks[1] != saved {
			t.Errorf("%s: applied %+v, want the chosen network first", guard, applied)
		}

		if len(s.System.Network.Config.Wireless.Networks) != 1 {
			t.Errorf("%s: the commit changed the current config in place", guard)
		}
	}
}

func TestPassphraseFieldCap(t *testing.T) {
	t.Parallel()

	if !passphraseAccept(strings.Repeat("a", wifi.MaxPassphraseBytes), 'a') {
		t.Error("128 bytes were refused")
	}

	if passphraseAccept(strings.Repeat("a", wifi.MaxPassphraseBytes+1), 'a') {
		t.Error("the 129th byte was accepted")
	}

	for _, text := range []string{"abc\x01", "abc\t", "abc\x7f", "abc\x1b"} {
		if passphraseAccept(text, rune(text[len(text)-1])) || passphraseAccept(text, 0) {
			t.Errorf("a control character was accepted in %q", text)
		}
	}
}

// appOverlay is the console's overlay on a tview application, the way *tui.TUI
// does it: a page on the console's pages, and the focus on a widget of it.
type appOverlay struct {
	app   *tview.Application
	pages *tview.Pages
}

func (o *appOverlay) ShowArcaOverlay(root tview.Primitive, focus tview.Primitive) {
	o.app.QueueUpdateDraw(func() {
		o.pages.AddPage("arca", root, true, true)
		o.app.SetFocus(focus)
	})
}

func (o *appOverlay) HideArcaOverlay() {
	o.app.QueueUpdateDraw(func() { o.pages.RemovePage("arca") })
}

func (o *appOverlay) QueueArcaUpdate(f func()) { o.app.QueueUpdateDraw(f) }

// onLoop runs f on the application's event loop and returns its result.
func onLoop[T any](app *tview.Application, f func() T) T {
	var v T

	app.QueueUpdate(func() { v = f() })

	return v
}

func TestPickerRootFocus(t *testing.T) {
	t.Parallel()

	screen := tcell.NewSimulationScreen("UTF-8")

	pages := tview.NewPages().AddPage("frame", tview.NewBox(), true, true)
	app := tview.NewApplication().SetScreen(screen).SetRoot(pages, true)

	done := make(chan error, 1)

	go func() { done <- app.Run() }()

	t.Cleanup(func() {
		app.Stop()

		err := <-done
		if err != nil {
			t.Errorf("the application stopped with %v", err)
		}
	})

	p := newPicker(context.Background(), &state.State{}, &appOverlay{app: app, pages: pages})
	p.shown = true
	p.t.ShowArcaOverlay(p.root, p.list)

	waitFocus := func(what string, want tview.Primitive) {
		t.Helper()

		for range 500 {
			if onLoop(app, app.GetFocus) == want {
				return
			}

			time.Sleep(10 * time.Millisecond)
		}

		t.Fatalf("the focus did not move to the %s", what)
	}

	// The console refocuses its top page every second while it shows no modal.
	refocus := func() { app.QueueUpdateDraw(func() { pages.RemovePage("modal") }) }

	waitFocus("list", p.list)

	// focus is called from key handlers, on the event loop.
	app.QueueUpdate(func() { p.focus(p.field) })
	waitFocus("passphrase field", p.field)
	refocus()

	if onLoop(app, app.GetFocus) != p.field {
		t.Fatal("refocusing the console's pages took the focus off the passphrase field")
	}

	screen.InjectKey(tcell.KeyRune, 'x', tcell.ModNone)

	for i := 0; onLoop(app, p.field.GetText) != "x"; i++ {
		if i == 500 {
			t.Fatal("a typed key did not reach the passphrase field")
		}

		time.Sleep(10 * time.Millisecond)
	}

	app.QueueUpdate(func() { p.focus(p.list) })
	waitFocus("list again", p.list)
	refocus()

	if onLoop(app, app.GetFocus) != p.list {
		t.Error("refocusing the console's pages took the focus off the list")
	}
}

func TestCommitWaitTimesOut(t *testing.T) { //nolint:paralleltest // replaces the package's apply function and guard timing
	savedApply, savedTick, savedWait := applyFunc, guardTick, guardWait

	t.Cleanup(func() { applyFunc, guardTick, guardWait = savedApply, savedTick, savedWait })

	guardWait = 50 * time.Millisecond
	guardTick = func(ctx context.Context) error { return sleep(ctx, 5*time.Millisecond) }
	applyFunc = func(context.Context, *state.State, *api.SystemNetworkConfig) error {
		t.Error("the apply ran while another network change was running")

		return nil
	}

	s := &state.State{}
	s.System.Network.State.ConfigurationInProcess = true
	statuses := []string{}

	err := commit(context.Background(), s, api.SystemNetworkWirelessNetwork{SSID: "Velho", Security: api.SystemNetworkWirelessSecurityOpen}, func(text string) { statuses = append(statuses, text) })
	if !errors.Is(err, errGuardTimeout) {
		t.Errorf("commit returned %v, want the wait to time out", err)
	}

	if len(statuses) != 2 || !strings.HasPrefix(statuses[1], "Another network change is still running after 50ms") {
		t.Errorf("the status read %q", statuses)
	}
}

func TestWithFirstNilConfig(t *testing.T) {
	t.Parallel()

	n := api.SystemNetworkWirelessNetwork{SSID: "Velho", Security: api.SystemNetworkWirelessSecurityOpen}

	next, err := withFirst(nil, n)
	if err != nil || next == nil || next.Wireless == nil || len(next.Wireless.Networks) != 1 || next.Wireless.Networks[0] != n || len(next.Wireless.Addresses) == 0 {
		t.Errorf("withFirst(nil) = %+v, %v; want the defaults with the network", next, err)
	}
}

// inlineOverlay runs every update at once and records the status line.
type inlineOverlay struct {
	mu sync.Mutex
}

func (*inlineOverlay) ShowArcaOverlay(tview.Primitive, tview.Primitive) {}

func (*inlineOverlay) HideArcaOverlay() {}

func (o *inlineOverlay) QueueArcaUpdate(f func()) {
	o.mu.Lock()
	defer o.mu.Unlock()

	f()
}

func TestConnectIgnoresSecondEnter(t *testing.T) { //nolint:paralleltest // replaces the package's apply function
	savedApply := applyFunc

	t.Cleanup(func() { applyFunc = savedApply })

	started, release := make(chan struct{}, 4), make(chan struct{})

	var calls atomic.Int32

	applyFunc = func(context.Context, *state.State, *api.SystemNetworkConfig) error {
		calls.Add(1)

		started <- struct{}{}

		<-release

		return nil
	}

	o := &inlineOverlay{}
	p := newPicker(context.Background(), &state.State{}, o)
	p.entries = []wifi.Entry{
		{SSID: "Velho", Security: api.SystemNetworkWirelessSecurityOpen, Selectable: true},
		{SSID: "Casa", Security: api.SystemNetworkWirelessSecurityWPAPersonal, Selectable: true},
	}

	p.choose(0)
	<-started

	// A second Enter on the list, and on the passphrase field, while the commit runs.
	p.choose(1)
	p.connect("abcdefgh")

	o.mu.Lock()
	label, chosen := p.field.GetLabel(), p.chosen.SSID
	o.mu.Unlock()

	if label != "" || chosen != "Velho" {
		t.Errorf("a second Enter during a commit asked for a passphrase (label %q, chosen %q)", label, chosen)
	}

	close(release)

	for i := 0; p.committing.Load(); i++ {
		if i == 500 {
			t.Fatal("the commit did not finish")
		}

		time.Sleep(10 * time.Millisecond)
	}

	if calls.Load() != 1 {
		t.Errorf("%d applies, want 1", calls.Load())
	}

	// Once the commit is done, Enter works again.
	p.choose(0)
	<-started

	if calls.Load() != 2 {
		t.Errorf("%d applies after the commit, want 2", calls.Load())
	}
}
