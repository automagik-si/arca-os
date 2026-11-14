package passphrase

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strings"
	"testing"
)

// seeded returns a reader of deterministic bytes.
func seeded(seed byte) *bytes.Reader {
	sum := sha256.Sum256([]byte{seed})

	return bytes.NewReader(sum[:])
}

func TestWordlist(t *testing.T) {
	t.Parallel()

	sum := sha256.Sum256([]byte(english))
	if hex.EncodeToString(sum[:]) != "2f5eed53a4727b4bf8880d8f3f199efc90e58503646d9ff8eff3a2ed3b24dbda" {
		t.Fatal("english.txt is not the BIP39 English list")
	}

	if len(wordlist) != 2048 || len(index) != 2048 {
		t.Fatalf("got %d words, %d unique, want 2048", len(wordlist), len(index))
	}

	if Bits != 88 || math.Log2(float64(len(wordlist)))*Words != Bits {
		t.Fatalf("a passphrase carries %d bits, want 88", Bits)
	}

	for _, w := range wordlist {
		if strings.Trim(w, "abcdefghijklmnopqrstuvwxyz") != "" {
			t.Fatalf("word %q is not lowercase ASCII", w)
		}
	}
}

func TestNew(t *testing.T) {
	t.Parallel()

	p, err := Generate()
	if err != nil {
		t.Fatal(err)
	}

	if !Valid(p) || len(strings.Split(p, " ")) != 8 || strings.Contains(p, "  ") {
		t.Fatalf("passphrase %q is not 8 list words separated by single spaces", p)
	}

	// The same reader gives the same passphrase, another reader another one.
	a, _ := New(seeded(1))
	b, _ := New(seeded(1))
	c, _ := New(seeded(2))

	if a != b || a == c || !Valid(a) || !Valid(c) {
		t.Fatalf("seeded passphrases: %q, %q, %q", a, b, c)
	}

	// Each 11-bit group indexes the list; words are drawn with repetition.
	zero, _ := New(bytes.NewReader(make([]byte, 11)))
	if zero != strings.TrimSpace(strings.Repeat("abandon ", 8)) {
		t.Fatalf("all-zero passphrase is %q", zero)
	}

	ones, _ := New(bytes.NewReader(bytes.Repeat([]byte{0xff}, 11)))
	if ones != strings.TrimSpace(strings.Repeat("zoo ", 8)) {
		t.Fatalf("all-one passphrase is %q", ones)
	}

	// Index 1 then index 2047: 00000000001 11111111111 ...
	mixed, _ := New(bytes.NewReader([]byte{0x00, 0x3f, 0xfc, 0, 0, 0, 0, 0, 0, 0, 0}))
	if Word(mixed, 1) != "ability" || Word(mixed, 2) != "zoo" || Word(mixed, 3) != "abandon" {
		t.Fatalf("bit order: %q", mixed)
	}

	_, err = New(bytes.NewReader(make([]byte, 10)))
	if err == nil {
		t.Fatal("a short reader gave a passphrase")
	}
}

func TestHelpers(t *testing.T) {
	t.Parallel()

	p := "abandon ability able about above absent absorb abstract"

	if !Valid(p) || Valid(p+" zoo") || Valid("abandon  ability able about above absent absorb abstract") || Valid("Abandon ability able about above absent absorb abstract") {
		t.Fatal("Valid accepts or rejects the wrong phrases")
	}

	if Word(p, 2) != "ability" || Word(p, 5) != "above" || Word(p, 9) != "" || Word(p, 0) != "" {
		t.Fatal("Word counts from 1")
	}

	g := Groups(p, 4)
	if len(g) != 2 || g[0] != "abandon ability able about" || g[1] != "above absent absorb abstract" {
		t.Fatalf("groups: %q", g)
	}
}
