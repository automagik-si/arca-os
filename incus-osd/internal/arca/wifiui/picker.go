package wifiui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/arca/wifi"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

var errNoDevice = errors.New("no Wi-Fi device answered")

// otherRow is the last row of the list: a network that the scan does not show.
const otherRow = "Other network…"

// step is what the picker is doing: choosing from the list, or one step of "Other network…".
type step int

const (
	stepList       step = iota // the list of networks
	stepName                   // the name of "Other network…", typed in the field
	stepSecurity               // the security of "Other network…", offered as the list's two rows
	stepPassphrase             // the passphrase of a network: a scanned one or "Other network…"
)

// picker is the console page that lists networks and joins one.
type picker struct {
	ctx context.Context //nolint:containedctx
	s   *state.State
	t   overlay

	root   *pickerRoot
	list   *tview.List
	field  *tview.InputField
	status *tview.TextView

	mu     sync.Mutex
	shown  bool
	stop   chan struct{}
	rescan chan struct{}

	// committing is set while a commit runs; Enter is ignored meanwhile.
	committing atomic.Bool

	// Used on the TUI's event loop only.
	entries    []wifi.Entry
	chosen     wifi.Entry
	hidden     bool // the chosen network came from "Other network…", so it is saved hidden
	step       step
	scanStatus bool
}

// pickerRoot is the picker's overlay page. Its focus goes to the widget in use, not
// to its first focusable item: the console refocuses its top page every second
// while it shows no modal, which would otherwise pull the keys off the passphrase field.
type pickerRoot struct {
	*tview.Flex

	mu      sync.Mutex
	current tview.Primitive
}

func (r *pickerRoot) setCurrent(widget tview.Primitive) {
	r.mu.Lock()
	r.current = widget
	r.mu.Unlock()
}

// Focus gives the focus to the widget in use.
func (r *pickerRoot) Focus(delegate func(p tview.Primitive)) {
	r.mu.Lock()
	widget := r.current
	r.mu.Unlock()

	delegate(widget)
}

// passphraseAccept refuses passphrase input over 128 bytes, not UTF-8, or with a control character.
func passphraseAccept(text string, _ rune) bool {
	return wifi.PassphraseValid(text)
}

// ssidAccept refuses a network name that is not 1-32 bytes of UTF-8.
func ssidAccept(text string, _ rune) bool {
	return wifi.SSIDValid(text)
}

func newPicker(ctx context.Context, s *state.State, t overlay) *picker {
	p := &picker{ctx: ctx, s: s, t: t, rescan: make(chan struct{}, 1)}

	p.list = tview.NewList().ShowSecondaryText(false).SetSelectedFunc(func(i int, _ string, _ string, _ rune) { p.choose(i) })
	p.list.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch {
		case event.Key() == tcell.KeyEscape && p.step != stepList:
			p.backToList()

			return nil
		case event.Rune() == 'r' || event.Rune() == 'R':
			p.requestScan()

			return nil
		}

		return event
	})

	p.field = tview.NewInputField().SetMaskCharacter('*').SetAcceptanceFunc(passphraseAccept).SetDoneFunc(p.fieldDone)
	p.status = tview.NewTextView().SetText("Scanning...")

	header := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(tview.NewTextView().SetText("Connect to Wi-Fi"), 0, 1, false).
		AddItem(tview.NewTextView().SetText("No wired internet").SetTextAlign(tview.AlignRight), len("No wired internet"), 0, false)

	p.root = &pickerRoot{current: p.list, Flex: tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(header, 1, 0, false).
		AddItem(p.list, 0, 1, true).
		AddItem(p.field, 1, 0, false).
		AddItem(p.status, 1, 0, false).
		AddItem(tview.NewTextView().SetText("Up/Down choose - Enter connect - R rescan"), 1, 0, false)}
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

