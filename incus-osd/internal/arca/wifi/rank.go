package wifi

import (
	"slices"
	"strings"

	"github.com/lxc/incus-os/incus-osd/api"
)

// Entry is one network of the picker list, represented by its best BSS.
type Entry struct {
	SSID           string
	Security       string // wpa-personal, open, enterprise, wep, owe, unsupported
	SAE, PSK       bool
	Generation     int
	Band           string // "2.4", "5", "6"
	Width          int
	Signal         int
	BSSID          string
	NotRecommended bool
	Selectable     bool

	rsn bool // the personal key management is offered under WPA2 (RSN), not WPA alone
}

// Rank returns one entry per non-empty SSID, represented by its best BSS, fastest
// first. Within one SSID only the BSSes of its strongest security class count
// (WPA3-SAE, then WPA2, then other WPA-Personal, then a secured network that cannot
// be joined, then open), so an open BSS never stands for a network that also has a
// secured one. Among those, a BSS is faster when its generation is higher, then its
// band (6 GHz, 5 GHz, 2.4 GHz), then its width, then its signal (dBm, higher is
// stronger). Two networks that tie are ordered by SSID. A network whose best BSS is
// on 2.4 GHz is marked NotRecommended; an enterprise, WEP, OWE or other network
// whose key management the supplicant configuration cannot express is listed but
// not Selectable.
func Rank(bss []BSS) []Entry {
	best := map[string]Entry{}

	for _, b := range bss {
		if b.SSID == "" {
			continue
		}

		e := entryFor(b)

		cur, ok := best[b.SSID]
		if !ok || secClass(e) > secClass(cur) || (secClass(e) == secClass(cur) && faster(e, cur)) {
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

	e.Security, e.SAE, e.PSK, e.rsn = security(b.Flags)

	e.Selectable = e.Security == api.SystemNetworkWirelessSecurityWPAPersonal || e.Security == api.SystemNetworkWirelessSecurityOpen
	e.NotRecommended = e.Band == "2.4"

	return e
}

// secClass orders the security of the BSSes of one SSID: WPA3-SAE, WPA2-PSK, other
// WPA-Personal, a secured network that cannot be joined, then open.
func secClass(e Entry) int {
	switch {
	case e.Security == api.SystemNetworkWirelessSecurityWPAPersonal && e.SAE:
		return 5
	case e.Security == api.SystemNetworkWirelessSecurityWPAPersonal && e.rsn:
		return 4
	case e.Security == api.SystemNetworkWirelessSecurityWPAPersonal:
		return 3
	case e.Security == api.SystemNetworkWirelessSecurityOpen:
		return 1
	}

	return 2
}

// akms are the key management names that wpa_supplicant writes in a BSS's flags,
// as "[<proto>-<akm>+<akm>...-<ciphers>]". Longer names come before their prefixes.
var akms = []string{
	"EAP-SUITE-B-192", "EAP-SUITE-B", "EAP-SHA384", "EAP-SHA256", "EAP",
	"FT/EAP-SHA384", "FT/EAP", "FT/PSK", "FT/SAE-EXT-KEY", "FT/SAE",
	"FT-FILS-SHA384", "FT-FILS-SHA256", "FILS-SHA384", "FILS-SHA256",
	"PSK-SHA256", "PSK", "SAE-EXT-KEY", "SAE", "OWE", "DPP", "OSEN", "None", "?",
}

// security classifies a BSS from its flags. Any EAP makes it enterprise, and WEP
// makes it wep. It is wpa-personal only when it offers a key management that
// RenderSupplicant writes (SAE, WPA-PSK or WPA-PSK-SHA256); OWE, and any other key
// management alone, make it owe or unsupported; no WPA element and no WEP make it open. rsn is set when
// the personal key management is offered under WPA2.
func security(flags string) (string, bool, bool, bool) {
	var eap, wep, owe, other, sae, psk, rsn bool

	for group := range strings.SplitSeq(flags, "[") {
		group = strings.TrimSuffix(group, "]")
		if group == "WEP" {
			wep = true

			continue
		}

		proto, rest, ok := strings.Cut(group, "-")
		if !ok || (proto != "WPA" && proto != "WPA2" && proto != "RSN" && proto != "OSEN") {
			continue
		}

		for rest != "" {
			name := ""

			for _, a := range akms {
				if strings.HasPrefix(rest, a) && (len(rest) == len(a) || rest[len(a)] == '+' || rest[len(a)] == '-') {
					name = a

					break
				}
			}

			if name == "" {
				name, _, _ = strings.Cut(rest, "+")
				name, _, _ = strings.Cut(name, "-")
			}

			switch {
			case strings.Contains(name, "EAP"):
				eap = true
			case name == "SAE":
				sae = true
				rsn = rsn || proto != "WPA"
			case name == "PSK" || name == "PSK-SHA256":
				psk = true
				rsn = rsn || proto != "WPA"
			case name == "OWE":
				owe = true
			default:
				other = true
			}

			rest = rest[len(name):]
			if !strings.HasPrefix(rest, "+") {
				break // the ciphers follow
			}

			rest = rest[1:]
		}
	}

	switch {
	case eap:
		return "enterprise", false, false, false
	case wep:
		return "wep", false, false, false
	case sae || psk:
		return api.SystemNetworkWirelessSecurityWPAPersonal, sae, psk, rsn
	case owe:
		return "owe", false, false, false
	case other:
		return "unsupported", false, false, false
	}

	return api.SystemNetworkWirelessSecurityOpen, false, false, false
}
