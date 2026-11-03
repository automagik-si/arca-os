package wifi

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus-os/incus-osd/api"
)

// UseSysRoot points the validation that looks at this machine's hardware at a
// fake sysfs for the rest of the test. The external tests use it.
func UseSysRoot(t *testing.T, root string) {
	t.Helper()

	saved := machineSysRoot
	machineSysRoot = root

	t.Cleanup(func() { machineSysRoot = saved })
}

func wpa(ssid string, passphrase string) *api.SystemNetworkWireless {
	return &api.SystemNetworkWireless{
		Addresses: []string{"dhcp4", "slaac"},
		Networks:  []api.SystemNetworkWirelessNetwork{{SSID: ssid, Security: api.SystemNetworkWirelessSecurityWPAPersonal, Passphrase: passphrase}},
	}
}

func TestRenderSupplicant(t *testing.T) {
	t.Parallel()

	// IEEE 802.11i-2004 H.4.1: passphrase "password", SSID "IEEE".
	got := RenderSupplicant(wpa("IEEE", "password"))

	for _, want := range []string{
		"ctrl_interface=DIR=/run/incus-os/wifi/ctrl\n",
		"\tssid=" + hex.EncodeToString([]byte("IEEE")) + "\n",
		"\tkey_mgmt=SAE WPA-PSK WPA-PSK-SHA256\n",
		"\tieee80211w=1\n",
		"\tpsk=f42c6fc52df0ebef9ebb4b90b38a5f902e83fe1b135a70e23aed762e9710a12e\n",
		"\tsae_password=" + hex.EncodeToString([]byte("password")) + "\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the supplicant config lacks %q", want)
		}
	}

	if strings.Contains(got, "password\n") || strings.Contains(got, "\"") {
		t.Error("the supplicant config holds the passphrase in clear or a quoted string")
	}
}

func TestPassphraseBound(t *testing.T) {
	t.Parallel()

	hex64 := strings.Repeat("0123456789abcdef", 4)
	rows := []struct {
		name       string
		passphrase string
		valid      bool
		psk        bool
	}{
		{"8 ASCII", "abcdefgh", true, true},
		{"63 ASCII", strings.Repeat("a", 63), true, true},
		{"64 hex", hex64, true, false},
		{"64 non-hex", strings.Repeat("z", 64), true, false},
		{"7 ASCII", "abcdefg", true, false},
		{"128 bytes", strings.Repeat("é", 64), true, false},
		{"empty", "", false, false},
		{"129 bytes", strings.Repeat("a", 129), false, false},
		{"control character", "abcd\tefgh", false, false},
		{"NUL", "abcd\x00efgh", false, false},
		{"DEL", "abcd\x7fefgh", false, false},
		{"invalid UTF-8", "abcdefgh\xff", false, false},
	}

	for _, row := range rows {
		w := wpa("Velho", row.passphrase)

		err := Validate(&api.SystemNetworkConfig{Wireless: w}, false, "", nil)
		if (err == nil) != row.valid {
			t.Errorf("%s: Validate returned %v, want valid=%v", row.name, err, row.valid)
		}

		if err != nil {
			if row.passphrase != "" && (strings.Contains(err.Error(), row.passphrase) || strings.Contains(err.Error(), hex.EncodeToString([]byte(row.passphrase)))) {
				t.Errorf("%s: the error carries the passphrase", row.name)
			}

			continue
		}

		got := RenderSupplicant(w)
		if row.psk != strings.Contains(got, "\tpsk=") || row.psk != strings.Contains(got, "WPA-PSK WPA-PSK-SHA256") {
			t.Errorf("%s: WPA-PSK half rendered=%v, want %v", row.name, !row.psk, row.psk)
		}

		if !row.psk && !strings.Contains(got, "\tkey_mgmt=SAE\n") {
			t.Errorf("%s: not rendered as SAE alone", row.name)
		}
	}
}

func TestRenderNetwork(t *testing.T) {
	t.Parallel()

	got := RenderNetwork(&api.SystemNetworkConfig{Wireless: Defaults()})

	for _, want := range []string{"[Match]\nType=wlan\nWLANInterfaceType=station\n", "RequiredForOnline=no\n", "DHCP=ipv4\n", "IPv6AcceptRA=true\n", "[DHCPv4]\n", "[IPv6AcceptRA]\nRouteMetric=600\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("the .network lacks %q", want)
		}
	}

	if strings.Count(got, "RouteMetric=600\n") != 2 {
		t.Error("the .network does not set metric 600 for both families")
	}
}

