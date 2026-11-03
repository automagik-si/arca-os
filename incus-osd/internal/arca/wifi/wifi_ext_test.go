package wifi_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/arca/wifi"
	"github.com/lxc/incus-os/incus-osd/internal/network"
)

// fakeSys makes a sysfs with the network device name, a Wi-Fi device when isWifi.
func fakeSys(t *testing.T, name string, isWifi bool) string {
	t.Helper()

	root := t.TempDir()

	dir := filepath.Join(root, "class", "net", name)
	if isWifi {
		dir = filepath.Join(dir, "phy80211")
	}

	err := os.MkdirAll(dir, 0o755)
	if err != nil {
		t.Fatal(err)
	}

	return root
}

func TestValidateZeroAddressesWiredOnly(t *testing.T) { //nolint:paralleltest // points the package's sysfs at a fake tree
	wiredOnly := func() *api.SystemNetworkConfig {
		return &api.SystemNetworkConfig{
			Interfaces: []api.SystemNetworkInterface{{Name: "enp1s0", Hwaddr: "00:16:3e:00:00:01"}},
			Wireless:   wifi.Defaults(),
		}
	}

	// The first pass does not look at this machine's devices, so the zero-address
	// rule leaves the decision to the pass that does.
	err := network.ValidateNetworkConfiguration(context.TODO(), wiredOnly(), false)
	if err != nil {
		t.Errorf("the first pass refused a wired interface without addresses plus the wireless defaults: %v", err)
	}

	// The pass that looks at the hardware, on a machine without a Wi-Fi device:
	// upstream's error.
	wifi.UseSysRoot(t, fakeSys(t, "enp1s0", false))

	err = network.ValidateNetworkConfiguration(context.TODO(), wiredOnly(), true)
	if err == nil || !strings.Contains(err.Error(), "zero IP addresses") {
		t.Errorf("without a Wi-Fi device, a wired interface without addresses plus the wireless defaults gave %v, want the zero IP addresses error", err)
	}

	// The same pass on a machine with a Wi-Fi device: the defaults count. The
	// wired MAC is not on this machine, so another check may refuse it.
	wifi.UseSysRoot(t, fakeSys(t, "wlan0", true))

	err = network.ValidateNetworkConfiguration(context.TODO(), wiredOnly(), true)
	if err != nil && strings.Contains(err.Error(), "zero IP addresses") {
		t.Errorf("with a Wi-Fi device, the wireless defaults did not count: %v", err)
	}
}

func TestValidateWifiOnlyDefaults(t *testing.T) { //nolint:paralleltest // points the package's sysfs at a fake tree
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
