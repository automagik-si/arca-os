// Package wifi renders and applies the Wi-Fi configuration: one systemd-networkd
// .network that matches every Wi-Fi station, and one wpa_supplicant configuration
// that the supplicant of each Wi-Fi device reads. It imports only the API and the
// standard library, because the seed package (linked into flasher-tool) uses it.
package wifi

import (
	"context"
	"crypto/pbkdf2"
	"crypto/sha1" //nolint:gosec // WPA-PSK derives its key with PBKDF2-SHA1.
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/lxc/incus-os/incus-osd/api"
)

const (
	// RunDir holds the supplicant configuration and control sockets (mode 0700).
	RunDir = "/run/incus-os/wifi"

	// SupplicantConf is the configuration every supplicant reads (mode 0600).
	SupplicantConf = RunDir + "/wpa_supplicant.conf"

	// CtrlDir holds one control socket per Wi-Fi device.
	CtrlDir = RunDir + "/ctrl"

	// NetworkFile is the systemd-networkd configuration of every Wi-Fi station.
	NetworkFile = "/run/systemd/network/30-arca-wifi.network"

	// Unit is the supplicant unit of one Wi-Fi device.
	Unit = "arca-wifi@%s.service"

	// MaxPassphraseBytes is the longest accepted WPA-Personal passphrase.
	MaxPassphraseBytes = 128
)

// Defaults returns the Wi-Fi configuration of a new system: DHCPv4 and SLAAC, no networks.
func Defaults() *api.SystemNetworkWireless {
	return &api.SystemNetworkWireless{Addresses: []string{"dhcp4", "slaac"}}
}

