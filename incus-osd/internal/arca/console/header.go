package console

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/lxc/incus-os/incus-osd/internal/applications"
	"github.com/lxc/incus-os/incus-osd/internal/arca/wifi"
	"github.com/lxc/incus-os/incus-osd/internal/network"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// brand is drawn before the node name; release follows the OS name.
const (
	brand  = " ARCA "
	osName = "Arca OS"
)

var (
	// hostname is the node name, as upstream's header shows it.
	hostname = os.Hostname

	// ipAddresses lists an interface's addresses, link-local ones left out.
	ipAddresses = network.GetIPAddresses

	// procRoot and sysRoot are where /proc and /sys are read.
	procRoot = "/proc"
	sysRoot  = "/sys"
)

// readNode returns the node name, or "" when the hostname cannot be read.
func readNode() string {
	name, err := hostname()
	if err != nil {
		return ""
	}

	return name
}

// readApps returns the installed applications, sorted by name, as upstream's footer lists them.
func readApps(ctx context.Context, s *state.State) ([]App, error) {
	installed, err := applications.GetInstalled(ctx, s)
	if err != nil {
		return nil, err
	}

	apps := make([]App, 0, len(installed))
	for _, a := range installed {
		apps = append(apps, App{Name: a.Name(), Version: a.FriendlyVersion()})
	}

	slices.SortFunc(apps, func(a, b App) int { return strings.Compare(a.Name, b.Name) })

	return apps, nil
}

// interfaces returns the devices whose addresses the header shows: each configured
// interface, bond, VLAN and WireGuard device with addresses that exists, as
// upstream's footer lists them, then each Wi-Fi device.
func interfaces(s *state.State) []string {
	names := []string{}

	add := func(name string, addrs []string) {
		if len(addrs) == 0 || slices.Contains(names, name) {
			return
		}

		_, err := os.Stat(filepath.Join(sysRoot, "class", "net", name))
		if err == nil {
			names = append(names, name)
		}
	}

	if cfg := s.System.Network.Config; cfg != nil {
		for _, i := range cfg.Interfaces {
			add(i.Name, i.Addresses)
		}

		for _, b := range cfg.Bonds {
			add(b.Name, b.Addresses)
		}

		for _, v := range cfg.VLANs {
			add(v.Name, v.Addresses)
		}

		for _, w := range cfg.Wireguard {
			add(w.Name, w.Addresses)
		}
	}

	devices, _ := wifi.Devices(sysRoot)
	for _, d := range devices {
		if !slices.Contains(names, d) {
			names = append(names, d)
		}
	}

	return names
}

// readIPs returns the node's IPv4 addresses, then its first global IPv6 address.
// It returns an empty list, not nil, when there is none.
func readIPs(ctx context.Context, s *state.State) []string {
	v4 := []string{}
	v6 := ""

	for _, name := range interfaces(s) {
		addrs, err := ipAddresses(ctx, name)
		if err != nil {
			continue
		}

		for _, a := range addrs {
			ip, err := netip.ParseAddr(a)
			if err != nil {
				continue
			}

			if ip.Is4() && !ip.IsLoopback() && !slices.Contains(v4, a) {
				v4 = append(v4, a)
			} else if ip.Is6() && v6 == "" && ip.IsGlobalUnicast() {
				v6 = a
			}
		}
	}

	if v6 != "" {
		return append(v4, v6)
	}

	return v4
}

// readOnline returns the result of the Wi-Fi monitor's connectivity check when it
// runs on this node; otherwise only whether a default route exists. It sends nothing.
func readOnline(s *state.State) Online {
	if c := s.System.Network.State.Connectivity; c != nil {
		return Online{Known: true, Up: c.Online, Reason: c.Reason}
	}

	if defaultRoute() {
		return Online{Known: true, Up: true, RouteOnly: true}
	}

	return Online{Known: true, RouteOnly: true, Reason: "no default route"}
}

// defaultRoute reports whether an IPv4 or IPv6 default route is up, as the Wi-Fi
// monitor reads it.
func defaultRoute() bool {
	// Iface Destination Gateway Flags RefCnt Use Metric Mask ...
	if hasRoute(filepath.Join(procRoot, "net", "route"), func(f []string) bool {
		return len(f) > 7 && f[1] == "00000000" && f[7] == "00000000" && routeUp(f[3])
	}) {
		return true
	}

	// Destination PrefixLen Source PrefixLen NextHop Metric RefCnt Use Flags Iface
	return hasRoute(filepath.Join(procRoot, "net", "ipv6_route"), func(f []string) bool {
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

// onlineText is the online field: "—" until read, then the state.
func onlineText(o Online) string {
	switch {
	case !o.Known:
		return tagDim + "—" + tagReset
	case o.RouteOnly && o.Up:
		return tagDim + "● route" + tagReset
	case o.RouteOnly:
		return tagBad + "● no route" + tagReset
	case o.Up:
		return tagOK + "● online" + tagReset
	default:
		return tagBad + "● offline" + tagReset
	}
}

// drawHeader draws the header's two rows at (x, y), w wide: the brand, node, release
// and applications, with the online state and the clock on the right; then the
// node's addresses. Only the rows that fit in h are drawn.
func drawHeader(screen tcell.Screen, s Snapshot, x, y, w, h int) {
	if h < 1 || w < 1 {
		return
	}

	clock := "--:--"
	if !s.Now.IsZero() {
		clock = s.Now.Format("15:04")
	}

	right := onlineText(s.Online) + "  " + clock + " "
	room := w - tview.TaggedStringWidth(right) - 1

	node := printable(s.Node)
	if node == "" {
		node = "—"
	}

	release := printable(s.OSRelease)
	if release == "" {
		release = "—"
	}

	fixed := width(brand) + 1 + 2 + width(osName) + 1 + width(release)
	node = fit(node, max(room-fixed, 1))

	var left strings.Builder

	left.WriteString(tagBrand + brand + tagReset + " " + tagBold + tview.Escape(node) + tagReset +
		"  " + osName + " " + tview.Escape(release))
	used := fixed + width(node)

	for _, a := range s.Apps {
		item := printable(a.Name) + " " + printable(a.Version)
		if used+2+width(item) > room {
			break
		}

		left.WriteString("  " + tagDim + tview.Escape(item) + tagReset)
		used += 2 + width(item)
	}

	tview.Print(screen, left.String(), x, y, w, tview.AlignLeft, tcell.ColorWhite)
	tview.Print(screen, right, x, y, w, tview.AlignRight, tcell.ColorWhite)

	if h < 2 {
		return
	}

	var line string

	switch {
	case s.IPs == nil:
		line = tagDim + "—" + tagReset
	case len(s.IPs) == 0:
		line = tagBad + "no address" + tagReset
	default:
		var b strings.Builder

		used := 0

		for i, ip := range s.IPs {
			item := printable(ip)

			gap := 0
			if i > 0 {
				gap = 2
			}

			if used+gap+width(item) > w-2 {
				break
			}

			if i > 0 {
				b.WriteString("  ")
			}

			b.WriteString(tagBold + tview.Escape(item) + tagReset)
			used += gap + width(item)
		}

		line = b.String()
	}

	tview.Print(screen, " "+line, x, y+1, w, tview.AlignLeft, tcell.ColorWhite)
}
