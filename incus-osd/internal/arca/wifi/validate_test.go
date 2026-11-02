package wifi

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lxc/incus-os/incus-osd/api"
)

// savedNetworks returns a wireless object with the given networks and the default addresses.
func savedNetworks(ns ...api.SystemNetworkWirelessNetwork) *api.SystemNetworkWireless {
	return &api.SystemNetworkWireless{Addresses: []string{"dhcp4", "slaac"}, Networks: ns}
}

// pskOf returns the PSK that the supplicant configuration of one WPA-Personal network holds.
func pskOf(t *testing.T, ssid string, passphrase string) string {
	t.Helper()

	for _, line := range strings.Split(RenderSupplicant(wpa(ssid, passphrase)), "\n") {
		if psk, ok := strings.CutPrefix(line, "\tpsk="); ok {
			return psk
		}
	}

	t.Fatal("the supplicant configuration holds no psk")

	return ""
}

func TestValidateWireless(t *testing.T) {
	velho := func(passphrase string) api.SystemNetworkWirelessNetwork {
		return api.SystemNetworkWirelessNetwork{SSID: "Velho", Security: api.SystemNetworkWirelessSecurityWPAPersonal, Passphrase: passphrase}
	}

	bssid := func(b string) *api.SystemNetworkWireless {
		n := velho("password")
		n.BSSID = b

		return savedNetworks(n)
	}

	ssid := func(s string) *api.SystemNetworkWireless {
		n := velho("password")
		n.SSID = s

		return savedNetworks(n)
	}

	// The firewall validator refuses every rule, so a rule shows the error is passed through.
	refuseRules := func(rules []api.SystemNetworkFirewallRule) error {
		if len(rules) > 0 {
			return errors.New(`unsupported action "skip"`)
		}

		return nil
	}

	firewalled := savedNetworks(velho("password"))
	firewalled.FirewallRules = []api.SystemNetworkFirewallRule{{Action: "skip"}}

	rows := []struct {
		name string
		w    *api.SystemNetworkWireless
		want string // "" when the configuration is valid, else a part of the refusal
	}{
		{name: "defaults", w: Defaults()},
		{name: "wpa-personal", w: savedNetworks(velho("password"))},
		{name: "open without passphrase", w: savedNetworks(api.SystemNetworkWirelessNetwork{SSID: "Cafe", Security: api.SystemNetworkWirelessSecurityOpen})},
		{name: "same SSID with another security", w: savedNetworks(velho("password"), api.SystemNetworkWirelessNetwork{SSID: "Velho", Security: api.SystemNetworkWirelessSecurityOpen})},
		{name: "SSID of 32 bytes", w: ssid(strings.Repeat("a", 32))},
		{name: "SSID of 33 bytes", w: ssid(strings.Repeat("a", 33)), want: "the SSID must be 1-32 bytes"},
		{name: "empty SSID", w: ssid(""), want: "the SSID must be 1-32 bytes"},
		{name: "SSID that is not UTF-8", w: ssid("\xff"), want: "the SSID must be 1-32 bytes"},
		{name: "same SSID and security twice", w: savedNetworks(velho("password"), velho("another-one")), want: "same SSID and security"},
		{name: "enterprise", w: savedNetworks(api.SystemNetworkWirelessNetwork{SSID: "Office", Security: "enterprise"}), want: "enterprise (802.1X) networks are not supported"},
		{name: "EAP", w: savedNetworks(api.SystemNetworkWirelessNetwork{SSID: "Office", Security: "wpa-eap"}), want: "enterprise (802.1X) networks are not supported"},
		{name: "WEP", w: savedNetworks(api.SystemNetworkWirelessNetwork{SSID: "Old", Security: "wep"}), want: "the security is not wpa-personal or open"},
		{name: "wpa-personal without passphrase", w: savedNetworks(api.SystemNetworkWirelessNetwork{SSID: "Nova", Security: api.SystemNetworkWirelessSecurityWPAPersonal}), want: "the passphrase must be 1-128 bytes"},
		{name: "wpa-personal with a control character", w: savedNetworks(velho("pass\tword")), want: "the passphrase must be 1-128 bytes"},
		{name: "wpa-personal over 128 bytes", w: savedNetworks(velho(strings.Repeat("a", 129))), want: "the passphrase must be 1-128 bytes"},
		{name: "open with a passphrase", w: savedNetworks(api.SystemNetworkWirelessNetwork{SSID: "Cafe", Security: api.SystemNetworkWirelessSecurityOpen, Passphrase: "password"}), want: "takes no passphrase"},
		{name: "BSSID", w: bssid("00:11:22:33:44:55")},
		{name: "BSSID with five pairs", w: bssid("00:11:22:33:44"), want: "unicast MAC address"},
		{name: "BSSID in upper case", w: bssid("00:11:22:33:44:AA"), want: "unicast MAC address"},
		{name: "BSSID that is multicast", w: bssid("01:11:22:33:44:55"), want: "unicast MAC address"},
		{name: "BSSID with dashes", w: bssid("00-11-22-33-44-55"), want: "unicast MAC address"},
		{name: "BSSID in dotted form", w: bssid("0011.2233.4455"), want: "unicast MAC address"},
		{name: "country", w: &api.SystemNetworkWireless{Country: "US", Addresses: []string{"dhcp4"}}},
		{name: "country in lower case", w: &api.SystemNetworkWireless{Country: "us"}, want: "ISO 3166 alpha-2"},
		{name: "country of three letters", w: &api.SystemNetworkWireless{Country: "USA"}, want: "ISO 3166 alpha-2"},
		{name: "country with a digit", w: &api.SystemNetworkWireless{Country: "U1"}, want: "ISO 3166 alpha-2"},
		{name: "DHCPv6 and SLAAC", w: &api.SystemNetworkWireless{Addresses: []string{"dhcp6", "slaac"}}},
		{name: "address listed twice", w: &api.SystemNetworkWireless{Addresses: []string{"dhcp4", "dhcp4"}}, want: "listed more than once"},
		{name: "static address", w: &api.SystemNetworkWireless{Addresses: []string{"10.42.1.5/24"}}, want: "not dhcp4, dhcp6 or slaac"},
		{name: "firewall rules through the validator", w: firewalled, want: `wireless unsupported action "skip"`},
	}

	for _, row := range rows {
		err := Validate(&api.SystemNetworkConfig{Wireless: row.w}, false, "", refuseRules)

		if row.want == "" {
			if err != nil {
				t.Errorf("%s: refused: %v", row.name, err)
			}

			continue
		}

		if err == nil || !strings.Contains(err.Error(), row.want) {
			t.Errorf("%s: Validate returned %v, want a refusal with %q", row.name, err, row.want)
		}
	}
}

