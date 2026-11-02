package wifi

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lxc/incus-os/incus-osd/api"
)

func TestRedact(t *testing.T) {
	if Redact(nil) != nil {
		t.Error("Redact of nil is not nil")
	}

	bare := &api.SystemNetworkConfig{Time: &api.SystemNetworkTime{Timezone: "UTC"}}
	if got := Redact(bare); got == nil || got.Wireless != nil || got.Time == nil || got.Time.Timezone != "UTC" {
		t.Errorf("Redact of a configuration without a wireless object changed it: %+v", got)
	}

	const passphrase = "password-Velho"

	cfg := &api.SystemNetworkConfig{
		Wireless: &api.SystemNetworkWireless{
			Country:   "US",
			Addresses: []string{"dhcp4"},
			Networks: []api.SystemNetworkWirelessNetwork{
				{SSID: "Velho", Security: api.SystemNetworkWirelessSecurityWPAPersonal, Passphrase: passphrase},
				{SSID: "Cafe", Security: api.SystemNetworkWirelessSecurityOpen},
			},
		},
	}

	got := Redact(cfg)
	if got == nil || got.Wireless == nil || got == cfg || got.Wireless == cfg.Wireless {
		t.Fatal("Redact did not return a copy with its own wireless object")
	}

	if got.Wireless.Country != "US" || len(got.Wireless.Addresses) != 1 || len(got.Wireless.Networks) != 2 || got.Wireless.Networks[0].SSID != "Velho" {
		t.Errorf("Redact changed the other fields: %+v", got.Wireless)
	}

	for _, n := range got.Wireless.Networks {
		if n.Passphrase != "" {
			t.Errorf("Redact left the passphrase of %q", n.SSID)
		}
	}

	if cfg.Wireless.Networks[0].Passphrase != passphrase {
		t.Error("Redact changed the stored passphrase")
	}

	// A GET body carries neither the passphrase nor its hex form, and the stored configuration does.
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}

	for _, s := range []string{passphrase, hex.EncodeToString([]byte(passphrase))} {
		if strings.Contains(string(body), s) {
			t.Errorf("the redacted configuration holds %q", s)
		}
	}

	stored, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(stored), passphrase) {
		t.Error("the stored configuration lost its passphrase, so the check above proves nothing")
	}

	// A configuration with no networks keeps its nil list.
	none := Redact(&api.SystemNetworkConfig{Wireless: &api.SystemNetworkWireless{Addresses: []string{"slaac"}}})
	if none.Wireless.Networks != nil {
		t.Error("Redact made a list of networks out of a nil one")
	}
}

func TestKeepSecret(t *testing.T) {
	saved := &api.SystemNetworkConfig{
		Wireless: &api.SystemNetworkWireless{
			Addresses: []string{"dhcp4", "slaac"},
			Networks: []api.SystemNetworkWirelessNetwork{
				{SSID: "Velho", Security: api.SystemNetworkWirelessSecurityWPAPersonal, Passphrase: "password-Velho"},
				{SSID: "Cafe", Security: api.SystemNetworkWirelessSecurityOpen},
			},
		},
	}

	next := &api.SystemNetworkConfig{
		Wireless: &api.SystemNetworkWireless{
			Addresses: []string{"dhcp4", "slaac"},
			Networks: []api.SystemNetworkWirelessNetwork{
				// A saved network with the same SSID and security keeps its passphrase.
				{SSID: "Velho", Security: api.SystemNetworkWirelessSecurityWPAPersonal},
				// A passphrase that is sent is kept as sent, and a network not saved keeps none.
				{SSID: "Casa", Security: api.SystemNetworkWirelessSecurityWPAPersonal, Passphrase: "new-passphrase"},
				// The same SSID with another security does not match.
				{SSID: "Cafe", Security: api.SystemNetworkWirelessSecurityWPAPersonal},
			},
		},
	}

	KeepSecrets(next, saved)

	want := []string{"password-Velho", "new-passphrase", ""}
	for i, n := range next.Wireless.Networks {
		if n.Passphrase != want[i] {
			t.Errorf("network %d (%s): passphrase %q, want %q", i, n.SSID, n.Passphrase, want[i])
		}
	}

	if saved.Wireless.Networks[0].Passphrase != "password-Velho" {
		t.Error("KeepSecrets changed the saved configuration")
	}

	// Nil and wireless-less configurations are left alone.
	KeepSecrets(nil, saved)
	KeepSecrets(&api.SystemNetworkConfig{}, saved)
	KeepSecrets(next, nil)
	KeepSecrets(next, &api.SystemNetworkConfig{})

	// A new WPA-Personal network with no saved match and no passphrase is refused.
	nova := &api.SystemNetworkConfig{
		Wireless: &api.SystemNetworkWireless{
			Addresses: []string{"dhcp4"},
			Networks:  []api.SystemNetworkWirelessNetwork{{SSID: "Nova", Security: api.SystemNetworkWirelessSecurityWPAPersonal}},
		},
	}

	KeepSecrets(nova, saved)

	err := Validate(nova, false, "", nil)
	if err == nil || !strings.Contains(err.Error(), "the passphrase must be 1-128 bytes") {
		t.Errorf("a new WPA-Personal network without a passphrase: Validate returned %v", err)
	}
}
