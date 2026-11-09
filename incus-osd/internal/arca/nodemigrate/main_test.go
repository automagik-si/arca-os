package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestParseArgs(t *testing.T) {
	t.Parallel()

	opts, err := parseArgs([]string{"--check", "--version", "202611031550", "--source", "/src"})
	if err != nil || !opts.check || opts.version != "202611031550" || opts.source != "/src" {
		t.Fatalf("got %+v, %v", opts, err)
	}

	for _, args := range [][]string{
		{"--version", "2026", "--source", "/src"},
		{"--version", "202611031550"},
		{"--version"},
		{"--version", "202611031550", "--source", "/src", "--force"},
	} {
		_, err := parseArgs(args)
		if err == nil {
			t.Errorf("%v: expected an error", args)
		}
	}
}

func TestIsUKIFile(t *testing.T) {
	t.Parallel()

	uki := "IncusOS_202611031550.efi"

	for name, want := range map[string]bool{
		"IncusOS_202611031550.efi":     true,
		"IncusOS_202611031550+3.efi":   true,
		"IncusOS_202611031550+3-0.efi": true,
		"IncusOS_202611031550+2-1.efi": true,
		"IncusOS_202611022009.efi":     false,
		"IncusOS_2026110315501.efi":    false,
		"IncusOS_202611031550.efi.bak": false,
	} {
		if isUKIFile(name, uki) != want {
			t.Errorf("isUKIFile(%q) = %v, want %v", name, !want, want)
		}
	}
}

func TestOneShotValue(t *testing.T) {
	t.Parallel()

	got := oneShotValue("A.efi")
	want := []byte{0x07, 0, 0, 0, 'A', 0, '.', 0, 'e', 0, 'f', 0, 'i', 0, 0, 0}

	if !bytes.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "BOOTX64.EFI")

	err := os.WriteFile(path, []byte("old"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	err = writeFileAtomic(path, []byte("new"), 0o444)
	if err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Fatalf("got %q", got)
	}

	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o444 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("left %d files behind", len(entries))
	}

	// A failed replace leaves the old content and no temporary file.
	err = writeFileAtomic(filepath.Join(dir, "missing", "x"), []byte("new"), 0o644)
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestRestoreBootLoadersRunsEveryFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	a := filepath.Join(dir, "a.efi")
	b := filepath.Join(dir, "missing", "b.efi")
	c := filepath.Join(dir, "c.efi")

	for _, p := range []string{a, c} {
		err := os.WriteFile(p, []byte("new"), 0o644)
		if err != nil {
			t.Fatal(err)
		}
	}

	err := restoreBootLoaders(map[string][]byte{a: []byte("old-a"), b: []byte("old-b"), c: []byte("old-c")})
	if err == nil {
		t.Fatal("expected the unwritable file to fail")
	}

	for p, want := range map[string]string{a: "old-a", c: "old-c"} {
		got, _ := os.ReadFile(p)
		if string(got) != want {
			t.Errorf("%s: got %q, want %q", p, got, want)
		}
	}
}

func TestFindOSFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	v := "202611031550"
	names := []string{
		"IncusOS_" + v + ".efi",
		"IncusOS_" + v + ".usr-x86-64.aa.raw",
		"IncusOS_" + v + ".usr-x86-64-verity.bb.raw",
		"IncusOS_" + v + ".usr-x86-64-verity-sig.cc.raw",
		"incus.raw",
		"IncusOS_202611022009.efi",
	}

	for _, n := range names {
		err := os.WriteFile(filepath.Join(dir, n), nil, 0o644)
		if err != nil {
			t.Fatal(err)
		}
	}

	found, err := findOSFiles(v, dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(found) != 4 || filepath.Base(found["usr-verity-sig"]) != names[3] || filepath.Base(found["efi"]) != names[0] {
		t.Fatalf("got %v", found)
	}

	err = os.Remove(filepath.Join(dir, names[2]))
	if err != nil {
		t.Fatal(err)
	}

	_, err = findOSFiles(v, dir)
	if err == nil {
		t.Fatal("expected a missing verity image to fail")
	}
}

func TestExistingAncestor(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	got := existingAncestor(filepath.Join(dir, "a", "b"))
	if got != dir {
		t.Fatalf("got %s, want %s", got, dir)
	}
}
