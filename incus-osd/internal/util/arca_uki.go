package util

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"sync"

	"github.com/lxc/incus/v7/shared/subprocess"
)

// UKIDir holds the UKI images systemd-boot starts.
var UKIDir = "/boot/EFI/Linux"

// ukiCounterRegex matches systemd-boot's boot counter ("+<left>" or "+<left>-<done>") before ".efi".
var ukiCounterRegex = regexp.MustCompile(`\+\d+(-\d+)?\.efi`)

// ukiFileRegex splits a UKI file name into its stem, version and boot counter.
var ukiFileRegex = regexp.MustCompile(`^(.+)_(\d{12})(?:\+(\d+)(?:-(\d+))?)?\.efi$`)

// entryVersionRegex finds the version in a boot entry ID such as "IncusOS_202611091832.efi@main".
var entryVersionRegex = regexp.MustCompile(`_(\d{12})\.efi`)

// failedBoots are the OS versions no next boot may pick, unless no other one is left.
var failedBoots struct {
	sync.Mutex

	versions []string
}

// SetFailedBootVersions sets the OS versions that every next-boot choice skips.
func SetFailedBootVersions(versions []string) {
	failedBoots.Lock()
	defer failedBoots.Unlock()

	failedBoots.versions = slices.Clone(versions)
}

func failedBootVersions() []string {
	failedBoots.Lock()
	defer failedBoots.Unlock()

	return slices.Clone(failedBoots.versions)
}

// bootEntryID returns a boot entry ID without systemd-boot's boot counter.
func bootEntryID(id string) string {
	return ukiCounterRegex.ReplaceAllString(id, ".efi")
}

func entryVersion(id string) string {
	m := entryVersionRegex.FindStringSubmatch(id)
	if m == nil {
		return ""
	}

	return m[1]
}

// nextBootID picks the next boot entry. With a version, only that version is picked; otherwise a
// failed version is skipped, unless no other version is left.
func nextBootID(ctx context.Context, bootID BootID, version string) (string, error) {
	skip := failedBootVersions()

	target, err := getNextBootID(ctx, bootID, func(id string) bool {
		if version != "" {
			return entryVersion(id) == version
		}

		return !slices.Contains(skip, entryVersion(id))
	})
	if err != nil && version == "" && len(skip) > 0 {
		target, err = getNextBootID(ctx, bootID, func(string) bool { return true })
	}

	if err != nil {
		return "", err
	}

	return listedBootID(ctx, target)
}

// listedBootID returns the entry ID as bootctl lists it, which may still carry a boot counter.
func listedBootID(ctx context.Context, target string) (string, error) {
	output, err := subprocess.RunCommandContext(ctx, "bootctl", "list", "--json=short")
	if err != nil {
		return "", err
	}

	entries := []struct {
		ID string `json:"id"`
	}{}

	err = json.Unmarshal([]byte(output), &entries)
	if err != nil {
		return "", err
	}

	for _, entry := range entries {
		if bootEntryID(entry.ID) == target {
			return entry.ID, nil
		}
	}

	return target, nil
}

// SetNextBootVersion makes the next boot start version, in the current UKI profile.
func SetNextBootVersion(ctx context.Context, version string) error {
	id, err := nextBootID(ctx, BootIDNewest, version)
	if err != nil {
		return err
	}

	_, err = subprocess.RunCommandContext(ctx, "bootctl", "set-oneshot", id)

	return err
}

// CurrentUKIPath returns the running UKI's file, whose name may carry a boot counter.
func CurrentUKIPath(osName string, release string) string {
	v, err := GetUKIVersions()
	if err == nil && v.CurrentFilepath != "" {
		return v.CurrentFilepath
	}

	return fmt.Sprintf("/boot/EFI/Linux/%s_%s.efi", osName, release)
}

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