func TestWriteFilesModes(t *testing.T) {
	t.Parallel()

	for _, w := range []*api.SystemNetworkWireless{wpa("Velho", "abcdefgh"), nil} {
		root := t.TempDir()

		err := WriteFiles(root, &api.SystemNetworkConfig{Wireless: w})
		if err != nil {
			t.Fatal(err)
		}

		for path, mode := range map[string]os.FileMode{RunDir: os.ModeDir | 0o700, SupplicantConf: 0o600, NetworkFile: 0o644} {
			fi, err := os.Stat(filepath.Join(root, path))
			if err != nil {
				t.Fatal(err)
			}

			if fi.Mode() != mode {
				t.Errorf("%s (wireless set: %v) has mode %v, want %v", path, w != nil, fi.Mode(), mode)
			}
		}
	}
}

// supplicantLine is one well-formed line of the supplicant configuration: a
// global setting, the start or end of a network block, or one of its settings,
// whose value holds no quote, brace, equals sign or line break.
var supplicantLine = regexp.MustCompile(`^(ctrl_interface=DIR=/[a-z/-]+|[a-z_0-9]+=[A-Za-z0-9:,._ -]*|network=\{|\t[a-z_0-9]+=[A-Za-z0-9:._ -]*|\}|)$`)

func TestWriteFilesHostileInput(t *testing.T) {
	t.Parallel()

	const passphrase = "abcdefgh"

	for _, bad := range []string{"\n", "\"", "}", "="} {
		for _, field := range []string{"ssid", "passphrase", "bssid", "country"} {
			n := api.SystemNetworkWirelessNetwork{SSID: "Velho", Security: api.SystemNetworkWirelessSecurityWPAPersonal, Passphrase: passphrase, BSSID: "02:00:00:00:00:01"}
			w := &api.SystemNetworkWireless{Addresses: []string{"dhcp4"}, Country: "US"}

			switch field {
			case "ssid":
				n.SSID = "Vel" + bad + "ho"
			case "passphrase":
				n.Passphrase = "abcd" + bad + "efgh"
			case "bssid":
				n.BSSID += bad
			case "country":
				w.Country = "U" + bad
			default:
				t.Fatalf("unknown field %q", field)
			}

			w.Networks = []api.SystemNetworkWirelessNetwork{n}
			name := fmt.Sprintf("%q in the %s", bad, field)
			root := t.TempDir()

			err := WriteFiles(root, &api.SystemNetworkConfig{Wireless: w})
			if err != nil {
				if strings.Contains(err.Error(), n.Passphrase) || strings.Contains(err.Error(), hex.EncodeToString([]byte(n.Passphrase))) {
					t.Errorf("%s: the error carries the passphrase", name)
				}

				_, statErr := os.Stat(filepath.Join(root, SupplicantConf))
				if !os.IsNotExist(statErr) {
					t.Errorf("%s: refused, but a supplicant config was written", name)
				}

				continue
			}

			// Only an SSID or a passphrase may hold these; both are rendered in hex.
			if field != "ssid" && (field != "passphrase" || bad == "\n") {
				t.Errorf("%s: accepted", name)
			}

			body, err := os.ReadFile(filepath.Join(root, SupplicantConf))
			if err != nil {
				t.Fatal(err)
			}

			conf := string(body)
			for line := range strings.SplitSeq(strings.TrimSuffix(conf, "\n"), "\n") {
				if !supplicantLine.MatchString(line) {
					t.Errorf("%s: the supplicant config has the line %q", name, line)
				}
			}

			if strings.Count(conf, "network={\n") != 1 || strings.Count(conf, "\n}\n") != 1 {
				t.Errorf("%s: the supplicant config does not hold exactly one network block", name)
			}

			if !strings.Contains(conf, "\tssid="+hex.EncodeToString([]byte(n.SSID))+"\n") || strings.Contains(conf, n.Passphrase) {
				t.Errorf("%s: the SSID is not in hex or the passphrase is in clear", name)
			}
		}
	}
}

func fakeSys(t *testing.T, devices map[string]bool) string {
	t.Helper()

	root := t.TempDir()

	for name, isWifi := range devices {
		dir := filepath.Join(root, "class", "net", name)
		if isWifi {
			dir = filepath.Join(dir, "phy80211")
		}

		err := os.MkdirAll(dir, 0o755)
		if err != nil {
			t.Fatal(err)
		}
	}

	return root
}

