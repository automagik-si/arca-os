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

// machineSysRoot is this machine's sysfs. Tests point it at a fake one.
var machineSysRoot = "/sys"

// SysRoot returns the sysfs root that validation looks at: this machine's when
// the configuration must match its hardware, and none otherwise (a first pass,
// or a configuration checked on another machine, such as a build host).
func SysRoot(requireValidMAC bool) string {
	if requireValidMAC {
		return machineSysRoot
	}

	return ""
}

// CountAddresses returns how many Wi-Fi addresses count towards the configuration
// having at least one. The defaults count when no wired device is configured, when
// a network is saved, or when a Wi-Fi device exists, so a wired machine without
// Wi-Fi keeps its "zero IP addresses" error. With an empty sysRoot the devices are
// unknown and the defaults count: the pass that looks at the hardware decides.
func CountAddresses(cfg *api.SystemNetworkConfig, sysRoot string) int {
	if cfg == nil || cfg.Wireless == nil {
		return 0
	}

	wired := len(cfg.Interfaces) > 0 || len(cfg.Bonds) > 0 || len(cfg.VLANs) > 0
	if !wired || cfg.Wireless.HasNetworks() || sysRoot == "" {
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

// SSIDValid reports whether s is an acceptable network name: 1-32 bytes of UTF-8.
func SSIDValid(s string) bool {
	return s != "" && len(s) <= 32 && utf8.ValidString(s)
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
	default:
		// No DHCP address is requested; the link keeps the addresses it has.
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
// with the defaults when the configuration has no wireless object. A configuration
// that Validate refuses writes nothing.
func WriteFiles(root string, cfg *api.SystemNetworkConfig) error {
	err := Validate(cfg, false, "", nil)
	if err != nil {
		return err
	}

	if root == "" {
		root = "/"
	}

	w := Defaults()
	if cfg != nil && cfg.Wireless != nil {
		w = cfg.Wireless
	}

	dir := filepath.Join(root, RunDir)

	err = os.MkdirAll(dir, 0o700)
	if err != nil {
		return err
	}

	// G302 asks for a file mode of 0600 or less; a directory needs the execute bit.
	err = os.Chmod(dir, 0o700) //nolint:gosec // a directory is 0700, its files 0600
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
		_ = conn.Close() //nolint:contextcheck // Close detaches with its own one-second deadline
	}

	return nil
}
