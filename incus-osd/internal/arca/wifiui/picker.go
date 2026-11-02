package wifiui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/arca/wifi"
	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/tui"
)

var errNoDevice = errors.New("no Wi-Fi device answered")

// picker is the console page that lists networks and joins one.
type picker struct {
	ctx context.Context //nolint:containedctx
	s   *state.State
	t   *tui.TUI

	root   *tview.Flex
	list   *tview.List
	field  *tview.InputField
	status *tview.TextView

	mu     sync.Mutex
	shown  bool
	stop   chan struct{}
	rescan chan struct{}

	// Used on the TUI's event loop only.
	entries    []wifi.Entry
	chosen     wifi.Entry
	scanStatus bool
}

// passphraseAccept refuses passphrase input over 128 bytes, not UTF-8, or with a control character.
func passphraseAccept(text string, _ rune) bool {
	return wifi.PassphraseValid(text)
}

func newPicker(ctx context.Context, s *state.State, t *tui.TUI) *picker {
	p := &picker{ctx: ctx, s: s, t: t, rescan: make(chan struct{}, 1)}

	p.list = tview.NewList().ShowSecondaryText(false).SetSelectedFunc(func(i int, _ string, _ string, _ rune) { p.choose(i) })
	p.list.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Rune() == 'r' || event.Rune() == 'R' {
			p.requestScan()

			return nil
		}

		return event
	})

	p.field = tview.NewInputField().SetMaskCharacter('*').SetAcceptanceFunc(passphraseAccept).SetDoneFunc(p.passphraseDone)
	p.status = tview.NewTextView().SetText("Scanning...")

	p.root = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(tview.NewTextView().SetText("Connect to Wi-Fi"), 1, 0, false).
		AddItem(p.list, 0, 1, true).
		AddItem(p.field, 1, 0, false).
		AddItem(p.status, 1, 0, false).
		AddItem(tview.NewTextView().SetText("Up/Down choose - Enter connect - R rescan"), 1, 0, false)
	p.root.SetBorder(true)

	return p
}

// show puts the picker on the console and starts its rescans, every 10 s.
func (p *picker) show() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.shown {
		return
	}

	p.shown = true
	p.stop = make(chan struct{})
	p.t.ShowArcaOverlay(p.root, p.list)
	slog.InfoContext(p.ctx, "Wi-Fi: picker shown")

	go p.scanLoop(p.stop)
}

// hide closes the picker.
func (p *picker) hide() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.shown {
		return
	}

	p.shown = false
	close(p.stop)
	p.t.HideArcaOverlay()
	slog.InfoContext(p.ctx, "Wi-Fi: picker closed")
}

// focus moves the keyboard to one of the picker's widgets, if it is shown.
func (p *picker) focus(widget tview.Primitive) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.shown {
		p.t.ShowArcaOverlay(p.root, widget)
	}
}

func (p *picker) requestScan() {
	select {
	case p.rescan <- struct{}{}:
	default:
	}
}

func (p *picker) scanLoop(stop chan struct{}) {
	for {
		p.scan()

		timer := time.NewTimer(10 * time.Second)

		select {
		case <-stop:
			timer.Stop()

			return
		case <-p.rescan:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// scan lists the networks the first answering Wi-Fi device sees.
func (p *picker) scan() {
	devices, _ := wifi.Devices(sysRoot)
	results := []wifi.BSS{}
	err := errNoDevice

	for _, device := range devices {
		results, err = scanDevice(p.ctx, device)
		if err == nil {
			break
		}
	}

	entries := wifi.Rank(results)

	p.t.QueueArcaUpdate(func() { p.setEntries(entries, err) })
}

func scanDevice(ctx context.Context, device string) ([]wifi.BSS, error) {
	conn, err := wifi.Dial(wifi.CtrlDir, device)
	if err != nil {
		return nil, err
	}

	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return conn.Scan(ctx)
}

// setEntries replaces the list, keeping the selected network selected.
func (p *picker) setEntries(entries []wifi.Entry, err error) {
	selected := ""

	i := p.list.GetCurrentItem()
	if i >= 0 && i < len(p.entries) {
		selected = p.entries[i].SSID
	}

	p.entries = entries
	p.list.Clear()

	for i, e := range entries {
		p.list.AddItem(row(e), "", 0, nil)

		if e.SSID == selected {
			p.list.SetCurrentItem(i)
		}
	}

	switch {
	case len(entries) == 0 && err != nil && (p.scanStatus || p.status.GetText(true) == "Scanning..."):
		p.setStatus("Scanning... ("+err.Error()+")", true)
	case len(entries) == 0 && (p.scanStatus || p.status.GetText(true) == "Scanning..."):
		p.setStatus("No Wi-Fi network found yet; scanning again", true)
	case len(entries) > 0 && p.scanStatus:
		p.setStatus("", false)
	}
}

func row(e wifi.Entry) string {
	security := "Open"

	switch {
	case e.Security == "enterprise":
		security = "Enterprise (not supported)"
	case e.Security == "wep":
		security = "WEP (not supported)"
	case e.SAE && e.PSK:
		security = "WPA2/WPA3"
	case e.SAE:
		security = "WPA3"
	case e.PSK:
		security = "WPA2"
	}

	line := fmt.Sprintf("%-32s %4d dBm  %s", tview.Escape(e.SSID), e.Signal, security)
	if e.NotRecommended {
		line += "  2.4 GHz - not recommended"
	}

	return line
}

func (p *picker) setStatus(text string, fromScan bool) {
	p.status.SetText(text)
	p.scanStatus = fromScan
}

// choose handles Enter on a network: an open one is joined, a WPA-Personal one asks for its passphrase.
func (p *picker) choose(i int) {
	if i < 0 || i >= len(p.entries) {
		return
	}

	e := p.entries[i]
	if !e.Selectable {
		p.setStatus(e.SSID+" is not supported", false)

		return
	}

	p.chosen = e

	if e.Security == api.SystemNetworkWirelessSecurityOpen {
		p.connect("")

		return
	}

	p.field.SetLabel("Passphrase for " + tview.Escape(e.SSID) + ": ").SetText("")
	p.setStatus("Type the passphrase, then Enter", false)
	p.focus(p.field)
}

func (p *picker) passphraseDone(key tcell.Key) {
	text := p.field.GetText()

	p.field.SetLabel("").SetText("")
	p.focus(p.list)

	if key == tcell.KeyEnter && text != "" {
		p.connect(text)
	} else {
		p.setStatus("", false)
	}
}

// connect commits the chosen network in the background; the status line follows it.
func (p *picker) connect(passphrase string) {
	n := api.SystemNetworkWirelessNetwork{SSID: p.chosen.SSID, Security: p.chosen.Security, Passphrase: passphrase}
	status := func(text string) { p.t.QueueArcaUpdate(func() { p.setStatus(text, false) }) }

	go func() {
		err := commit(p.ctx, p.s, n, status)
		if err != nil {
			status("Could not connect: " + err.Error())

			return
		}

		status("Connected to " + n.SSID + "; waiting for the update server")
	}()
}
