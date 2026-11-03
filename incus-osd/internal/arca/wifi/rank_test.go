package wifi

import (
	"context"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fixtureIEs returns the information elements that testdata/ies.txt names.
func fixtureIEs(t *testing.T, name string) []byte {
	t.Helper()

	body, err := os.ReadFile("testdata/ies.txt")
	if err != nil {
		t.Fatal(err)
	}

	for line := range strings.SplitSeq(string(body), "\n") {
		n, h, ok := strings.Cut(line, " ")
		if !ok || n != name {
			continue
		}

		ies, err := hex.DecodeString(h)
		if err != nil {
			t.Fatal(err)
		}

		return ies
	}

	t.Fatalf("no fixture %s", name)

	return nil
}

func TestParseIEs(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile("testdata/ies.txt")
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]Caps{
		"eht320_6ghz": {Generation: 7, Width: 320},
		"he160_6ghz":  {Generation: 6, Width: 160},
		"he80_5ghz":   {Generation: 6, Width: 80},
		"he20_2ghz":   {Generation: 6, Width: 20},
		"vht80":       {Generation: 5, Width: 80},
		"ht40":        {Generation: 4, Width: 40},
		"ht20":        {Generation: 4, Width: 20},
		"legacy":      {Generation: 0, Width: 20},
	}

	read := 0

	for line := range strings.SplitSeq(string(body), "\n") {
		name, h, ok := strings.Cut(line, " ")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}

		ies, err := hex.DecodeString(h)
		if err != nil {
			t.Fatal(err)
		}

		w, ok := want[name]
		if !ok {
			t.Errorf("no expectation for the fixture %s", name)

			continue
		}

		if got := ParseIEs(ies); got != w {
			t.Errorf("%s: ParseIEs = %+v, want %+v", name, got, w)
		}

		// Cut at every length, a fixture parses without a panic; so does each of its
		// elements when its own length and body are cut short, which hands every
		// element reader (heWidth among them) each short body.
		for i := range ies {
			_ = ParseIEs(ies[:i])
		}

		for rest := ies; len(rest) >= 2 && len(rest) >= 2+int(rest[1]); rest = rest[2+int(rest[1]):] {
			for k := range int(rest[1]) {
				_ = ParseIEs(append([]byte{rest[0], byte(k)}, rest[2:2+k]...))
			}
		}

		read++
	}

	if read != len(want) {
		t.Errorf("%d fixtures read, want %d", read, len(want))
	}

	for freq, band := range map[int]string{2411: "", 2412: "2.4", 2484: "2.4", 2485: "", 5160: "5", 5885: "5", 5886: "", 5955: "6", 7115: "6", 7116: ""} {
		if got := Band(freq); got != band {
			t.Errorf("Band(%d) = %q, want %q", freq, got, band)
		}
	}

	// Malformed elements: a length past the end, a lone id, an empty extension and a short HE operation.
	for _, raw := range [][]byte{nil, {255}, {255, 200, 35}, {61, 1}, {255, 0}, {192, 0}, {255, 4, 36, 0xff, 0xff, 0xff}, {255, 1, 106}} {
		_ = ParseIEs(raw)
	}

	// The HE Operation of he160_6ghz, cut short: the width is read only from a whole
	// 6 GHz information field, through ParseIEs and heWidth alike.
	heOp := []byte{0x24, 0x00, 0x00, 0x02, 0x01, 0xff, 0xff, 0x25, 0x03, 0x32, 0x00, 0x0c}
	for k := 1; k <= len(heOp); k++ {
		want := 0
		if k == len(heOp) {
			want = 160
		}

		if got := heWidth(heOp[:k]); got != want {
			t.Errorf("heWidth of %d bytes = %d, want %d", k, got, want)
		}

		if got := ParseIEs(append([]byte{255, byte(k)}, heOp[:k]...)); got.Width != max(want, 20) {
			t.Errorf("ParseIEs of an HE Operation of %d bytes = %+v, want width %d", k, got, max(want, 20))
		}
	}

	if got := ParseIEs(nil); got != (Caps{Generation: 0, Width: 20}) {
		t.Errorf("ParseIEs(nil) = %+v", got)
	}
}

