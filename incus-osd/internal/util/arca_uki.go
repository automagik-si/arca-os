package util

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

// UKIDir holds the UKI images systemd-boot starts.
var UKIDir = "/boot/EFI/Linux"

// ukiCounterRegex matches systemd-boot's boot counter ("+<left>" or "+<left>-<done>") before ".efi".
var ukiCounterRegex = regexp.MustCompile(`\+\d+(-\d+)?\.efi`)

// ukiFileRegex splits a UKI file name into its stem, version and boot counter.
var ukiFileRegex = regexp.MustCompile(`^(.+)_(\d{12})(?:\+(\d+)(?:-(\d+))?)?\.efi$`)

// UKIFileVersion returns the version in a UKI file name, with or without a boot counter.
func UKIFileVersion(name string) string {
	m := ukiFileRegex.FindStringSubmatch(name)
	if m == nil {
		return ""
	}

	return m[2]
}

// MarkUKIGood blesses version's UKI the way systemd-bless-boot does: its boot counter is removed.
func MarkUKIGood(version string) error {
	return renameUKI(version, func(_ int, _ int) string { return "" })
}

// MarkUKIBad leaves version's UKI with no tries left, so systemd-boot sorts it last.
func MarkUKIBad(version string) error {
	return renameUKI(version, func(_ int, done int) string { return "+0-" + strconv.Itoa(max(done, 1)) })
}

// ResetUKITries gives version's UKI three fresh tries, for an owner who tries it again.
func ResetUKITries(version string) error {
	return renameUKI(version, func(_ int, _ int) string { return "+3-0" })
}

func renameUKI(version string, counter func(left int, done int) string) error {
	entries, err := os.ReadDir(UKIDir)
	if err != nil {
		return err
	}

	found := false

	for _, entry := range entries {
		m := ukiFileRegex.FindStringSubmatch(entry.Name())
		if m == nil || m[2] != version {
			continue
		}

		found = true
		left, _ := strconv.Atoi(m[3])
		done, _ := strconv.Atoi(m[4])
		name := fmt.Sprintf("%s_%s%s.efi", m[1], m[2], counter(left, done))

		if name == entry.Name() {
			continue
		}

		err := os.Rename(filepath.Join(UKIDir, entry.Name()), filepath.Join(UKIDir, name))
		if err != nil {
			return err
		}
	}

	if !found {
		return errors.New("no boot entry found for version " + version)
	}

	return nil
}