// focus moves the keyboard to one of the picker's widgets, if it is shown. It is
// called from key handlers on the TUI's event loop, where a queued update would
// wait for itself (QueueUpdate returns only after the loop ran it), so the
// update is queued from another goroutine.
func (p *picker) focus(widget tview.Primitive) {
	p.root.setCurrent(widget)

	go func() {
		p.mu.Lock()
		defer p.mu.Unlock()

		if p.shown {
			p.t.ShowArcaOverlay(p.root, widget)
		}
	}()
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

// setEntries replaces the list with the scanned networks, keeping the selected one
// selected. While "Other network…" is under way the list is left alone, and the
// rows are kept for when it ends.
func (p *picker) setEntries(entries []wifi.Entry, err error) {
	if p.step != stepList {
		p.entries = entries

		return
	}

	p.rebuildList(entries)

	switch {
	case len(entries) == 0 && err != nil && (p.scanStatus || p.status.GetText(true) == "Scanning..."):
		p.setStatus("Scanning... ("+err.Error()+")", true)
	case len(entries) == 0 && (p.scanStatus || p.status.GetText(true) == "Scanning..."):
		p.setStatus("No Wi-Fi network found yet; scanning again", true)
	case len(entries) > 0 && p.scanStatus:
		p.setStatus("", false)
	}
}

// rebuildList shows the entries and the "Other network…" row. The selection stays on
// the same network, or on "Other network…", when it is still there.
func (p *picker) rebuildList(entries []wifi.Entry) {
	cur := p.list.GetCurrentItem()
	onOther := cur >= len(p.entries)

	selected := ""
	if !onOther && cur >= 0 {
		selected = p.entries[cur].SSID
	}

	p.entries = entries
	p.list.Clear()

	for _, e := range entries {
		p.list.AddItem(row(e), "", 0, nil)
	}

	p.list.AddItem(otherRow, "", 0, nil)

	idx := min(max(cur, 0), len(entries))
	if onOther {
		idx = len(entries)
	}

	for i, e := range entries {
		if selected != "" && e.SSID == selected {
			idx = i
		}
	}

	p.list.SetCurrentItem(idx)
}

// row formats one network as the list shows it: name, Wi-Fi generation, band, width,
// four signal bars and security. A 2.4 GHz network carries a yellow note; a network
// that cannot be joined is greyed out and cannot be chosen. The list reads colour
// tags, so the name is escaped.
func row(e wifi.Entry) string {
	line := fmt.Sprintf("%-24s %-8s %-7s %4d MHz %-4s  %-26s", tview.Escape(printable(e.SSID)), generation(e), band(e.Band), e.Width, bars(e.Signal), security(e))

	if !e.Selectable {
		line = "[gray]" + line + "[-]"
	}

	if e.NotRecommended {
		line += "[yellow]2.4 GHz – not recommended[-]"
	}

	return line
}

// printable returns an SSID as the console may show it: each control character
// (U+0000-U+001F, U+007F-U+009F), an escape sequence's ESC included, becomes U+FFFD.
func printable(ssid string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '\uFFFD'
		}

		return r
	}, ssid)
}

// generation names a network's Wi-Fi generation; HE on 6 GHz is Wi-Fi 6E.
func generation(e wifi.Entry) string {
	switch e.Generation {
	case 7:
		return "Wi-Fi 7"
	case 6:
		if e.Band == "6" {
			return "Wi-Fi 6E"
		}

		return "Wi-Fi 6"
	case 5:
		return "Wi-Fi 5"
	case 4:
		return "Wi-Fi 4"
	}

	return "Older"
}

func band(b string) string {
	switch b {
	case "2.4":
		return "2.4 GHz"
	case "5":
		return "5 GHz"
	case "6":
		return "6 GHz"
	}

	return ""
}

// bars draws four signal bars for the thresholds -55, -67 and -75 dBm.
func bars(level int) string {
	switch {
	case level >= -55:
		return "▂▄▆█"
	case level >= -67:
		return "▂▄▆ "
	case level >= -75:
		return "▂▄  "
	}

	return "▂   "
}

func security(e wifi.Entry) string {
	switch {
	case e.Security == "enterprise":
		return "Enterprise (not supported)"
	case e.Security == "wep":
		return "WEP (not supported)"
	case e.Security == "owe":
		return "OWE (not supported)"
	case e.Security == "unsupported":
		return "Other (not supported)"
	case e.SAE && e.PSK:
		return "WPA2/WPA3"
	case e.SAE:
		return "WPA3"
	case e.PSK:
		return "WPA2"
	case e.Security == api.SystemNetworkWirelessSecurityOpen:
		return "Open"
	}

	return "WPA-Personal"
}

func (p *picker) setStatus(text string, fromScan bool) {
	p.status.SetText(text)
	p.scanStatus = fromScan
}

// choose handles Enter on a row of the list, or on one of the two security rows that
// "Other network…" offers. It does nothing while a commit runs.
func (p *picker) choose(i int) {
	if i < 0 || p.committing.Load() {
		return
	}

	switch p.step {
	case stepList:
		p.chooseNetwork(i)
	case stepSecurity:
		p.chooseSecurity(i)
	}
}