func TestDevices(t *testing.T) {
	t.Parallel()

	root := fakeSys(t, map[string]bool{"wlp2s0": true, "enp1s0": false, "wlan0": true})

	got, err := Devices(root)
	if err != nil || !reflect.DeepEqual(got, []string{"wlan0", "wlp2s0"}) {
		t.Errorf("Devices = %v, %v", got, err)
	}

	got, _ = Devices("")
	if len(got) != 0 || IsDevice("", "wlan0") || IsDevice(root, "enp1s0") || !IsDevice(root, "wlp2s0") {
		t.Error("an empty sysRoot or a wired device counted as Wi-Fi")
	}
}

func TestCountAddresses(t *testing.T) {
	t.Parallel()

	wired := []api.SystemNetworkInterface{{Name: "enp1s0", Hwaddr: "00:16:3e:00:00:01"}}
	noWifi := fakeSys(t, map[string]bool{"enp1s0": false})
	withWifi := fakeSys(t, map[string]bool{"enp1s0": false, "wlan0": true})

	rows := []struct {
		name    string
		cfg     *api.SystemNetworkConfig
		sysRoot string
		want    int
	}{
		{"wired only", &api.SystemNetworkConfig{Interfaces: wired, Wireless: Defaults()}, noWifi, 0},
		{"no wired device", &api.SystemNetworkConfig{Wireless: Defaults()}, noWifi, 2},
		{"a saved network", &api.SystemNetworkConfig{Interfaces: wired, Wireless: wpa("Velho", "abcdefgh")}, noWifi, 2},
		{"a Wi-Fi device", &api.SystemNetworkConfig{Interfaces: wired, Wireless: Defaults()}, withWifi, 2},
		{"devices unknown", &api.SystemNetworkConfig{Interfaces: wired, Wireless: Defaults()}, "", 2},
		{"no wireless object", &api.SystemNetworkConfig{}, withWifi, 0},
	}

	for _, row := range rows {
		got := CountAddresses(row.cfg, row.sysRoot)
		if got != row.want {
			t.Errorf("%s: CountAddresses = %d, want %d", row.name, got, row.want)
		}
	}
}

func TestResetStateKeepsInProcess(t *testing.T) {
	t.Parallel()

	for _, inProcess := range []bool{true, false} {
		prev := api.SystemNetworkState{
			Interfaces:             map[string]api.SystemNetworkInterfaceState{"enp1s0": {State: "routable"}},
			ConfigurationInProcess: inProcess,
			Connectivity:           &api.SystemNetworkConnectivity{Online: true},
		}

		got := ResetState(prev)
		want := api.SystemNetworkState{Interfaces: map[string]api.SystemNetworkInterfaceState{}, ConfigurationInProcess: inProcess}

		if got.Interfaces == nil || !reflect.DeepEqual(got, want) {
			t.Errorf("ResetState(in process %v) = %+v, want %+v", inProcess, got, want)
		}
	}
}

func TestSeedMergeWirelessDefaults(t *testing.T) {
	t.Parallel()

	defaults := &api.SystemNetworkConfig{Wireless: Defaults()}

	cfg := &api.SystemNetworkConfig{}
	MergeSeedDefaults(cfg, defaults)

	if !reflect.DeepEqual(cfg.Wireless, Defaults()) {
		t.Errorf("a seed without wireless got %+v", cfg.Wireless)
	}

	own := &api.SystemNetworkConfig{Wireless: wpa("Velho", "abcdefgh")}
	MergeSeedDefaults(own, defaults)

	if !reflect.DeepEqual(own.Wireless, wpa("Velho", "abcdefgh")) {
		t.Errorf("a seed's own wireless changed to %+v", own.Wireless)
	}
}

// fakeSupplicant answers one control socket with the given BSS replies and
// records every request it gets.
func fakeSupplicant(t *testing.T, dir string, bss []string, requests chan<- string) {
	t.Helper()

	server, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: filepath.Join(dir, "wlan0"), Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = server.Close() })

	go func() {
		buf := make([]byte, 4096)

		for {
			n, from, err := server.ReadFromUnix(buf)
			if err != nil {
				return
			}

			req := string(buf[:n])
			requests <- req

			reply := ""

			switch {
			case req == "ATTACH" || req == "DETACH":
				reply = "OK\n"
			case req == "SCAN":
				_, _ = server.WriteToUnix([]byte("OK\n"), from)
				reply = "<3>CTRL-EVENT-SCAN-RESULTS "
			case req == "BSS FIRST MASK=0x1c87":
				reply = bss[0]
			case strings.HasPrefix(req, "BSS NEXT-"):
				id, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(req, "BSS NEXT-"), " MASK=0x1c87"))
				if id+1 < len(bss) {
					reply = bss[id+1]
				}
			default:
				// An unknown request; answer nothing.
			}

			_, _ = server.WriteToUnix([]byte(reply), from)
		}
	}()
}