func TestRank(t *testing.T) {
	t.Parallel()

	bss := []BSS{
		{SSID: "Casa-6E", BSSID: "02:00:00:00:00:10", Freq: 6115, Level: -55, Flags: "[WPA2-SAE-CCMP][ESS]", IEs: fixtureIEs(t, "he160_6ghz")},
		{SSID: "Casa", BSSID: "02:00:00:00:00:20", Freq: 5180, Level: -50, Flags: "[WPA2-SAE-CCMP][WPA2-PSK-CCMP][ESS]", IEs: fixtureIEs(t, "he80_5ghz")},
		{SSID: "Casa", BSSID: "02:00:00:00:00:21", Freq: 2412, Level: -20, Flags: "[WPA2-PSK-CCMP][ESS]", IEs: fixtureIEs(t, "ht20")},
		{SSID: "Irmao", BSSID: "02:00:00:00:00:30", Freq: 5180, Level: -50, Flags: "[WPA2-PSK-CCMP][ESS]", IEs: fixtureIEs(t, "he80_5ghz")},
		{SSID: "Escritorio", BSSID: "02:00:00:00:00:40", Freq: 5200, Level: -45, Flags: "[WPA2-EAP-CCMP][ESS]", IEs: fixtureIEs(t, "he80_5ghz")},
		{SSID: "Vizinho", BSSID: "02:00:00:00:00:50", Freq: 5240, Level: -40, Flags: "[WPA2-PSK-CCMP][ESS]", IEs: fixtureIEs(t, "vht80")},
		{SSID: "", BSSID: "02:00:00:00:00:60", Freq: 5180, Level: -30, Flags: "[WPA2-PSK-CCMP][ESS]", IEs: fixtureIEs(t, "he80_5ghz")},
		{SSID: "Velho", BSSID: "02:00:00:00:00:70", Freq: 2437, Level: -40, Flags: "[WPA2-PSK-CCMP][ESS]", IEs: fixtureIEs(t, "ht40")},
		{SSID: "Grande", BSSID: "02:00:00:00:00:80", Freq: 5180, Level: -40, Flags: "[ESS]", NoIEs: true},
		{SSID: "Antigo", BSSID: "02:00:00:00:00:90", Freq: 2462, Level: -30, Flags: "[WEP][ESS]", IEs: fixtureIEs(t, "legacy")},
	}

	got := Rank(bss)

	order := make([]string, 0, len(got))
	for _, e := range got {
		order = append(order, e.SSID)
	}

	want := []string{"Casa-6E", "Escritorio", "Casa", "Irmao", "Vizinho", "Velho", "Grande", "Antigo"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("Rank order = %v, want %v", order, want)
	}

	for _, e := range got {
		switch e.SSID {
		case "Casa-6E":
			if e.Band != "6" || e.Width != 160 || e.Generation != 6 || e.NotRecommended || !e.Selectable || !e.SAE {
				t.Errorf("Casa-6E = %+v", e)
			}
		case "Casa":
			if e.Band != "5" || e.Signal != -50 || !e.SAE || !e.PSK {
				t.Errorf("Casa is represented by its 2.4 GHz BSS, not its best: %+v", e)
			}
		case "Velho":
			if !e.NotRecommended || !e.Selectable || e.Band != "2.4" || e.Width != 40 {
				t.Errorf("Velho = %+v", e)
			}
		case "Escritorio", "Antigo":
			if e.Selectable {
				t.Errorf("%s is selectable: %+v", e.SSID, e)
			}
		case "Grande":
			if e.Generation != 0 || e.Width != 20 || e.Band != "5" || !e.Selectable {
				t.Errorf("a network with no information elements = %+v", e)
			}
		case "Irmao", "Vizinho":
			if e.Band != "5" || !e.PSK || !e.Selectable {
				t.Errorf("%s = %+v", e.SSID, e)
			}
		default:
			t.Errorf("unexpected network %q", e.SSID)
		}
	}

	if len(Rank(nil)) != 0 {
		t.Error("Rank of no BSS is not empty")
	}

	// Within one SSID the strongest security class wins before speed: an open BSS
	// never stands for a network that has a secured one, nearer and faster though it is.
	t.Run("security-first", func(t *testing.T) {
		t.Parallel()

		he80, ht20 := fixtureIEs(t, "he80_5ghz"), fixtureIEs(t, "ht20")
		cases := []struct {
			name  string
			bss   []BSS
			sec   string
			sae   bool
			bssid string
		}{
			{"open twin of SAE", []BSS{
				{SSID: "Casa", BSSID: "02:00:00:00:01:01", Freq: 5180, Level: -30, Flags: "[ESS]", IEs: he80},
				{SSID: "Casa", BSSID: "02:00:00:00:01:02", Freq: 2412, Level: -70, Flags: "[WPA2-SAE-CCMP][ESS]", IEs: ht20},
			}, "wpa-personal", true, "02:00:00:00:01:02"},
			{"SAE before WPA2", []BSS{
				{SSID: "Casa", BSSID: "02:00:00:00:02:01", Freq: 5180, Level: -30, Flags: "[WPA2-PSK-CCMP][ESS]", IEs: he80},
				{SSID: "Casa", BSSID: "02:00:00:00:02:02", Freq: 2412, Level: -70, Flags: "[WPA2-SAE-CCMP][ESS]", IEs: ht20},
			}, "wpa-personal", true, "02:00:00:00:02:02"},
			{"WPA2 before WPA", []BSS{
				{SSID: "Casa", BSSID: "02:00:00:00:03:01", Freq: 5180, Level: -30, Flags: "[WPA-PSK-TKIP][ESS]", IEs: he80},
				{SSID: "Casa", BSSID: "02:00:00:00:03:02", Freq: 2412, Level: -70, Flags: "[WPA-PSK-CCMP][WPA2-PSK-CCMP][ESS]", IEs: ht20},
			}, "wpa-personal", false, "02:00:00:00:03:02"},
			{"WPA before open", []BSS{
				{SSID: "Casa", BSSID: "02:00:00:00:04:01", Freq: 5180, Level: -30, Flags: "[ESS]", IEs: he80},
				{SSID: "Casa", BSSID: "02:00:00:00:04:02", Freq: 2412, Level: -70, Flags: "[WPA-PSK-TKIP][ESS]", IEs: ht20},
			}, "wpa-personal", false, "02:00:00:00:04:02"},
			{"enterprise before open", []BSS{
				{SSID: "Casa", BSSID: "02:00:00:00:05:01", Freq: 5180, Level: -30, Flags: "[ESS]", IEs: he80},
				{SSID: "Casa", BSSID: "02:00:00:00:05:02", Freq: 2412, Level: -70, Flags: "[WPA2-EAP-CCMP][ESS]", IEs: ht20},
			}, "enterprise", false, "02:00:00:00:05:02"},
			{"OWE before open", []BSS{
				{SSID: "Casa", BSSID: "02:00:00:00:06:01", Freq: 5180, Level: -30, Flags: "[ESS]", IEs: he80},
				{SSID: "Casa", BSSID: "02:00:00:00:06:02", Freq: 2412, Level: -70, Flags: "[WPA2-OWE-CCMP][ESS]", IEs: ht20},
			}, "owe", false, "02:00:00:00:06:02"},
			{"speed within a class", []BSS{
				{SSID: "Casa", BSSID: "02:00:00:00:07:01", Freq: 2412, Level: -30, Flags: "[WPA2-SAE-CCMP][ESS]", IEs: ht20},
				{SSID: "Casa", BSSID: "02:00:00:00:07:02", Freq: 5180, Level: -70, Flags: "[WPA2-PSK+SAE-CCMP][SAE-H2E][ESS]", IEs: he80},
			}, "wpa-personal", true, "02:00:00:00:07:02"},
		}

		for _, c := range cases {
			for _, order := range [][]BSS{c.bss, {c.bss[1], c.bss[0]}} {
				got := Rank(order)
				if len(got) != 1 || got[0].Security != c.sec || got[0].SAE != c.sae || got[0].BSSID != c.bssid {
					t.Errorf("%s: Rank = %+v, want %s (SAE %v) from %s", c.name, got, c.sec, c.sae, c.bssid)
				}
			}
		}
	})

	// A key management that the supplicant configuration cannot express is listed,
	// marked, and not selectable; it is never taken for an open network.
	t.Run("key-management", func(t *testing.T) {
		t.Parallel()

		for flags, want := range map[string]struct {
			sec      string
			sae, psk bool
		}{
			"[WPA2-OWE-CCMP][ESS]":             {"owe", false, false},
			"[WPA2-DPP-CCMP][ESS]":             {"unsupported", false, false},
			"[WPA2-FT/PSK-CCMP][ESS]":          {"unsupported", false, false},
			"[WPA2-SAE-EXT-KEY-GCMP-256][ESS]": {"unsupported", false, false},
			"[WPA2-FILS-SHA256-CCMP][ESS]":     {"unsupported", false, false},
			"[WPA2-?-CCMP][ESS]":               {"unsupported", false, false},
			"[WPA2-FT/EAP+EAP-SHA256-CCMP]":    {"enterprise", false, false},
			"[WPA2-PSK-SHA256-CCMP][ESS]":      {"wpa-personal", false, true},
			"[WPA2-PSK+FT/PSK+SAE-CCMP][ESS]":  {"wpa-personal", true, true},
			"[WPA2-FT/SAE+SAE-CCMP-preauth]":   {"wpa-personal", true, false},
			"[ESS][OWE-TRANS]":                 {"open", false, false},
			"[WPA2-PSK-CCMP][SAE-H2E][SAE-PK]": {"wpa-personal", false, true},
			"[WEP][ESS]":                       {"wep", false, false},
		} {
			got := Rank([]BSS{{SSID: "Rede", BSSID: "02:00:00:00:08:01", Freq: 5180, Level: -50, Flags: flags}})
			if len(got) != 1 || got[0].Security != want.sec || got[0].SAE != want.sae || got[0].PSK != want.psk {
				t.Errorf("%s: Rank = %+v, want %s (SAE %v, PSK %v)", flags, got, want.sec, want.sae, want.psk)

				continue
			}

			if joinable := want.sec == "wpa-personal" || want.sec == "open"; got[0].Selectable != joinable {
				t.Errorf("%s: Selectable = %v, want %v", flags, got[0].Selectable, joinable)
			}
		}
	})
}

