package wifi

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/lxc/incus-os/incus-osd/api"
)

// bssidFormat matches a BSSID: six lowercase hex pairs, colon-separated.
var bssidFormat = regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)

// networkKey is what identifies a saved network: its SSID plus its security.
type networkKey struct {
	ssid     string
	security string
}

// Validate checks the wireless object of a network configuration. With requireValidMAC,
// it also refuses an interfaces entry that names a Wi-Fi device of sysRoot by name or by
// hardware address; without it, no hardware is read. No error carries a passphrase, its
// hex form or a PSK.
func Validate(cfg *api.SystemNetworkConfig, requireValidMAC bool, sysRoot string, validateFirewall func([]api.SystemNetworkFirewallRule) error) error {
	if cfg == nil {
		return nil
	}

	if requireValidMAC {
		err := refuseWiFiInterfaces(cfg.Interfaces, sysRoot)
		if err != nil {
			return err
		}
	}

	if cfg.Wireless == nil {
		return nil
	}

	w := cfg.Wireless

	for i, address := range w.Addresses {
		if !slices.Contains([]string{"dhcp4", "dhcp6", "slaac"}, address) {
			return fmt.Errorf("wireless address %d: %q is not dhcp4, dhcp6 or slaac", i, address)
		}

		if slices.Index(w.Addresses, address) != i {
			return fmt.Errorf("wireless address %d: %q is listed more than once", i, address)
		}
	}

	if w.Country != "" && (len(w.Country) != 2 || strings.ToUpper(w.Country) != w.Country || !isLetters(w.Country)) {
		return fmt.Errorf("wireless country %q is not an ISO 3166 alpha-2 code", w.Country)
	}

	if validateFirewall != nil {
		err := validateFirewall(w.FirewallRules)
		if err != nil {
			return fmt.Errorf("wireless %w", err)
		}
	}

	saved := make(map[networkKey]bool, len(w.Networks))

	for i, n := range w.Networks {
		if !SSIDValid(n.SSID) {
			return fmt.Errorf("wireless network %d: the SSID must be 1-32 bytes of UTF-8", i)
		}

		key := networkKey{ssid: n.SSID, security: n.Security}
		if saved[key] {
			return fmt.Errorf("wireless network %d: a network with the same SSID and security is listed before it", i)
		}

		saved[key] = true

		if n.BSSID != "" {
			mac, err := net.ParseMAC(n.BSSID)
			if !bssidFormat.MatchString(n.BSSID) || err != nil || mac[0]&1 != 0 {
				return fmt.Errorf("wireless network %d: the BSSID must be a unicast MAC address of six lowercase hex pairs, like 00:11:22:33:44:55", i)
			}
		}

		switch {
		case n.Security == api.SystemNetworkWirelessSecurityWPAPersonal:
			if !PassphraseValid(n.Passphrase) {
				return fmt.Errorf("wireless network %d: the passphrase must be 1-%d bytes of UTF-8 with no control character", i, MaxPassphraseBytes)
			}
		case n.Security == api.SystemNetworkWirelessSecurityOpen:
			if n.Passphrase != "" {
				return fmt.Errorf("wireless network %d: an open network takes no passphrase", i)
			}
		case isEnterprise(n.Security):
			return fmt.Errorf("wireless network %d: enterprise (802.1X) networks are not supported", i)
		default:
			return fmt.Errorf("wireless network %d: the security is not wpa-personal or open", i)
		}
	}

	return nil
}

// refuseWiFiInterfaces refuses an interfaces entry that names a Wi-Fi device of sysRoot,
// by its name or by its hardware address: the wireless object configures those devices.
func refuseWiFiInterfaces(interfaces []api.SystemNetworkInterface, sysRoot string) error {
	devices, err := Devices(sysRoot)
	if err != nil {
		return err
	}

	for _, device := range devices {
		hwaddr := readHwaddr(sysRoot, device)

		for i, iface := range interfaces {
			if iface.Name == device || (hwaddr != "" && strings.EqualFold(iface.Hwaddr, hwaddr)) {
				return fmt.Errorf("interface %d: %s is a Wi-Fi device, configure it under wireless", i, device)
			}
		}
	}

	return nil
}

// readHwaddr returns the hardware address of a network device of sysRoot, or "" when
// it cannot be read.
func readHwaddr(sysRoot string, device string) string {
	content, err := os.ReadFile(filepath.Join(sysRoot, "class", "net", device, "address"))
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(content))
}

// isEnterprise reports whether a security names an EAP (802.1X) network.
func isEnterprise(security string) bool {
	s := strings.ToLower(security)

	return s == "enterprise" || strings.Contains(s, "eap")
}

func isLetters(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return r < 'A' || r > 'Z' })
}
