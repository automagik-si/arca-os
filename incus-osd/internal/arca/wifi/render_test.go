package wifi

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/lxc/incus-os/incus-osd/api"
)

func TestRenderSupplicantVariants(t *testing.T) {
	t.Parallel()

	const wpa = api.SystemNetworkWirelessSecurityWPAPersonal

	open := api.SystemNetworkWirelessSecurityOpen

	globals := []string{
		"ctrl_interface=DIR=" + CtrlDir + "\n",
		"update_config=0\n",
		"sae_pwe=2\n",
		"p2p_disabled=1\n",
		"autoscan=exponential:3:300\n",
	}

	render := func(w *api.SystemNetworkWireless) string {
		t.Helper()

		return RenderSupplicant(w)
	}

	t.Run("globals", func(t *testing.T) {
		t.Parallel()

		got := render(nil)
		for _, line := range globals {
			if !strings.Contains(got, line) {
				t.Errorf("the configuration lacks %q", line)
			}
		}

		if strings.Contains(got, "network=") {
			t.Error("a configuration with no networks has a network block")
		}
	})

	t.Run("sae alone", func(t *testing.T) {
		t.Parallel()

		// Seven characters are outside 8-63 printable ASCII, so the network is SAE only.
		got := render(&api.SystemNetworkWireless{Networks: []api.SystemNetworkWirelessNetwork{{SSID: "Solo", Security: wpa, Passphrase: "short7x"}}})
		for _, want := range []string{"\tkey_mgmt=SAE\n\tieee80211w=1\n", "\tsae_password=" + hex.EncodeToString([]byte("short7x")) + "\n"} {
			if !strings.Contains(got, want) {
				t.Errorf("the SAE-only network lacks %q", want)
			}
		}

		if strings.Contains(got, "psk=") || strings.Contains(got, "WPA-PSK") {
			t.Error("the SAE-only network renders a WPA-PSK key")
		}
	})

	t.Run("open", func(t *testing.T) {
		t.Parallel()

		got := render(&api.SystemNetworkWireless{Networks: []api.SystemNetworkWirelessNetwork{{SSID: "Aberta", Security: open}}})
		if !strings.Contains(got, "\tkey_mgmt=NONE\n}\n") {
			t.Error("the open network has no key_mgmt=NONE block")
		}

		for _, key := range []string{"psk=", "sae_password=", "ieee80211w="} {
			if strings.Contains(got, key) {
				t.Errorf("the open network renders %q", key)
			}
		}
	})

	t.Run("hidden and pinned", func(t *testing.T) {
		t.Parallel()

		got := render(&api.SystemNetworkWireless{Networks: []api.SystemNetworkWirelessNetwork{
			{SSID: "Oculta", Security: open, Hidden: true},
			{SSID: "Fixa", Security: open, BSSID: "02:AA:BB:CC:DD:EE"},
		}})
		if !strings.Contains(got, "\tscan_ssid=1\n") {
			t.Error("the hidden network lacks scan_ssid=1")
		}

		if !strings.Contains(got, "\tbssid=02:aa:bb:cc:dd:ee\n") {
			t.Error("the pinned network lacks its lowercase bssid")
		}
	})

	t.Run("country", func(t *testing.T) {
		t.Parallel()

		with := render(&api.SystemNetworkWireless{Country: "US"})
		if !strings.Contains(with, "country=US\n") {
			t.Error("a set country is not rendered")
		}

		if strings.Contains(render(nil), "country=") {
			t.Error("an unset country is rendered")
		}
	})

	t.Run("priority", func(t *testing.T) {
		t.Parallel()

		got := render(&api.SystemNetworkWireless{Networks: []api.SystemNetworkWirelessNetwork{
			{SSID: "Um", Security: open}, {SSID: "Dois", Security: open}, {SSID: "Tres", Security: open},
		}})

		blocks := strings.Split(got, "network={")
		if len(blocks) != 4 {
			t.Fatalf("%d network blocks, want 3", len(blocks)-1)
		}

		for i, want := range []string{"\tpriority=3\n", "\tpriority=2\n", "\tpriority=1\n"} {
			if !strings.Contains(blocks[i+1], want) {
				t.Errorf("network %d lacks %q", i+1, want)
			}
		}
	})

	t.Run("names as hex", func(t *testing.T) {
		t.Parallel()

		// Quotes, spaces and non-ASCII UTF-8 must reach the file only as hex.
		ssid := "Café \"one\" two"

		got := render(&api.SystemNetworkWireless{Networks: []api.SystemNetworkWirelessNetwork{{SSID: ssid, Security: wpa, Passphrase: "password"}}})
		if !strings.Contains(got, "\tssid="+hex.EncodeToString([]byte(ssid))+"\n") {
			t.Error("the SSID is not rendered as its hex")
		}

		for _, raw := range []string{ssid, "Caf", "\"", "é"} {
			if strings.Contains(got, raw) {
				t.Errorf("the configuration holds %q in clear", raw)
			}
		}
	})
}
