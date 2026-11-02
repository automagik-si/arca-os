package wifiui

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/arca/wifi"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

const (
	testWidth  = 120
	testHeight = 24
)

// drawPicker draws the picker's page on a simulated screen and returns the screen.
func drawPicker(t *testing.T, p *picker) tcell.SimulationScreen {
	t.Helper()

	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}

	screen.SetSize(testWidth, testHeight)
	p.root.SetRect(0, 0, testWidth, testHeight)
	p.root.Draw(screen)

	return screen
}

// screenLine returns the text of one row of a simulated screen.
func screenLine(screen tcell.SimulationScreen, y int) string {
	var b strings.Builder

	for x := range testWidth {
		r, _, _, _ := screen.GetContent(x, y)
		b.WriteRune(r)
	}

	return b.String()
}

// column returns the cell column at which sub starts in a row of the screen, or -1.
func column(line string, sub string) int {
	i := strings.Index(line, sub)
	if i < 0 {
		return -1
	}

	return utf8.RuneCountInString(line[:i])
}

func TestPickerRows(t *testing.T) {
	wpa := api.SystemNetworkWirelessSecurityWPAPersonal
	open := api.SystemNetworkWirelessSecurityOpen

	entries := []wifi.Entry{
		{SSID: "Casa-6E", Security: wpa, SAE: true, Generation: 6, Band: "6", Width: 160, Signal: -50, Selectable: true},
		{SSID: "Casa", Security: wpa, SAE: true, PSK: true, Generation: 6, Band: "5", Width: 80, Signal: -60, Selectable: true},
		{SSID: "Vizinho", Security: wpa, PSK: true, Generation: 5, Band: "5", Width: 80, Signal: -70, Selectable: true},
		{SSID: "Velho", Security: wpa, PSK: true, Generation: 4, Band: "2.4", Width: 20, Signal: -80, NotRecommended: true, Selectable: true},
		{SSID: "Escritorio", Security: "enterprise", Generation: 6, Band: "5", Width: 80, Signal: -45},
		{SSID: "Antigo", Security: "wep", Generation: 0, Band: "2.4", Width: 20, Signal: -40},
		{SSID: "Aberta", Security: open, Generation: 0, Band: "5", Width: 20, Signal: -90, Selectable: true},
	}

	// The labels of each row, as the console shows them.
	labels := map[string][]string{
		"Casa-6E":    {"Wi-Fi 6E", "6 GHz", "160 MHz", "▂▄▆█", "WPA3"},
		"Casa":       {"Wi-Fi 6 ", "5 GHz", "80 MHz", "▂▄▆ ", "WPA2/WPA3"},
		"Vizinho":    {"Wi-Fi 5", "5 GHz", "80 MHz", "▂▄  ", "WPA2"},
		"Velho":      {"Wi-Fi 4", "2.4 GHz", "20 MHz", "2.4 GHz – not recommended"},
		"Escritorio": {"Enterprise (not supported)"},
		"Antigo":     {"WEP (not supported)", "Older"},
		"Aberta":     {"Older", "▂   ", "Open"},
	}

	p := newPicker(context.Background(), &state.State{}, &inlineOverlay{})
	p.setEntries(entries, nil)

	screen := drawPicker(t, p)

	rows := map[string]int{}

	for y := range testHeight {
		line := screenLine(screen, y)
		for _, e := range entries {
			// A name is padded to its column, so "Casa " is the Casa row and not Casa-6E's.
			if strings.Contains(line, e.SSID+" ") {
				rows[e.SSID] = y
			}
		}
	}

	for _, e := range entries {
		y, ok := rows[e.SSID]
		if !ok {
			t.Errorf("%s is not on the screen", e.SSID)

			continue
		}

		line := screenLine(screen, y)
		for _, want := range labels[e.SSID] {
			if !strings.Contains(line, want) {
				t.Errorf("the %s row lacks %q: %q", e.SSID, want, strings.TrimSpace(line))
			}
		}
	}

	if !strings.Contains(screenLine(screen, 1), "No wired internet") {
		t.Error("the header lacks its right side")
	}

	// The enterprise row is greyed: its text is in another colour than a plain row's.
	fg := func(line string, sub string, y int) tcell.Color {
		x := column(line, sub)
		if x < 0 {
			t.Fatalf("%q is not on row %d", sub, y)
		}

		_, _, style, _ := screen.GetContent(x, y)
		color, _, _ := style.Decompose()

		return color
	}

	yEnt, yCasa := rows["Escritorio"], rows["Vizinho"]
	if fg(screenLine(screen, yEnt), "Escritorio", yEnt) == fg(screenLine(screen, yCasa), "Vizinho", yCasa) {
		t.Error("the enterprise network is not greyed out")
	}

	// Selectability: the unsupported rows say so and open no prompt.
	p.choose(indexOf(entries, "Escritorio"))

	if got := p.status.GetText(true); !strings.Contains(got, "Escritorio is not supported") || p.step != stepList || p.field.GetLabel() != "" {
		t.Errorf("choosing an enterprise network: status %q, step %d", got, p.step)
	}

	p.choose(indexOf(entries, "Antigo"))

	if got := p.status.GetText(true); !strings.Contains(got, "Antigo is not supported") {
		t.Errorf("choosing a WEP network: status %q", got)
	}

	p.choose(indexOf(entries, "Velho"))

	if p.step != stepPassphrase || !strings.Contains(p.field.GetLabel(), "Velho") {
		t.Errorf("choosing a WPA-Personal network asks for no passphrase: step %d, label %q", p.step, p.field.GetLabel())
	}

	t.Run("unsafe-name", func(t *testing.T) {
		// A tag, a control byte and an escape sequence in a name are shown as text.
		const name = "a[red]b\x07c\x1b[31md"
		const shown = "a[red]b\uFFFDc\uFFFD[31md"

		odd := []wifi.Entry{
			{SSID: name, Security: "owe", Generation: 6, Band: "2.4", Width: 20, Signal: -50, NotRecommended: true},
			{SSID: name + "2", Security: wpa, PSK: true, Generation: 6, Band: "5", Width: 80, Signal: -50, Selectable: true},
		}

		if r := row(odd[0]); !strings.HasPrefix(r, "[gray]") || !strings.HasSuffix(r, "[-][yellow]2.4 GHz – not recommended[-]") {
			t.Errorf("the yellow note is inside the grey span: %q", r)
		}

		p := newPicker(context.Background(), &state.State{}, &inlineOverlay{})
		p.setEntries(odd, nil)

		find := func(screen tcell.SimulationScreen, sub string) string {
			for y := range testHeight {
				for x := range testWidth {
					if r, _, _, _ := screen.GetContent(x, y); r < 0x20 || r == 0x7f {
						t.Fatalf("a control character %#x at %d,%d", r, x, y)
					}
				}

				if line := screenLine(screen, y); strings.Contains(line, sub) {
					return line
				}
			}

			return ""
		}

		screen := drawPicker(t, p)
		if line := find(screen, shown+" "); !strings.Contains(line, "OWE (not supported)") {
			t.Errorf("the row of %q is %q", shown, strings.TrimSpace(line))
		}

		p.choose(0)

		if p.step != stepList {
			t.Errorf("an OWE network is selectable: step %d", p.step)
		}

		if find(drawPicker(t, p), shown+" is not supported") == "" {
			t.Errorf("the status line does not show %q as text", shown)
		}

		p.choose(1)

		if find(drawPicker(t, p), "Passphrase for "+shown+"2:") == "" {
			t.Errorf("the passphrase label does not show %q as text: %q", shown, p.field.GetLabel())
		}
	})
}

