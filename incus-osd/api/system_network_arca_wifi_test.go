package api_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
)

func wirelessConfig(nets ...api.SystemNetworkWirelessNetwork) api.SystemNetwork {
	return api.SystemNetwork{
		Config: &api.SystemNetworkConfig{
			Wireless: &api.SystemNetworkWireless{Networks: nets},
		},
	}
}

func TestWithRedactedWirelessSecrets(t *testing.T) {
	t.Parallel()

	n := wirelessConfig(
		api.SystemNetworkWirelessNetwork{SSID: "home", Security: api.SystemNetworkWirelessSecurityWPAPersonal, Passphrase: "s3cret"},
		api.SystemNetworkWirelessNetwork{SSID: "cafe", Security: api.SystemNetworkWirelessSecurityOpen},
	)

	redacted := n.WithRedactedWirelessSecrets()

	// The returned copy carries no passphrases.
	require.Empty(t, redacted.Config.Wireless.Networks[0].Passphrase)
	require.Empty(t, redacted.Config.Wireless.Networks[1].Passphrase)
	// Non-secret fields survive.
	require.Equal(t, "home", redacted.Config.Wireless.Networks[0].SSID)

	// The original is untouched: redaction copied, not mutated in place.
	require.Equal(t, "s3cret", n.Config.Wireless.Networks[0].Passphrase)
}

func TestWithRedactedWirelessSecretsNoWireless(t *testing.T) {
	t.Parallel()

	// Safe on a config with no wireless section and on an empty network.
	require.NotPanics(t, func() {
		_ = api.SystemNetwork{Config: &api.SystemNetworkConfig{}}.WithRedactedWirelessSecrets()
		_ = api.SystemNetwork{}.WithRedactedWirelessSecrets()
	})
}

func TestFillMissingWirelessPassphrases(t *testing.T) {
	t.Parallel()

	stored := wirelessConfig(
		api.SystemNetworkWirelessNetwork{SSID: "home", Passphrase: "stored-home"},
		api.SystemNetworkWirelessNetwork{SSID: "work", BSSID: "aa:bb", Passphrase: "stored-work"},
		api.SystemNetworkWirelessNetwork{SSID: "open", Security: api.SystemNetworkWirelessSecurityOpen},
	)

	// A client PUTs back a read (passphrases omitted), changes one, and adds a new one.
	incoming := wirelessConfig(
		api.SystemNetworkWirelessNetwork{SSID: "home"},                                         // omitted -> keep stored
		api.SystemNetworkWirelessNetwork{SSID: "work", BSSID: "aa:bb", Passphrase: "new-work"}, // provided -> replace
		api.SystemNetworkWirelessNetwork{SSID: "guest", Passphrase: "guest-pass"},              // new SSID -> keep as given
		api.SystemNetworkWirelessNetwork{SSID: "nomatch"},                                      // no stored match -> stays empty
	)

	incoming.FillMissingWirelessPassphrases(stored)

	nets := incoming.Config.Wireless.Networks
	require.Equal(t, "stored-home", nets[0].Passphrase, "omitted passphrase should keep the stored one")
	require.Equal(t, "new-work", nets[1].Passphrase, "a provided passphrase should replace the stored one")
	require.Equal(t, "guest-pass", nets[2].Passphrase, "a new SSID keeps the supplied passphrase")
	require.Empty(t, nets[3].Passphrase, "an SSID with no stored match stays empty")
}

func TestFillMissingWirelessPassphrasesBSSIDMustMatch(t *testing.T) {
	t.Parallel()

	stored := wirelessConfig(api.SystemNetworkWirelessNetwork{SSID: "home", BSSID: "aa:bb", Passphrase: "stored"})
	// Same SSID, different BSSID: not the same saved network, so nothing is carried over.
	incoming := wirelessConfig(api.SystemNetworkWirelessNetwork{SSID: "home", BSSID: "cc:dd"})

	incoming.FillMissingWirelessPassphrases(stored)

	require.Empty(t, incoming.Config.Wireless.Networks[0].Passphrase)
}