// chooseNetwork handles Enter on a network: an open one is joined, a WPA-Personal one
// asks for its passphrase, and the last row starts "Other network…".
func (p *picker) chooseNetwork(i int) {
	if i == len(p.entries) {
		p.startOther()

		return
	}

	if i > len(p.entries) {
		return
	}

	e := p.entries[i]
	if !e.Selectable {
		p.setStatus(printable(e.SSID)+" is not supported", false)

		return
	}

	p.chosen, p.hidden = e, false

	if e.Security == api.SystemNetworkWirelessSecurityOpen {
		p.connect("")

		return
	}

	p.askPassphrase()
}

// chooseSecurity handles the security row of "Other network…": Open joins at once,
// WPA-Personal asks for the passphrase.
func (p *picker) chooseSecurity(i int) {
	switch i {
	case 0:
		p.chosen.Security = api.SystemNetworkWirelessSecurityOpen
		p.backToList()
		p.connect("")
	case 1:
		p.chosen.Security = api.SystemNetworkWirelessSecurityWPAPersonal
		p.askPassphrase()
	}
}

// startOther begins "Other network…" with the network's name.
func (p *picker) startOther() {
	p.step = stepName
	p.chosen, p.hidden = wifi.Entry{}, true
	p.field.SetAcceptanceFunc(ssidAccept).SetLabel("Network name: ").SetText("")
	p.setStatus("Type the network name, then Enter; Esc cancels", false)
	p.focus(p.field)
}

// showSecurity offers Open and WPA-Personal, as the list's two rows, for the name typed.
func (p *picker) showSecurity() {
	p.step = stepSecurity
	p.field.SetLabel("").SetText("")
	p.list.Clear()
	p.list.AddItem("Open", "", 0, nil)
	p.list.AddItem("WPA-Personal", "", 0, nil)
	p.list.SetCurrentItem(0)
	p.setStatus("Choose the security of "+printable(p.chosen.SSID)+", then Enter; Esc cancels", false)
	p.focus(p.list)
}

// askPassphrase shows the masked passphrase field for the chosen network.
func (p *picker) askPassphrase() {
	p.step = stepPassphrase
	p.field.SetAcceptanceFunc(passphraseAccept).SetLabel("Passphrase for " + tview.Escape(printable(p.chosen.SSID)) + ": ").SetText("")
	p.setStatus("Type the passphrase, then Enter", false)
	p.focus(p.field)
}

// fieldDone handles the field's Enter and Esc. Enter on a name moves on to its
// security, Enter on a passphrase connects, and Esc or an empty passphrase goes back
// to the list.
func (p *picker) fieldDone(key tcell.Key) {
	text := p.field.GetText()

	switch {
	case key != tcell.KeyEnter:
		p.backToList()
	case p.step == stepName && text == "":
		p.setStatus("Type the network name, then Enter; Esc cancels", false)
	case p.step == stepName:
		p.chosen = wifi.Entry{SSID: text}
		p.showSecurity()
	case p.step == stepPassphrase && text != "":
		p.backToList()
		p.connect(text)
	default:
		p.backToList()
	}
}

// backToList ends "Other network…" or a passphrase prompt, and shows the list again.
func (p *picker) backToList() {
	p.step = stepList
	p.field.SetAcceptanceFunc(passphraseAccept).SetLabel("").SetText("")
	p.rebuildList(p.entries)
	p.setStatus("", false)
	p.focus(p.list)
}

// connect commits the chosen network in the background; the status line follows
// it. It does nothing while another commit runs.
func (p *picker) connect(passphrase string) {
	if !p.committing.CompareAndSwap(false, true) {
		return
	}

	n := api.SystemNetworkWirelessNetwork{SSID: p.chosen.SSID, Security: p.chosen.Security, Passphrase: passphrase, Hidden: p.hidden}
	status := func(text string) { p.t.QueueArcaUpdate(func() { p.setStatus(text, false) }) }

	go func() {
		defer p.committing.Store(false)

		err := commit(p.ctx, p.s, n, status)
		if errors.Is(err, errGuardTimeout) {
			return
		}

		if err != nil {
			status("Could not connect: " + err.Error())

			return
		}

		status("Connected to " + printable(n.SSID) + "; waiting for the update server")
	}()
}