func TestScanWalk(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bss := []string{
		"id=0\nbssid=02:00:00:00:00:01\nfreq=2412\nlevel=-40\nie=000556656c686f\nflags=[WPA2-PSK-CCMP][ESS]\nssid=Velho\n",
		"id=1\nbssid=02:00:00:00:00:02\nfreq=5180\nlevel=-50\nie=00\nflags=[WPA2-SAE-CCMP][ESS]\nssid=Casa\n",
		"id=2\nbssid=02:00:00:00:00:03\nfreq=2437\nlevel=-60\nie=\nflags=[ESS]\nssid=Caf\\xc3\\xa9 \\\"1\\\"\n",
	}
	requests := make(chan string, 32)
	fakeSupplicant(t, dir, bss, requests)

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

	if len(got) != 3 || got[0].SSID != "Velho" || got[1].Freq != 5180 || got[2].SSID != "Café \"1\"" || got[0].Level != -40 || len(got[0].IEs) != 7 {
		t.Fatalf("Scan read %+v", got)
	}

	_ = conn.Close()

	_, err = os.Stat(conn.local)
	if !os.IsNotExist(err) {
		t.Error("Close left the client socket behind")
	}

	last := ""

	for len(requests) > 0 {
		req := <-requests
		if strings.HasPrefix(req, "BSS") && !strings.HasPrefix(req, "BSS LAST") {
			last = req
		}
	}

	if last != "BSS NEXT-2 MASK=0x1c87" {
		t.Errorf("the walk ended after %q, want the empty reply to BSS NEXT-2", last)
	}

	entries := Rank(got)
	if len(entries) != 3 || entries[0].SSID != "Casa" || !entries[0].SAE || !entries[1].NotRecommended || entries[2].Security != api.SystemNetworkWirelessSecurityOpen {
		t.Errorf("Rank = %+v", entries)
	}
}

func TestUnescapeSSIDMalformed(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		`Velho`:       "Velho",
		`Caf\xc3\xa9`: "Café",
		`a\"b\\c`:     `a"b\c`,
		`tab\tnl\n`:   "tab\tnl\n",
		`trailing\`:   `trailing\`,
		`short\x4`:    `short\x4`,
		`end\x`:       `end\x`,
		`nothex\xzz`:  `nothex\xzz`,
		`unknown\q`:   "unknownq",
		`nul\x00byte`: "nul\x00byte",
		`\x`:          `\x`,
		`\`:           `\`,
		``:            "",
	} {
		got := unescapeSSID(in)
		if got != want {
			t.Errorf("unescapeSSID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseBSSMalformed(t *testing.T) {
	t.Parallel()

	for _, reply := range []string{"", "FAIL\n", "UNKNOWN COMMAND\n", "bssid=02:00:00:00:00:01\nssid=Velho\n", "id=x\nssid=Velho\n", "id=\n", "garbage"} {
		_, err := parseBSS(reply)
		if err == nil {
			t.Errorf("parseBSS(%q) gave no error", reply)
		}
	}

	bss, err := parseBSS("id=4\nfreq=abc\nlevel=\nie=00zz\nno line\n=empty key\nflags=[ESS]\nssid=Velho\nssid2=x\n")
	if err != nil || bss.ID != 4 || bss.Freq != 0 || bss.Level != 0 || bss.IEs != nil || bss.Flags != "[ESS]" || bss.SSID != "Velho" {
		t.Errorf("parseBSS of a malformed reply = %+v, %v", bss, err)
	}
}

func TestScanMalformedReply(t *testing.T) {
	t.Parallel()

	for _, bss := range [][]string{
		{"FAIL\n"},
		{"id=0\nbssid=02:00:00:00:00:01\nfreq=2412\nlevel=-40\nflags=[ESS]\nssid=Velho\n", "no id here\n"},
	} {
		dir := t.TempDir()
		fakeSupplicant(t, dir, bss, make(chan string, 32))

		conn, err := Dial(dir, "wlan0")
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

		got, err := conn.Scan(ctx)
		if err == nil {
			t.Errorf("Scan of %q read %+v and no error", bss, got)
		}

		cancel()

		_ = conn.Close()
	}
}
