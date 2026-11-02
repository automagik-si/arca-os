// Package wifiui runs Wi-Fi on the console. It activates only on a machine with a
// Wi-Fi device or a saved network, shows the picker while the machine is offline,
// and holds the first boot's update check until the machine is online.
package wifiui

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/applications"
	"github.com/lxc/incus-os/incus-osd/internal/arca/wifi"
	"github.com/lxc/incus-os/incus-osd/internal/network"
	"github.com/lxc/incus-os/incus-osd/internal/nftables"
	"github.com/lxc/incus-os/incus-osd/internal/providers"
	"github.com/lxc/incus-os/incus-osd/internal/seed"
	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/tui"
)

// defaultServerURL is the images provider's server when none is configured.
const defaultServerURL = "https://images.linuxcontainers.org/os"

var (
	// applyFunc applies a network configuration the way the API's PUT does, then saves state.
	applyFunc = func(ctx context.Context, s *state.State, cfg *api.SystemNetworkConfig) error {
		err := nftables.ApplyHwaddrFilters(ctx, cfg)
		if err != nil {
			return err
		}

		err = network.ApplyNetworkConfiguration(ctx, s, cfg, 15*time.Second, true, providers.Notify, false)
		if err != nil {
			return err
		}

		return s.Save()
	}

	// guardTick waits between two looks at the network guards.
	guardTick = func(ctx context.Context) error { return sleep(ctx, time.Second) }

	sysRoot  = "/sys"
	active   atomic.Bool
	commitMu sync.Mutex

	seedProvider = sync.OnceValue(func() map[string]string {
		p, err := seed.GetProvider(context.Background())
		if err != nil || p == nil {
			return nil
		}

		return p.Config
	})
)

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// wanted reports whether this machine has a Wi-Fi device or a saved network.
func wanted(s *state.State) bool {
	devices, _ := wifi.Devices(sysRoot)

	return len(devices) > 0 || (s.System.Network.Config != nil && s.System.Network.Config.Wireless.HasNetworks())
}

// Start runs Wi-Fi handling in the background and returns at once. Until the
// machine has a Wi-Fi device or a saved network it only looks at sysfs every 30 s.
func Start(ctx context.Context, s *state.State, t *tui.TUI) {
	go func() {
		for !wanted(s) {
			if sleep(ctx, 30*time.Second) != nil {
				return
			}
		}

		active.Store(true)
		slog.InfoContext(ctx, "Wi-Fi: active")
		t.EnableArcaOverlay()

		monitor(ctx, s, newPicker(ctx, s, t))
	}()
}

// monitor checks every 5 s, shows the picker while offline and closes it once online.
func monitor(ctx context.Context, s *state.State, p *picker) {
	first := true
	wasOnline := false

	for {
		ok, reason := check(ctx, s)
		if first || ok != wasOnline {
			if ok {
				slog.InfoContext(ctx, "Wi-Fi: online")
			} else {
				slog.InfoContext(ctx, "Wi-Fi: offline", "reason", reason)
			}
		}

		first, wasOnline = false, ok

		if ok {
			p.hide()
		} else {
			p.show()
		}

		if sleep(ctx, 5*time.Second) != nil {
			return
		}
	}
}

// check reports whether the machine is online: a default route exists and the
// update server's index answers a HEAD with any status within 5 s.
func check(ctx context.Context, s *state.State) (bool, string) {
	if !defaultRoute() {
		return false, "no default route"
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, serverURL(s)+"/index.sjson", nil)
	if err != nil {
		return false, "the update server URL is not valid"
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, "the update server did not answer"
	}

	_ = resp.Body.Close()

	return true, ""
}

// serverURL is the provider's server_url, else the provider seed's, else the default.
func serverURL(s *state.State) string {
	url := s.System.Provider.Config.Config["server_url"]
	if url == "" {
		url = seedProvider()["server_url"]
	}

	if url == "" {
		url = defaultServerURL
	}

	return strings.TrimRight(url, "/")
}

// defaultRoute reports whether an IPv4 or IPv6 default route is up.
func defaultRoute() bool {
	// Iface Destination Gateway Flags RefCnt Use Metric Mask ...
	if hasRoute("/proc/net/route", func(f []string) bool {
		return len(f) > 7 && f[1] == "00000000" && f[7] == "00000000" && routeUp(f[3])
	}) {
		return true
	}

	// Destination PrefixLen Source PrefixLen NextHop Metric RefCnt Use Flags Iface
	return hasRoute("/proc/net/ipv6_route", func(f []string) bool {
		return len(f) > 9 && f[0] == strings.Repeat("0", 32) && f[1] == "00" && f[9] != "lo" && routeUp(f[8])
	})
}

func hasRoute(path string, isDefault func([]string) bool) bool {
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	for line := range strings.SplitSeq(string(body), "\n") {
		if isDefault(strings.Fields(line)) {
			return true
		}
	}

	return false
}

// routeUp reports RTF_UP without RTF_REJECT.
func routeUp(flags string) bool {
	v, err := strconv.ParseUint(flags, 16, 32)

	return err == nil && v&0x1 != 0 && v&0x200 == 0
}

// WaitOnline holds the first boot when no primary application is installed, Wi-Fi
// is active and the machine is offline, until it is online. Otherwise it returns at once.
func WaitOnline(ctx context.Context, s *state.State) error {
	_, err := applications.GetPrimary(ctx, s, false)
	if err == nil {
		return nil
	}

	if !errors.Is(err, applications.ErrNoPrimary) {
		return err
	}

	if !active.Load() && !wanted(s) {
		return nil
	}

	ok, _ := check(ctx, s)
	if ok {
		return nil
	}

	slog.InfoContext(ctx, "Wi-Fi: gate waiting for a network")

	for !ok {
		err := sleep(ctx, 5*time.Second)
		if err != nil {
			return err
		}

		ok, _ = check(ctx, s)
	}

	slog.InfoContext(ctx, "Wi-Fi: gate released")

	return nil
}

// commit makes network n the first saved network and applies the configuration.
// It first waits while a remote change awaits confirmation or another apply runs,
// never calling into the apply meanwhile. One commit runs at a time.
func commit(ctx context.Context, s *state.State, n api.SystemNetworkWirelessNetwork, status func(string)) error {
	commitMu.Lock()
	defer commitMu.Unlock()

	waiting := false

	for s.NetworkConfigurationPending || s.System.Network.State.ConfigurationInProcess {
		if !waiting {
			status("Waiting for another network change")

			waiting = true
		}

		err := guardTick(ctx)
		if err != nil {
			return err
		}
	}

	next, err := withFirst(s.System.Network.Config, n)
	if err != nil {
		return err
	}

	status("Connecting to " + n.SSID + "...")

	return applyFunc(ctx, s, next)
}

// withFirst returns a deep copy of cfg with n first among the saved networks,
// replacing a saved network with the same SSID and security.
func withFirst(cfg *api.SystemNetworkConfig, n api.SystemNetworkWirelessNetwork) (*api.SystemNetworkConfig, error) {
	body, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}

	next := &api.SystemNetworkConfig{}

	err = json.Unmarshal(body, next)
	if err != nil {
		return nil, err
	}

	if next.Wireless == nil {
		next.Wireless = wifi.Defaults()
	}

	networks := []api.SystemNetworkWirelessNetwork{n}

	for _, saved := range next.Wireless.Networks {
		if saved.SSID != n.SSID || saved.Security != n.Security {
			networks = append(networks, saved)
		}
	}

	next.Wireless.Networks = networks

	return next, nil
}
