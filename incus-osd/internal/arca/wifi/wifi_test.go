package wifi

import (
	"context"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus-os/incus-osd/api"
)

func wpa(ssid string, passphrase string) *api.SystemNetworkWireless {
	return &api.SystemNetworkWireless{
		Addresses: []string{"dhcp4", "slaac"},
		Networks:  []api.SystemNetworkWirelessNetwork{{SSID: ssid, Security: api.SystemNetworkWirelessSecurityWPAPersonal, Passphrase: passphrase}},
	}
}

func TestRenderSupplicant(t *testing.T) {
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
			}

			_, _ = server.WriteToUnix([]byte(reply), from)
		}
	}()
}

func TestScanWalk(t *testing.T) {
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
		if strings.HasPrefix(req, "BSS") {
			last = req
		}
	}

	if last != "BSS NEXT-2 MASK=0x1c87" {
		t.Errorf("the walk ended after %q, want the empty reply to BSS NEXT-2", last)
	}

	entries := Rank(got)
	if len(entries) != 3 || entries[0].SSID != "Velho" || !entries[0].NotRecommended || !entries[1].SAE || entries[2].Security != api.SystemNetworkWirelessSecurityOpen {
		t.Errorf("Rank = %+v", entries)
	}
}