// fakeWalk answers each request from replies, a SCAN with OK and a results event,
// and returns the requests it received, in order, once the walk is done.
func fakeWalk(t *testing.T, dir string, replies map[string]string) chan string {
	t.Helper()

	server, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: filepath.Join(dir, "wlan0"), Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = server.Close() })

	requests := make(chan string, 64)

	go func() {
		buf := make([]byte, 4096)

		for {
			n, from, err := server.ReadFromUnix(buf)
			if err != nil {
				return
			}

			req := string(buf[:n])
			requests <- req

			switch req {
			case "ATTACH", "DETACH":
				_, _ = server.WriteToUnix([]byte("OK\n"), from)
			case "SCAN":
				_, _ = server.WriteToUnix([]byte("OK\n"), from)
				_, _ = server.WriteToUnix([]byte("<3>CTRL-EVENT-SCAN-RESULTS "), from)
			default:
				_, _ = server.WriteToUnix([]byte(replies[req]), from)
			}
		}
	}()

	return requests
}

func drain(requests chan string) []string {
	var got []string

	for len(requests) > 0 {
		got = append(got, <-requests)
	}

	return got
}

func TestScanWalkOverflow(t *testing.T) {
	t.Parallel()

	const (
		small = "id=0\nbssid=02:00:00:00:00:01\nfreq=2437\nlevel=-40\nie=00\nflags=[WPA2-PSK-CCMP][ESS]\nssid=Velho\n"
		// The IEs of this entry do not fit one 4096-byte reply, so the supplicant sends nothing for it.
		withoutIEs = "id=1\nbssid=02:00:00:00:00:02\nfreq=5180\nlevel=-60\nflags=[WPA2-PSK-CCMP][ESS]\nssid=Grande\n"
		last       = "id=2\nbssid=02:00:00:00:00:03\nfreq=5180\nlevel=-50\nie=00\nflags=[WPA2-PSK-CCMP][ESS]\nssid=Casa\n"
	)

	t.Run("middle", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		requests := fakeWalk(t, dir, map[string]string{
			"BSS FIRST MASK=0x1c87":  small,
			"BSS NEXT-0 MASK=0x1c87": "",
			"BSS LAST MASK=0x1":      "id=2\n",
			"BSS NEXT-0 MASK=0x1887": withoutIEs,
			"BSS NEXT-1 MASK=0x1c87": last,
			"BSS NEXT-2 MASK=0x1c87": "",
		})

		conn, err := Dial(dir, "wlan0")
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		got, err := conn.Scan(ctx)
		if err != nil {
			t.Fatal(err)
		}

		_ = conn.Close()

		if len(got) != 3 || got[1].SSID != "Grande" || !got[1].NoIEs || got[1].Level != -60 || got[1].IEs != nil {
			t.Fatalf("Scan read %+v", got)
		}

		if got[0].NoIEs || got[2].NoIEs || got[2].SSID != "Casa" {
			t.Errorf("only the entry that did not fit is read without its IEs: %+v", got)
		}

		var walk []string

		for _, req := range drain(requests) {
			if strings.HasPrefix(req, "BSS") {
				walk = append(walk, req)
			}
		}

		want := []string{"BSS FIRST MASK=0x1c87", "BSS NEXT-0 MASK=0x1c87", "BSS LAST MASK=0x1", "BSS NEXT-0 MASK=0x1887", "BSS NEXT-1 MASK=0x1c87", "BSS NEXT-2 MASK=0x1c87", "BSS LAST MASK=0x1"}
		if !reflect.DeepEqual(walk, want) {
			t.Errorf("the walk sent %q, want %q", walk, want)
		}
	})

	t.Run("first", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		fakeWalk(t, dir, map[string]string{
			"BSS FIRST MASK=0x1c87":  "",
			"BSS LAST MASK=0x1":      "id=0\n",
			"BSS FIRST MASK=0x1887":  small,
			"BSS NEXT-0 MASK=0x1c87": "",
		})

		conn, err := Dial(dir, "wlan0")
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		got, err := conn.Scan(ctx)
		if err != nil {
			t.Fatal(err)
		}

		_ = conn.Close()

		if len(got) != 1 || !got[0].NoIEs || got[0].SSID != "Velho" {
			t.Fatalf("Scan read %+v when the first entry did not fit", got)
		}
	})
}
