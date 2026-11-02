package wifi

import (
	"slices"
	"strings"

	"github.com/lxc/incus-os/incus-osd/api"
)

// Entry is one network of the picker list, represented by its best BSS.
type Entry struct {
	SSID           string
	Security       string // wpa-personal, open, enterprise, wep
	SAE, PSK       bool
	Generation     int
	Band           string // "2.4", "5", "6"
	Width          int
	Signal         int
	BSSID          string
	NotRecommended bool
	Selectable     bool
}

// Rank returns one entry per non-empty SSID, represented by its best BSS, fastest
// first. A BSS is faster when its generation is higher, then its band (6 GHz, 5 GHz,
// 2.4 GHz), then its width, then its signal (dBm, higher is stronger). Two networks
// that tie are ordered by SSID. A network whose best BSS is on 2.4 GHz is marked
// NotRecommended; an enterprise or WEP network is listed but not Selectable.
func Rank(bss []BSS) []Entry {
	best := map[string]Entry{}

	for _, b := range bss {
		if b.SSID == "" {
			continue
		}

		e := entryFor(b)

		cur, ok := best[b.SSID]
		if !ok || faster(e, cur) {
			best[b.SSID] = e
		}
	}

	entries := make([]Entry, 0, len(best))
	for _, e := range best {
		entries = append(entries, e)
	}

	slices.SortFunc(entries, func(a Entry, b Entry) int {
		switch {
		case faster(a, b):
			return -1
		case faster(b, a):
			return 1
		}

		return strings.Compare(a.SSID, b.SSID)
	})

	return entries
}

// faster reports whether a is ranked above b by (generation, band, width, signal).
func faster(a Entry, b Entry) bool {
	ka := []int{a.Generation, bandRank(a.Band), a.Width, a.Signal}
	kb := []int{b.Generation, bandRank(b.Band), b.Width, b.Signal}

	return slices.Compare(ka, kb) > 0
}

// bandRank orders the bands, 6 GHz first; an unknown band ranks last.
func bandRank(band string) int {
	switch band {
	case "6":
		return 3
	case "5":
		return 2
	case "2.4":
		return 1
	}

	return 0
}

func entryFor(b BSS) Entry {
	caps := ParseIEs(b.IEs)
	e := Entry{
		SSID:       b.SSID,
		Generation: caps.Generation,
		Band:       Band(b.Freq),
		Width:      caps.Width,
		Signal:     b.Level,
		BSSID:      b.BSSID,
	}

	switch {
	case strings.Contains(b.Flags, "EAP"):
		e.Security = "enterprise"
	case strings.Contains(b.Flags, "[WEP]"):
		e.Security = "wep"
	case strings.Contains(b.Flags, "PSK") || strings.Contains(b.Flags, "SAE"):
		e.Security = api.SystemNetworkWirelessSecurityWPAPersonal
		e.SAE = strings.Contains(b.Flags, "SAE")
		e.PSK = strings.Contains(b.Flags, "PSK")
	default:
		e.Security = api.SystemNetworkWirelessSecurityOpen
	}

	e.Selectable = e.Security == api.SystemNetworkWirelessSecurityWPAPersonal || e.Security == api.SystemNetworkWirelessSecurityOpen
	e.NotRecommended = e.Band == "2.4"

	return e
}
