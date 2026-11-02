package wifi_test

import (
	"testing"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/arca/wifi"
	"github.com/lxc/incus-os/incus-osd/internal/seed"
)

func TestEmptyDevices(t *testing.T) {
	saved := &api.SystemNetworkWireless{
		Addresses: []string{"dhcp4", "slaac"},
		Networks:  []api.SystemNetworkWirelessNetwork{{SSID: "Velho", Security: api.SystemNetworkWirelessSecurityOpen}},
	}

	rows := []struct {
		name  string
		cfg   api.SystemNetworkConfig
		empty bool
	}{
		{"wired interface only", api.SystemNetworkConfig{Interfaces: []api.SystemNetworkInterface{{Name: "enp1s0"}}, Wireless: wifi.Defaults()}, false},
		{"wired bond only", api.SystemNetworkConfig{Bonds: []api.SystemNetworkBond{{Name: "bond0"}}}, false},
		{"wired VLAN only", api.SystemNetworkConfig{VLANs: []api.SystemNetworkVLAN{{Name: "vlan10"}}}, false},
		{"Wi-Fi only, with a saved network", api.SystemNetworkConfig{Wireless: saved}, false},
		{"Wi-Fi defaults, with no saved network", api.SystemNetworkConfig{Wireless: wifi.Defaults()}, true},
		{"nothing", api.SystemNetworkConfig{}, true},
	}

	for _, row := range rows {
		if got := seed.NetworkConfigHasEmptyDevices(row.cfg); got != row.empty {
			t.Errorf("%s: NetworkConfigHasEmptyDevices returned %v, want %v", row.name, got, row.empty)
		}
	}
}
