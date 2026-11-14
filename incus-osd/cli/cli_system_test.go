package cli

import (
	"slices"
	"testing"
)

func TestPassphraseGroups(t *testing.T) {
	t.Parallel()

	got := passphraseGroups("abandon ability able about above absent absorb abstract")
	if !slices.Equal(got, []string{"abandon ability able about", "above absent absorb abstract"}) {
		t.Fatalf("groups: %q", got)
	}

	for _, key := range []string{"cbdefghi-jklnrtuv-cbdefghi-jklnrtuv", "a b c d e f g", "abandon ability able about above absent absorb Abstract", "abandon  ability able about above absent absorb"} {
		if passphraseGroups(key) != nil {
			t.Fatalf("%q was grouped as a passphrase", key)
		}
	}
}
