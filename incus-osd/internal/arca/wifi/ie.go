package wifi

// Information element IDs, and the element ID extensions, that the ranking reads.
const (
	ieHTCapabilities  = 45
	ieHTOperation     = 61
	ieVHTCapabilities = 191
	ieVHTOperation    = 192
	ieExtension       = 255

	ieExtHECapabilities  = 35
	ieExtHEOperation     = 36
	ieExtEHTOperation    = 106
	ieExtEHTCapabilities = 108
)

// Caps is what a BSS's information elements say about its Wi-Fi generation and
// channel width.
type Caps struct {
	Generation int // 7 (EHT), 6 (HE), 5 (VHT), 4 (HT), or 0 for an older network
	Width      int // MHz; 20 when no element says otherwise
}

// Band returns the band of a frequency in MHz: "2.4", "5", "6", or "" outside them.
func Band(freq int) string {
	switch {
	case freq >= 2412 && freq <= 2484:
		return "2.4"
	case freq >= 5160 && freq <= 5885:
		return "5"
	case freq >= 5955 && freq <= 7115:
		return "6"
	}

	return ""
}

// ParseIEs reads the information elements of a BSS, each "id, length, body", and
// returns its generation and channel width. The width comes from the first of
// EHT Operation, the HE Operation's 6 GHz information, VHT Operation and HT
// Operation that says one. A truncated element ends the walk; no input panics.
func ParseIEs(ies []byte) Caps {
	caps := Caps{Width: 20}

	var ehtW, heW, vhtW, htW int

	for len(ies) >= 2 {
		id, n := int(ies[0]), int(ies[1])
		if len(ies) < 2+n {
			break
		}

		body := ies[2 : 2+n]
		ies = ies[2+n:]

		switch id {
		case ieHTCapabilities:
			caps.Generation = max(caps.Generation, 4)
		case ieVHTCapabilities:
			caps.Generation = max(caps.Generation, 5)
		case ieVHTOperation:
			vhtW = vhtWidth(body)
		case ieHTOperation:
			htW = htWidth(body)
		case ieExtension:
			if len(body) < 1 {
				continue
			}

			switch body[0] {
			case ieExtHECapabilities:
				caps.Generation = max(caps.Generation, 6)
			case ieExtEHTCapabilities:
				caps.Generation = max(caps.Generation, 7)
			case ieExtHEOperation:
				heW = heWidth(body)
			case ieExtEHTOperation:
				ehtW = ehtWidth(body)
			default:
				// An extension element this parser does not use.
			}
		default:
			// An element this parser does not use.
		}
	}

	switch {
	case ehtW > 0:
		caps.Width = ehtW
	case heW > 0:
		caps.Width = heW
	case vhtW > 0:
		caps.Width = vhtW
	case htW > 0:
		caps.Width = htW
	default:
		// No operation element states a width; it stays 20 MHz.
	}

	return caps
}

// ehtWidth reads the channel width of an EHT Operation element body (its extension
// byte, parameters, a four-byte MCS set, then the information's control byte when
// the parameters say it is present). It returns 0 when the element does not say.
func ehtWidth(body []byte) int {
	const controlAt = 6 // extension, parameters, four bytes of MCS set

	if len(body) <= controlAt || body[1]&0x01 == 0 {
		return 0
	}

	widths := [...]int{20, 40, 80, 160, 320}
	if c := int(body[controlAt] & 0x07); c < len(widths) {
		return widths[c]
	}

	return 0
}

// heWidth reads the 6 GHz channel width of an HE Operation element body: its
// extension byte, three bytes of parameters, the BSS colour and the basic MCS set
// (seven bytes in all), then the optional VHT information, the co-hosted BSSID
// count and the 6 GHz information. That information is five bytes, the primary
// channel, a control byte whose low two bits are the width, and three more. It
// returns 0 for a BSS that is not on 6 GHz.
func heWidth(body []byte) int {
	const start = 7

	if len(body) < start {
		return 0
	}

	params := int(body[1]) | int(body[2])<<8 | int(body[3])<<16

	at := start
	if params&0x004000 != 0 { // VHT Operation Information Present
		at += 3
	}

	if params&0x008000 != 0 { // Co-Hosted BSS
		at++
	}

	if params&0x020000 == 0 || len(body) < at+5 { // 6 GHz Operation Information Present
		return 0
	}

	return 20 << (body[at+1] & 0x03)
}

// vhtWidth reads the channel width of a VHT Operation element body. A value of 0
// (20 or 40 MHz) is left to the HT Operation element, so it returns 0.
func vhtWidth(body []byte) int {
	if len(body) < 1 {
		return 0
	}

	switch body[0] {
	case 1:
		return 80
	case 2, 3:
		return 160
	}

	return 0
}

// htWidth reads the channel width of an HT Operation element body: 40 MHz when the
// STA Channel Width bit is set, else 0 (20 MHz, which is the default).
func htWidth(body []byte) int {
	if len(body) >= 2 && body[1]&0x04 != 0 {
		return 40
	}

	return 0
}
