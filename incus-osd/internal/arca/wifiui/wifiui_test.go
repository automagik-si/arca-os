package wifiui

import (
	"context"
	"strings"
	"testing"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/arca/wifi"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

func TestCommitWaitsOnGuards(t *testing.T) {
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
