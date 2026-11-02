package wifi_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/arca/wifi"
	"github.com/lxc/incus-os/incus-osd/internal/network"
)

func TestValidateZeroAddressesWiredOnly(t *testing.T) {
	cfg := &api.SystemNetworkConfig{
		Interfaces: []api.SystemNetworkInterface{{Name: "enp1s0", Hwaddr: "00:16:3e:00:00:01"}},
		Wireless:   wifi.Defaults(),
	}

	err := network.ValidateNetworkConfiguration(context.TODO(), cfg, false)
	if err == nil || !strings.Contains(err.Error(), "zero IP addresses") {
		t.Errorf("a wired interface without addresses plus the wireless defaults gave %v, want the zero IP addresses error", err)
	}
}

func TestValidateWifiOnlyDefaults(t *testing.T) {
	cfg := &api.SystemNetworkConfig{Wireless: wifi.Defaults()}

	err := network.ValidateNetworkConfiguration(context.TODO(), cfg, false)
	if err != nil {
		t.Errorf("a config with no interfaces and the wireless defaults does not validate: %v", err)
	}

	cfg.Wireless.Addresses = []string{"10.42.1.5/24"}

	err = network.ValidateNetworkConfiguration(context.TODO(), cfg, false)
	if err == nil {
		t.Error("a static wireless address validated")
	}
}
