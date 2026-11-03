// Command arca-node-migrate installs a locally-staged, operator-trusted OS
// build onto the running node and re-seals the TPM for the new UKI, the same
// way incus-osd's own update apply does, but without the download and without
// the signature/trust checks (the operator built and staged the files).
//
// It exists for one job: moving a node whose embedded update CA and Secure
// Boot signing certificate no longer exist anywhere onto a build signed with
// the persistent CA. Such a node cannot verify, and so cannot apply, any
// signed update through the normal path; this writes the new build directly.
// It is a one-shot: after it runs and the node reboots, the node runs a build
// with the persistent CA and updates itself normally.
//
// It must run as root in the node's own context (its /dev, /sys, /boot,
// /var/lib/incus-os and the node's systemd-cryptenroll, systemd-sysupdate and
// veritysetup), which the migration gives it through a privileged container
// with the node's disk and TPM. It only supports a node with Secure Boot
// disabled (the degraded state these nodes are in); with Secure Boot enabled
// it refuses, because the correct re-seal then also rebinds PCR11 to a new
// signing key, which this does not do.
//
//	arca-node-migrate --version <12-digit> --source <dir>
//
// --source holds the build's UKI and usr images for <version>, named as the
// build wrote them (and as systemd-sysupdate's transfers match):
// <osname>_<version>.efi, .usr-<arch>.<u>.raw, .usr-<arch>-verity.<u>.raw and
// .usr-<arch>-verity-sig.<u>.raw.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/lxc/incus/v7/shared/subprocess"
	"golang.org/x/sys/unix"

	"github.com/lxc/incus-os/incus-osd/internal/secureboot"
	"github.com/lxc/incus-os/incus-osd/internal/systemd"
	"github.com/lxc/incus-os/incus-osd/internal/util"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "arca-node-migrate: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	version := ""
	source := ""

	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--version":
			i++
			if i >= len(args) {
				return errors.New("--version needs a value")
			}

			version = args[i]
		case "--source":
			i++
			if i >= len(args) {
				return errors.New("--source needs a value")
			}

			source = args[i]
		default:
			return errors.New("unknown argument: " + args[i])
		}
	}

	if !regexp.MustCompile(`^[0-9]{12}$`).MatchString(version) {
		return errors.New("--version must be 12 digits")
	}

	if source == "" {
		return errors.New("--source <dir> is required")
	}

	if os.Geteuid() != 0 {
		return errors.New("must run as root in the node's context")
	}

	ctx, stop := signal.NotifyContext(context.Background(), unix.SIGINT, unix.SIGTERM)
	defer stop()

	// Only migrate a Secure-Boot-disabled node (the state these nodes are in).
	sbEnabled, err := secureboot.Enabled()
	if err != nil {
		return fmt.Errorf("checking Secure Boot state: %w", err)
	}

	if sbEnabled {
		return errors.New("Secure Boot is enabled; this migration only supports the Secure-Boot-disabled case")
	}

	// Find the staged files for this version and copy them where
	// systemd-sysupdate looks for an update (SystemUpdatesPath). Their names
	// already match the sysupdate transfer patterns.
	staged, newUKI, err := stage(version, source)
	if err != nil {
		return err
	}

	fmt.Printf("arca-node-migrate: staged %d files for %s into %s\n", staged, version, systemd.SystemUpdatesPath)

	// Drop any pcrlock credentials so they are not measured into PCR12 on the
	// next boot (they would break the binding on a Secure-Boot-disabled node).
	_, err = secureboot.RemovePCRLockCredentials()
	if err != nil {
		return fmt.Errorf("removing pcrlock credentials: %w", err)
	}

	// Re-seal: bind the LUKS volumes to the PCR4 the new UKI will produce
	// (plus the unchanged PCR7 and the recomputed PCR12), authorised with the
	// node's recovery key file. This runs before sysupdate copies the UKI to
	// the ESP, exactly as incus-osd's apply does: the future PCR4 is computed
	// from the staged UKI, whose bytes are identical to what lands on the ESP.
	fmt.Println("arca-node-migrate: re-sealing the TPM for the new UKI")

	err = secureboot.UpdatePCR4Binding(ctx, newUKI)
	if err != nil {
		return fmt.Errorf("re-sealing TPM (UpdatePCR4Binding): %w", err)
	}

	// Write the new usr partitions and UKI with systemd-sysupdate, in a private
	// mount namespace with /usr mounted, the same workaround incus-osd uses.
	fmt.Println("arca-node-migrate: writing partitions with systemd-sysupdate")

	_, err = subprocess.RunCommandContext(ctx, "unshare", "-m", "--", "sh", "-c",
		"mount /dev/mapper/usr /usr && /usr/lib/systemd/systemd-sysupdate update "+version)
	if err != nil {
		return fmt.Errorf("systemd-sysupdate: %w", err)
	}

	// Flush writes so a forced reboot still sees a consistent ESP.
	unix.Sync()

	// Undo systemd's boot-counting rename for the new UKI, so the index-based
	// boot selection picks it (same as incus-osd's apply).
	err = clearBootCounting()
	if err != nil {
		return fmt.Errorf("clearing boot counting: %w", err)
	}

	// Boot the newest UKI (the one just written) next.
	err = util.SetNextBootID(ctx, util.BootIDNewest)
	if err != nil {
		return fmt.Errorf("selecting the new UKI for next boot: %w", err)
	}

	fmt.Printf("arca-node-migrate: migrated to %s; reboot to boot it (TPM will unlock automatically)\n", version)

	return nil
}

// stage copies the build's UKI and usr images for version from source into
// SystemUpdatesPath, returning the count and the staged UKI's path.
func stage(version string, source string) (int, string, error) {
	err := os.MkdirAll(systemd.SystemUpdatesPath, 0o755)
	if err != nil {
		return 0, "", err
	}

	entries, err := os.ReadDir(source)
	if err != nil {
		return 0, "", err
	}

	newUKI := ""
	count := 0

	for _, e := range entries {
		name := e.Name()

		isUKI := strings.HasSuffix(name, "_"+version+".efi")
		isUsr := strings.Contains(name, "_"+version+".usr-") && strings.HasSuffix(name, ".raw")

		if !isUKI && !isUsr {
			continue
		}

		src := filepath.Join(source, name)
		dst := filepath.Join(systemd.SystemUpdatesPath, name)

		err := copyFile(src, dst)
		if err != nil {
			return 0, "", err
		}

		count++

		if isUKI {
			newUKI = dst
		}
	}

	if newUKI == "" {
		return 0, "", errors.New("no UKI (" + version + ".efi) found in " + source)
	}

	// usr, usr-verity and usr-verity-sig.
	if count < 4 {
		return 0, "", fmt.Errorf("expected a UKI and three usr images in %s, found %d files", source, count)
	}

	return count, newUKI, nil
}

func copyFile(src string, dst string) error {
	in, err := os.Open(src) //nolint:gosec
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst) //nolint:gosec
	if err != nil {
		return err
	}

	_, err = out.ReadFrom(in)
	if err != nil {
		_ = out.Close()

		return err
	}

	return out.Close()
}

// clearBootCounting renames any "<uki>+3-0.efi" back to "<uki>.efi", undoing
// systemd's boot-counting rename so index-based boot selection is stable.
func clearBootCounting() error {
	dir := "/boot/EFI/Linux/"

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	for _, e := range entries {
		base, found := strings.CutSuffix(e.Name(), "+3-0.efi")
		if !found {
			continue
		}

		err := os.Rename(dir+e.Name(), dir+base+".efi")
		if err != nil {
			return err
		}
	}

	return nil
}
