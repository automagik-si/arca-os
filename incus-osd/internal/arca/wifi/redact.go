package wifi

import (
	"github.com/lxc/incus-os/incus-osd/api"
)

// Redact returns a copy of cfg for a GET response: the same configuration, with the
// passphrase of every saved Wi-Fi network left out. cfg itself is not changed. It is
// safe on a nil configuration and on one without a wireless object.
func Redact(cfg *api.SystemNetworkConfig) *api.SystemNetworkConfig {
	if cfg == nil {
		return nil
	}

	out := *cfg

	if cfg.Wireless != nil {
		w := *cfg.Wireless

		if cfg.Wireless.Networks != nil {
			w.Networks = make([]api.SystemNetworkWirelessNetwork, len(cfg.Wireless.Networks))

			for i, n := range cfg.Wireless.Networks {
				n.Passphrase = ""
				w.Networks[i] = n
			}
		}

		out.Wireless = &w
	}

	return &out
}

// KeepSecrets fills each saved network of next that has no passphrase with the passphrase
// of the network in prior that is the same saved network: the same SSID, BSSID and security.
// A client that sends back what GET returned therefore keeps every passphrase it did not
// change, while a network switched to another security keeps none. It changes next, and it
// is safe on nil configurations and on configurations without a wireless object.
func KeepSecrets(next, prior *api.SystemNetworkConfig) {
	if next == nil || next.Wireless == nil || prior == nil || prior.Wireless == nil {
		return
	}

	for i := range next.Wireless.Networks {
		n := &next.Wireless.Networks[i]
		if n.Passphrase != "" {
			continue
		}

		for _, p := range prior.Wireless.Networks {
			if p.SSID == n.SSID && p.BSSID == n.BSSID && p.Security == n.Security && p.Passphrase != "" {
				n.Passphrase = p.Passphrase

				break
			}
		}
	}
}