func TestValidateNoSecretInError(t *testing.T) {
	const passphrase = "Sup3r-Secret-Phrase"

	// Only the errors are checked: none may carry the passphrase, its hex form or its PSK.
	secrets := []string{passphrase, hex.EncodeToString([]byte(passphrase)), pskOf(t, "Velho", passphrase)}
	for _, s := range secrets {
		if s == "" {
			t.Fatal("an empty secret matches every error")
		}
	}

	refused := []struct {
		name string
		w    *api.SystemNetworkWireless
	}{
		{"open with a passphrase", savedNetworks(api.SystemNetworkWirelessNetwork{SSID: "Velho", Security: api.SystemNetworkWirelessSecurityOpen, Passphrase: passphrase})},
		{"enterprise with a passphrase", savedNetworks(api.SystemNetworkWirelessNetwork{SSID: "Velho", Security: "enterprise", Passphrase: passphrase})},
		{"a duplicate with a passphrase", savedNetworks(
			api.SystemNetworkWirelessNetwork{SSID: "Velho", Security: api.SystemNetworkWirelessSecurityWPAPersonal, Passphrase: passphrase},
			api.SystemNetworkWirelessNetwork{SSID: "Velho", Security: api.SystemNetworkWirelessSecurityWPAPersonal, Passphrase: passphrase})},
		{"a control character", savedNetworks(api.SystemNetworkWirelessNetwork{SSID: "Velho", Security: api.SystemNetworkWirelessSecurityWPAPersonal, Passphrase: passphrase + "\t"})},
		{"over 128 bytes", savedNetworks(api.SystemNetworkWirelessNetwork{SSID: "Velho", Security: api.SystemNetworkWirelessSecurityWPAPersonal, Passphrase: passphrase + strings.Repeat("x", 120)})},
		{"invalid UTF-8", savedNetworks(api.SystemNetworkWirelessNetwork{SSID: "Velho", Security: api.SystemNetworkWirelessSecurityWPAPersonal, Passphrase: passphrase + "\xff"})},
		{"a BSSID that is not a MAC address", savedNetworks(api.SystemNetworkWirelessNetwork{SSID: "Velho", Security: api.SystemNetworkWirelessSecurityWPAPersonal, Passphrase: passphrase, BSSID: "bad"})},
	}

	for _, row := range refused {
		err := Validate(&api.SystemNetworkConfig{Wireless: row.w}, false, "", nil)
		if err == nil {
			t.Errorf("%s: accepted", row.name)

			continue
		}

		for _, s := range secrets {
			if strings.Contains(err.Error(), s) {
				t.Errorf("%s: the error carries a passphrase, its hex form or its PSK: %v", row.name, err)
			}
		}
	}
}