// Devices returns the sorted names of the Wi-Fi devices under <sysRoot>/class/net.
// An empty sysRoot means no hardware is looked at, and returns none.
func Devices(sysRoot string) ([]string, error) {
	if sysRoot == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(filepath.Join(sysRoot, "class", "net"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, err
	}

	devices := []string{}

	for _, entry := range entries {
		if IsDevice(sysRoot, entry.Name()) {
			devices = append(devices, entry.Name())
		}
	}

	slices.Sort(devices)

	return devices, nil
}

// IsDevice reports whether the network device name is a Wi-Fi device (it has a phy80211).
func IsDevice(sysRoot string, name string) bool {
	if sysRoot == "" || name == "" || strings.Contains(name, "/") {
		return false
	}

	_, err := os.Stat(filepath.Join(sysRoot, "class", "net", name, "phy80211"))

	return err == nil
}

// SysRoot returns the sysfs root that validation looks at: "/sys" when the
// configuration must match this machine's hardware, and none otherwise.
func SysRoot(requireValidMAC bool) string {
	if requireValidMAC {
		return "/sys"
	}

	return ""
}

// CountAddresses returns how many Wi-Fi addresses count towards the configuration
// having at least one. The defaults count only when no wired device is configured,
// when a network is saved, or when a Wi-Fi device exists, so a wired machine keeps
// its "zero IP addresses" error.
func CountAddresses(cfg *api.SystemNetworkConfig, sysRoot string) int {
	if cfg == nil || cfg.Wireless == nil {
		return 0
	}

	wired := len(cfg.Interfaces) > 0 || len(cfg.Bonds) > 0 || len(cfg.VLANs) > 0
	if !wired || cfg.Wireless.HasNetworks() {
		return len(cfg.Wireless.Addresses)
	}

	devices, _ := Devices(sysRoot)
	if len(devices) > 0 {
		return len(cfg.Wireless.Addresses)
	}

	return 0
}

// MergeSeedDefaults gives a seed's network configuration the default Wi-Fi
// configuration when it has none; a seed's own wireless object is kept.
func MergeSeedDefaults(cfg *api.SystemNetworkConfig, defaults *api.SystemNetworkConfig) {
	if cfg != nil && defaults != nil && cfg.Wireless == nil {
		cfg.Wireless = defaults.Wireless
	}
}

// ResetState returns a fresh network state that keeps only whether a configuration
// is being applied, so a state refresh never clears that guard.
func ResetState(prev api.SystemNetworkState) api.SystemNetworkState {
	return api.SystemNetworkState{
		Interfaces:             make(map[string]api.SystemNetworkInterfaceState),
		ConfigurationInProcess: prev.ConfigurationInProcess,
	}
}

// PassphraseValid reports whether p is an acceptable WPA-Personal passphrase:
// 1-128 bytes of UTF-8 with no control character.
func PassphraseValid(p string) bool {
	if p == "" || len(p) > MaxPassphraseBytes || !utf8.ValidString(p) {
		return false
	}

	return !strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// PSKCompatible reports whether p can also be used for WPA-PSK: 8-63 printable ASCII characters.
func PSKCompatible(p string) bool {
	if len(p) < 8 || len(p) > 63 {
		return false
	}

	for i := range len(p) {
		if p[i] < 0x20 || p[i] > 0x7e {
			return false
		}
	}

	return true
}

// Validate checks the wireless object of a network configuration. No error carries
// a passphrase. requireValidMAC and sysRoot say whether this machine's hardware is
// looked at; this version checks none.
func Validate(cfg *api.SystemNetworkConfig, requireValidMAC bool, sysRoot string, validateFirewall func([]api.SystemNetworkFirewallRule) error) error { //nolint:revive
	_, _ = requireValidMAC, sysRoot

	if cfg == nil || cfg.Wireless == nil {
		return nil
	}

	w := cfg.Wireless

	for i, address := range w.Addresses {
		if !slices.Contains([]string{"dhcp4", "dhcp6", "slaac"}, address) {
			return fmt.Errorf("wireless address %d: %q is not dhcp4, dhcp6 or slaac", i, address)
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

	for i, n := range w.Networks {
		if n.SSID == "" || len(n.SSID) > 32 || !utf8.ValidString(n.SSID) {
			return fmt.Errorf("wireless network %d: the SSID must be 1-32 bytes of UTF-8", i)
		}

		if n.BSSID != "" {
			mac, err := net.ParseMAC(n.BSSID)
			if err != nil || len(mac) != 6 || mac.String() != strings.ToLower(n.BSSID) {
				return fmt.Errorf("wireless network %d: the BSSID is not a MAC address like 00:11:22:33:44:55", i)
			}
		}

		switch n.Security {
		case api.SystemNetworkWirelessSecurityWPAPersonal:
			if !PassphraseValid(n.Passphrase) {
				return fmt.Errorf("wireless network %d: the passphrase must be 1-%d bytes of UTF-8 with no control character", i, MaxPassphraseBytes)
			}
		case api.SystemNetworkWirelessSecurityOpen:
			if n.Passphrase != "" {
				return fmt.Errorf("wireless network %d: an open network takes no passphrase", i)
			}
		default:
			return fmt.Errorf("wireless network %d: security %q is not wpa-personal or open", i, n.Security)
		}
	}

	return nil
}

func isLetters(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return r < 'A' || r > 'Z' })
}

// RenderNetwork returns the .network of every Wi-Fi station.
func RenderNetwork(cfg *api.SystemNetworkConfig) string {
	w := Defaults()
	if cfg != nil && cfg.Wireless != nil {
		w = cfg.Wireless
	}

	var b strings.Builder

	b.WriteString("[Match]\nType=wlan\nWLANInterfaceType=station\n\n[Link]\nRequiredForOnline=no\n\n[Network]\n")

	if cfg != nil && cfg.DNS != nil {
		if len(cfg.DNS.SearchDomains) > 0 {
			b.WriteString("Domains=" + strings.Join(cfg.DNS.SearchDomains, " ") + "\n")
		}

		for _, ns := range cfg.DNS.Nameservers {
			b.WriteString("DNS=" + ns + "\n")
		}

		if cfg.DNS.DNSOverTLS {
			b.WriteString("DNSOverTLS=yes\n")
		}
	}

	if cfg != nil && cfg.Time != nil {
		for _, ntp := range cfg.Time.NTPServers {
			b.WriteString("NTP=" + ntp + "\n")
		}
	}

	if len(w.Addresses) == 0 {
		b.WriteString("LinkLocalAddressing=no\n")
	} else {
		b.WriteString("LinkLocalAddressing=ipv6\n")
	}

	b.WriteString("IgnoreCarrierLoss=3s\n")

	dhcp4 := slices.Contains(w.Addresses, "dhcp4")
	dhcp6 := slices.Contains(w.Addresses, "dhcp6")

	if dhcp6 || slices.Contains(w.Addresses, "slaac") {
		b.WriteString("IPv6AcceptRA=true\n")
	} else {
		b.WriteString("IPv6AcceptRA=false\n")
	}

	switch {
	case dhcp4 && dhcp6:
		b.WriteString("DHCP=yes\n")
	case dhcp4:
		b.WriteString("DHCP=ipv4\n")
	case dhcp6:
		b.WriteString("DHCP=ipv6\n")
	}

	b.WriteString("\n[DHCPv4]\nClientIdentifier=mac\nRouteMetric=600\nUseMTU=true\n")
	b.WriteString("\n[DHCPv6]\nWithoutRA=solicit\n")
	b.WriteString("\n[IPv6AcceptRA]\nRouteMetric=600\n")

	return b.String()
}

// RenderSupplicant returns the wpa_supplicant configuration. Every SSID and SAE
// password is hex, and a WPA-PSK key is the PBKDF2 PSK, so nothing needs quoting.
func RenderSupplicant(w *api.SystemNetworkWireless) string {
	var b strings.Builder

	b.WriteString("ctrl_interface=DIR=" + CtrlDir + "\nupdate_config=0\nsae_pwe=2\np2p_disabled=1\nautoscan=exponential:3:300\n")

	if w == nil {
		return b.String()
	}

	if w.Country != "" {
		b.WriteString("country=" + w.Country + "\n")
	}

	for i, n := range w.Networks {
		b.WriteString("\nnetwork={\n\tssid=" + hex.EncodeToString([]byte(n.SSID)) + "\n")

		if n.Hidden {
			b.WriteString("\tscan_ssid=1\n")
		}

		if n.BSSID != "" {
			b.WriteString("\tbssid=" + strings.ToLower(n.BSSID) + "\n")
		}

		fmt.Fprintf(&b, "\tpriority=%d\n", len(w.Networks)-i)

		if n.Security != api.SystemNetworkWirelessSecurityWPAPersonal {
			b.WriteString("\tkey_mgmt=NONE\n}\n")

			continue
		}

		psk := ""

		if PSKCompatible(n.Passphrase) {
			key, err := pbkdf2.Key(sha1.New, n.Passphrase, []byte(n.SSID), 4096, 32)
			if err == nil {
				psk = hex.EncodeToString(key)
			}
		}

		if psk != "" {
			b.WriteString("\tkey_mgmt=SAE WPA-PSK WPA-PSK-SHA256\n\tieee80211w=1\n\tpsk=" + psk + "\n")
		} else {
			b.WriteString("\tkey_mgmt=SAE\n\tieee80211w=1\n")
		}

		b.WriteString("\tsae_password=" + hex.EncodeToString([]byte(n.Passphrase)) + "\n}\n")
	}

	return b.String()
}

// WriteFiles writes the supplicant configuration (0600, in the 0700 RunDir) and
// the .network (0644) under root, "" meaning /. Both are written on every apply,
// with the defaults when the configuration has no wireless object.
func WriteFiles(root string, cfg *api.SystemNetworkConfig) error {
	if root == "" {
		root = "/"
	}

	w := Defaults()
	if cfg != nil && cfg.Wireless != nil {
		w = cfg.Wireless
	}

	dir := filepath.Join(root, RunDir)

	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		return err
	}

	err = os.Chmod(dir, 0o700)
	if err != nil {
		return err
	}

	err = writeFile(filepath.Join(root, SupplicantConf), RenderSupplicant(w), 0o600)
	if err != nil {
		return err
	}

	err = os.MkdirAll(filepath.Join(root, filepath.Dir(NetworkFile)), 0o755)
	if err != nil {
		return err
	}

	return writeFile(filepath.Join(root, NetworkFile), RenderNetwork(cfg), 0o644)
}

// writeFile replaces path with content through a temporary file in the same directory.
func writeFile(path string, content string, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}

	defer func() { _ = os.Remove(f.Name()) }()

	_, err = f.WriteString(content)
	if err == nil {
		err = f.Chmod(mode)
	}

	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}

	if err != nil {
		return err
	}

	return os.Rename(f.Name(), path)
}

// Activate starts the supplicant of every Wi-Fi device and asks each running one
// to reread its configuration.
func Activate(ctx context.Context, sysRoot string, start func(ctx context.Context, units ...string) error) error {
	devices, err := Devices(sysRoot)
	if err != nil || len(devices) == 0 {
		return err
	}

	units := make([]string, 0, len(devices))
	for _, device := range devices {
		units = append(units, fmt.Sprintf(Unit, device))
	}

	err = start(ctx, units...)
	if err != nil {
		return err
	}

	// A supplicant that has just started has read the new file; one that was
	// already running rereads it. A socket that is not up yet is skipped.
	for _, device := range devices {
		conn, err := Dial(CtrlDir, device)
		if err != nil {
			continue
		}

		_, _ = conn.Request(ctx, "RECONFIGURE")
		_ = conn.Close()
	}

	return nil
}
