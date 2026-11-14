// Package passphrase generates the recovery passphrase: eight words from the
// BIP39 English list, which an owner can read and type at a console.
package passphrase

import (
	"crypto/rand"
	_ "embed"
	"errors"
	"io"
	"strings"
)

// Words is the number of words in a passphrase, BitsPerWord what each carries.
const (
	Words       = 8
	BitsPerWord = 11
	Bits        = Words * BitsPerWord // 88 bits.
)

// english is the BIP39 English list: 2048 lowercase words, no two sharing their
// first four letters. Source: github.com/trezor/python-mnemonic,
// src/mnemonic/wordlist/english.txt, MIT License, Copyright (c) 2013-2016 Pavol
// Rusnak. SHA-256 2f5eed53a4727b4bf8880d8f3f199efc90e58503646d9ff8eff3a2ed3b24dbda.
//
//go:embed english.txt
var english string

// wordlist is english, one word per entry, in its order.
var wordlist = strings.Fields(english)

// index maps each word to its position in wordlist.
var index = func() map[string]int {
	m := make(map[string]int, len(wordlist))
	for i, w := range wordlist {
		m[w] = i
	}

	return m
}()

// Generate returns a new passphrase drawn from the system's random source.
func Generate() (string, error) {
	return New(rand.Reader)
}

// New returns a passphrase drawn from r: 11 bytes, read as eight 11-bit indexes
// into the list. Words are drawn independently and may repeat.
func New(r io.Reader) (string, error) {
	if len(wordlist) != 1<<BitsPerWord {
		return "", errors.New("passphrase word list is damaged")
	}

	buf := make([]byte, Bits/8)

	_, err := io.ReadFull(r, buf)
	if err != nil {
		return "", err
	}

	words := make([]string, 0, Words)

	var acc, n uint32

	for _, b := range buf {
		acc = acc<<8 | uint32(b)
		n += 8

		for n >= BitsPerWord {
			n -= BitsPerWord
			words = append(words, wordlist[(acc>>n)&(1<<BitsPerWord-1)])
		}
	}

	return strings.Join(words, " "), nil
}

// Valid reports whether s is a passphrase: eight words of the list, each
// separated by a single space.
func Valid(s string) bool {
	words := strings.Split(s, " ")
	if len(words) != Words {
		return false
	}

	for _, w := range words {
		_, ok := index[w]
		if !ok {
			return false
		}
	}

	return true
}

// Word returns the n-th word of s, counting from 1, or "" when there is none.
func Word(s string, n int) string {
	words := strings.Fields(s)
	if n < 1 || n > len(words) {
		return ""
	}

	return words[n-1]
}

// Groups splits s into lines of size words each, for reading aloud or copying.
func Groups(s string, size int) []string {
	words := strings.Fields(s)
	lines := []string{}

	for size > 0 && len(words) > 0 {
		n := min(size, len(words))
		lines = append(lines, strings.Join(words[:n], " "))
		words = words[n:]
	}

	return lines
}