// indexOf returns the list index of the network named ssid, or -1.
func indexOf(entries []wifi.Entry, ssid string) int {
	for i, e := range entries {
		if e.SSID == ssid {
			return i
		}
	}

	return -1
}

func TestPickerNeverRendersSecret(t *testing.T) {
	p := newPicker(context.Background(), &state.State{}, &inlineOverlay{})
	p.setEntries([]wifi.Entry{
		{SSID: "Casa", Security: api.SystemNetworkWirelessSecurityWPAPersonal, SAE: true, PSK: true, Generation: 6, Band: "5", Width: 80, Signal: -50, Selectable: true},
	}, nil)
	p.choose(0)

	const secret = "Tr0ub4dor&3-horse"

	for _, r := range secret {
		p.field.InputHandler()(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone), func(tview.Primitive) {})
	}

	if p.field.GetText() != secret {
		t.Fatalf("the field holds %q, not the typed passphrase", p.field.GetText())
	}

	screen := drawPicker(t, p)
	masked := false

	for y := range testHeight {
		line := screenLine(screen, y)

		for _, raw := range []string{secret, hex.EncodeToString([]byte(secret))} {
			if strings.Contains(line, raw) {
				t.Errorf("row %d shows the passphrase or its hex", y)
			}
		}

		if strings.Contains(line, strings.Repeat("*", len(secret))) {
			masked = true
		}
	}

	if !masked {
		t.Error("the passphrase field does not show its mask")
	}
}

func TestPickerOtherNetwork(t *testing.T) {
	savedApply := applyFunc

	t.Cleanup(func() { applyFunc = savedApply })

	applied := make(chan *api.SystemNetworkConfig, 1)
	applyFunc = func(_ context.Context, _ *state.State, cfg *api.SystemNetworkConfig) error {
		applied <- cfg

		return nil
	}

	p := newPicker(context.Background(), &state.State{}, &inlineOverlay{})
	p.setEntries(nil, nil)

	// The only row is "Other network…": a name, then its security, then the passphrase.
	p.choose(0)

	if p.step != stepName || p.field.GetLabel() != "Network name: " {
		t.Fatalf("Other network starts at step %d with label %q", p.step, p.field.GetLabel())
	}

	p.field.SetText("Vizinha")
	p.fieldDone(tcell.KeyEnter)

	if p.step != stepSecurity {
		t.Fatalf("after the name, step %d", p.step)
	}

	p.choose(1)

	if p.step != stepPassphrase {
		t.Fatalf("WPA-Personal asks at step %d, not for a passphrase", p.step)
	}

	p.field.SetText("abcdefgh")
	p.fieldDone(tcell.KeyEnter)

	select {
	case cfg := <-applied:
		if cfg.Wireless == nil || len(cfg.Wireless.Networks) != 1 {
			t.Fatalf("the applied configuration is %+v", cfg)
		}

		n := cfg.Wireless.Networks[0]
		if n.SSID != "Vizinha" || n.Security != api.SystemNetworkWirelessSecurityWPAPersonal || n.Passphrase != "abcdefgh" || !n.Hidden {
			t.Errorf("the other network was saved as %+v", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the other network was not applied")
	}

	if p.step != stepList {
		t.Errorf("after the commit, step %d, not the list", p.step)
	}
}
