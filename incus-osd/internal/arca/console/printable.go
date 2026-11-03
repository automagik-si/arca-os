package console

import (
	"strings"
	"unicode"

	"github.com/rivo/tview"
)

// printable returns s with invalid UTF-8 replaced by U+FFFD and each control
// character by '?', so text from outside (a name, a description, an error) cannot
// move the cursor or change the terminal: the Wi-Fi picker's rule.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}

		return r
	}, strings.ToValidUTF8(s, "�"))
}

// safe returns s ready for tview's print functions: printable, with tag-like text
// escaped so it shows as written. Every external string goes through it.
func safe(s string) string {
	return tview.Escape(printable(s))
}

// width is the number of cells plain text s takes.
func width(s string) int {
	return tview.TaggedStringWidth(tview.Escape(s))
}

// fit returns plain text s cut to n cells, ending in "…" when it was cut.
func fit(s string, n int) string {
	if n <= 0 {
		return ""
	}

	if width(s) <= n {
		return s
	}

	var b strings.Builder

	used := 0

	for _, r := range s {
		w := width(string(r))
		if used+w > n-1 {
			break
		}

		b.WriteRune(r)

		used += w
	}

	return b.String() + "…"
}