// addSysDevice adds a network device of a fake sysfs, a Wi-Fi device when wifi, with its hardware address.
func addSysDevice(t *testing.T, root string, name string, wifi bool, hwaddr string) {
	t.Helper()

	dir := filepath.Join(root, "class", "net", name)
	if wifi {
		dir = filepath.Join(dir, "phy80211")
	}

	err := os.MkdirAll(dir, 0o755)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(root, "class", "net", name, "address"), []byte(hwaddr+"\n"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
}

func TestValidateRequireValidMAC(t *testing.T) {
	root := t.TempDir()
	addSysDevice(t, root, "wlan0", true, "0a:00:00:00:00:10")
	addSysDevice(t, root, "enp1s0", false, "02:00:00:00:00:01")

	wifiOnly := savedNetworks(api.SystemNetworkWirelessNetwork{SSID: "Velho", Security: api.SystemNetworkWirelessSecurityOpen})

	rows := []struct {
		name            string
		requireValidMAC bool
		sysRoot         string
		interfaces      []api.SystemNetworkInterface
		want            string
	}{
		{"a wired interface", true, root, []api.SystemNetworkInterface{{Name: "enp1s0", Hwaddr: "02:00:00:00:00:01"}}, ""},
		{"a Wi-Fi device by name", true, root, []api.SystemNetworkInterface{{Name: "wlan0"}}, "is a Wi-Fi device"},
		{"a Wi-Fi device by hardware address", true, root, []api.SystemNetworkInterface{{Name: "eth9", Hwaddr: "0a:00:00:00:00:10"}}, "is a Wi-Fi device"},
		{"a Wi-Fi device by upper-case hardware address", true, root, []api.SystemNetworkInterface{{Name: "eth9", Hwaddr: "0A:00:00:00:00:10"}}, "is a Wi-Fi device"},
		{"a Wi-Fi name without requireValidMAC", false, root, []api.SystemNetworkInterface{{Name: "wlan0"}}, ""},
		{"a Wi-Fi hardware address without requireValidMAC", false, root, []api.SystemNetworkInterface{{Name: "eth9", Hwaddr: "0a:00:00:00:00:10"}}, ""},
		{"no sysfs root", true, "", []api.SystemNetworkInterface{{Name: "wlan0"}}, ""},
		{"a sysfs root with no network devices", true, filepath.Join(root, "missing"), []api.SystemNetworkInterface{{Name: "wlan0"}}, ""},
	}

	for _, row := range rows {
		cfg := &api.SystemNetworkConfig{Interfaces: row.interfaces, Wireless: wifiOnly}

		err := Validate(cfg, row.requireValidMAC, row.sysRoot, nil)
		if row.want == "" && err != nil {
			t.Errorf("%s: refused: %v", row.name, err)
		}

		if row.want != "" && (err == nil || !strings.Contains(err.Error(), row.want)) {
			t.Errorf("%s: Validate returned %v, want a refusal with %q", row.name, err, row.want)
		}
	}

	// Without a wireless object the interfaces are still checked, and nothing else is.
	err := Validate(&api.SystemNetworkConfig{Interfaces: []api.SystemNetworkInterface{{Name: "wlan0"}}}, true, root, nil)
	if err == nil {
		t.Error("an interface named after a Wi-Fi device was accepted without a wireless object")
	}

	err = Validate(&api.SystemNetworkConfig{}, true, root, nil)
	if err != nil {
		t.Errorf("an empty configuration was refused: %v", err)
	}
}
