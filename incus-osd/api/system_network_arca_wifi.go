package api

import (
	"time"
)

const (
	// SystemNetworkWirelessSecurityWPAPersonal is a WPA2/WPA3-Personal network (a passphrase).
	SystemNetworkWirelessSecurityWPAPersonal = "wpa-personal"

	// SystemNetworkWirelessSecurityOpen is a network without encryption.
	SystemNetworkWirelessSecurityOpen = "open"
)

// SystemNetworkWireless holds the Wi-Fi configuration. Networks are keyed by
// network, not by device: any Wi-Fi device present may join any of them.
type SystemNetworkWireless struct {
	Country       string                         `json:"country,omitempty"        yaml:"country,omitempty"`
	Addresses     []string                       `json:"addresses,omitempty"      yaml:"addresses,omitempty"`
	FirewallRules []SystemNetworkFirewallRule    `json:"firewall_rules,omitempty" yaml:"firewall_rules,omitempty"`
	Networks      []SystemNetworkWirelessNetwork `json:"networks,omitempty"       yaml:"networks,omitempty"`
}

// HasNetworks reports whether at least one network is saved. It is safe on a nil receiver.
func (w *SystemNetworkWireless) HasNetworks() bool {
	return w != nil && len(w.Networks) > 0
}

// SystemNetworkWirelessNetwork is one saved network. List order is priority, first is highest.
type SystemNetworkWirelessNetwork struct {
	SSID       string `json:"ssid"                 yaml:"ssid"`
	Security   string `json:"security"             yaml:"security"`
	Passphrase string `json:"passphrase,omitempty" yaml:"passphrase,omitempty"`
	BSSID      string `json:"bssid,omitempty"      yaml:"bssid,omitempty"`
	Hidden     bool   `json:"hidden,omitempty"     yaml:"hidden,omitempty"`
}

// SystemNetworkWirelessState holds the association of one Wi-Fi link.
type SystemNetworkWirelessState struct {
	SSID       string `json:"ssid"       yaml:"ssid"`
	BSSID      string `json:"bssid"      yaml:"bssid"`
	Frequency  int    `json:"frequency"  yaml:"frequency"`  // MHz
	Signal     int    `json:"signal"     yaml:"signal"`     // dBm
	Generation int    `json:"generation" yaml:"generation"` // 7, 6, 5, 4, or 0 for older
	Width      int    `json:"width"      yaml:"width"`      // MHz
}

// SystemNetworkConnectivity holds the result of the last connectivity check.
type SystemNetworkConnectivity struct {
	Online    bool      `json:"online"           yaml:"online"`
	LastCheck time.Time `json:"last_check"       yaml:"last_check"`
	Reason    string    `json:"reason,omitempty" yaml:"reason,omitempty"`
}
