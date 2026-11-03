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
// It must run as root in the node's own namespaces (its /dev, /sys, /boot,
// /var/lib/incus-os and the node's own systemd-cryptenroll, systemd-sysupdate
// and bootctl), which scripts/arca/migrate-node.sh gives it by entering the
// node's namespaces from a privileged container. It refuses to run anywhere
// else. It only supports a node with Secure Boot disabled (the degraded state
// these nodes are in); with Secure Boot enabled it refuses, because the
// correct re-seal then also rebinds PCR11 to a new signing key, which this
// does not do.
//
//	arca-node-migrate --version <12-digit> --source <dir>
//
// --source holds the build's UKI and usr images for <version>, named as the
// build wrote them (and as systemd-sysupdate's transfers match):
// <osname>_<version>.efi, .usr-<arch>.<u>.raw, .usr-<arch>-verity.<u>.raw and
// .usr-<arch>-verity-sig.<u>.raw, plus <app>.raw for every application the
// node has installed. The applications are staged as version <version>, so
// the first boot of the new build loads ones signed with its own key (the
// installed ones are signed with the old key, which the new build does not
// trust).
//
// Steps, in incus-osd's apply order: stage the OS files where sysupdate
// reads them and the applications where incus-osd keeps them; re-seal the
// LUKS volumes to the PCR4 the new UKI will produce and to the PCR11 policy
// of the new UKI's signing key (as incus-osd's key change does); write the
// partitions and the UKI with systemd-sysupdate; undo boot counting; select
// the new UKI for the next boot. On any failure after re-sealing, the binding
// is put back to the running UKI and the staged applications are removed, so
// the node keeps booting its current build without a recovery passphrase.
package main

import (
	"bytes"
	"context"
	"debug/pe"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/lxc/incus/v7/shared/subprocess"
	"golang.org/x/sys/unix"

	"github.com/lxc/incus-os/incus-osd/internal/secureboot"
	"github.com/lxc/incus-os/incus-osd/internal/systemd"
	"github.com/lxc/incus-os/incus-osd/internal/util"
)

// The paths that exist only in the node's own mount namespace.
var nodeContextPaths = []string{"/dev/mapper/usr", "/var/lib/incus-os/keys", "/boot/EFI/Linux", "/run/incus-os"}

// pcrPublicKeyPath holds the booted UKI's PCR11 policy signing key (systemd-stub
// puts it there); systemd-cryptenroll binds the PCR11 policy to whatever key it
// holds, so it must be the new UKI's key when re-sealing for the new UKI.
const pcrPublicKeyPath = "/run/systemd/tpm2-pcr-public-key.pem"

func main() {
	err := run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "arca-node-migrate: "+err.Error())
		os.Exit(1)
	}
}

func say(format string, args ...any) {
	fmt.Printf("arca-node-migrate: "+format+"\n", args...)
}

func run() error {
	version, source, err := parseArgs(os.Args[1:])
	if err != nil {
		return err
	}

	if os.Geteuid() != 0 {
		return errors.New("must run as root in the node's context")
	}

	for _, p := range nodeContextPaths {
		_, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("not in the node's own namespaces (%s: %w)", p, err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), unix.SIGINT, unix.SIGTERM)
	defer stop()

	// Only migrate a Secure-Boot-disabled node (the state these nodes are in).
	sbEnabled, err := secureboot.Enabled()
	if err != nil {
		return fmt.Errorf("checking Secure Boot state: %w", err)
	}

	if sbEnabled {
		return errors.New("secure boot is enabled; this migration only supports the Secure-Boot-disabled case")
	}

	ukis, err := util.GetUKIVersions()
	if err != nil {
		return fmt.Errorf("reading the running UKI: %w", err)
	}

	say("running %s (%s)", ukis.CurrentVersion, ukis.CurrentFilepath)

	if strings.Compare(version, ukis.CurrentVersion) <= 0 {
		return fmt.Errorf("version %s is not newer than the running %s", version, ukis.CurrentVersion)
	}

	apps, err := installedApps()
	if err != nil {
		return err
	}

	// Stage the OS files where systemd-sysupdate looks for an update.
	staged, err := stageOS(version, source)
	if err != nil {
		return err
	}

	newUKI := staged.newUKI

	say("staged %d OS files for %s into %s", staged.count, version, systemd.SystemUpdatesPath)

	// Stage the applications as version <version>.
	appDir := filepath.Join(systemd.LocalExtensionsPath, version)

	err = stageApps(apps, source, appDir)
	if err != nil {
		_ = os.RemoveAll(appDir)

		return err
	}

	say("staged applications %s into %s", strings.Join(apps, ","), appDir)

	// Drop any pcrlock credentials so they are not measured into PCR12 on the
	// next boot (incus-osd's apply does the same).
	_, err = secureboot.RemovePCRLockCredentials()
	if err != nil {
		_ = os.RemoveAll(appDir)

		return fmt.Errorf("removing pcrlock credentials: %w", err)
	}

	// The new UKI signs its PCR11 policy with the new key. Like incus-osd's
	// Secure Boot key change, put that key where systemd-cryptenroll reads
	// it, so the PCR11 policy binds to the key the new UKI's signatures use.
	oldKey, err := os.ReadFile(pcrPublicKeyPath)
	if err != nil {
		_ = os.RemoveAll(appDir)

		return fmt.Errorf("reading the running PCR signing key: %w", err)
	}

	newKey, err := ukiPCRPublicKey(newUKI)
	if err != nil {
		_ = os.RemoveAll(appDir)

		return err
	}

	if !bytes.Equal(bytes.TrimSpace(oldKey), bytes.TrimSpace(newKey)) {
		say("the new UKI signs PCR11 with a different key; binding the PCR11 policy to it")
	}

	// Keep the running ESP systemd-boot binaries, so rollback can restore them.
	oldLoaders, err := readBootLoaders()
	if err != nil {
		_ = os.RemoveAll(appDir)

		return fmt.Errorf("reading the current boot loader: %w", err)
	}

	// rollback puts the node back the way it was booting, after a failure past
	// this point: the ESP boot loader, the PCR signing key and the TPM binding
	// to the running UKI, and (should sysupdate already have written the newer
	// UKI, which systemd-boot would otherwise pick) the next boot pointed back
	// at the running UKI, which the TPM is sealed to again. The staged
	// applications are removed.
	rollback := func(cause error) error {
		say("rolling back to the running UKI")

		_ = os.RemoveAll(appDir)

		rbErr := restoreBootLoaders(oldLoaders)
		if rbErr == nil {
			rbErr = os.WriteFile(pcrPublicKeyPath, oldKey, 0o444)
		}

		if rbErr == nil {
			rbErr = secureboot.UpdatePCR4Binding(context.Background(), ukis.CurrentFilepath)
		}

		if rbErr != nil {
			return fmt.Errorf("%w; rolling back also failed: %w", cause, rbErr)
		}

		err := setBootOneShot(filepath.Base(ukis.CurrentFilepath))
		if err != nil {
			say("warning: could not point the next boot back at the running UKI (%v)", err)
		}

		return cause
	}

	// Replace the ESP systemd-boot binaries with the new build's signed ones.
	// This must happen before the re-seal, because PCR4 measures the boot
	// loader as well as the UKI.
	say("updating the ESP boot loader to the new build")

	err = updateBootLoaders(staged.newUsr)
	if err != nil {
		return rollback(fmt.Errorf("updating the boot loader: %w", err))
	}

	err = os.WriteFile(pcrPublicKeyPath, newKey, 0o444)
	if err != nil {
		return rollback(fmt.Errorf("writing the new PCR signing key: %w", err))
	}

	// Re-seal: bind the LUKS volumes to the PCR4 the new boot loader and UKI
	// will produce (plus the unchanged PCR7, the recomputed PCR12, an empty
	// PCR15 and the PCR11 policy of the new key), authorised with the node's
	// recovery key files. computeNewPCR4Value reads the boot loader from the
	// ESP (just updated) and the UKI from the staged file, which is what lands
	// on the ESP, so the future PCR4 matches the next boot.
	say("re-sealing the TPM for the new UKI")

	err = secureboot.UpdatePCR4Binding(ctx, newUKI)
	if err != nil {
		return rollback(fmt.Errorf("re-sealing the TPM: %w", err))
	}

	err = install(ctx, version, filepath.Base(newUKI))
	if err != nil {
		return rollback(err)
	}

	say("migrated to %s; reboot to boot it (the TPM unlocks it)", version)

	return nil
}

func parseArgs(args []string) (string, string, error) {
	version := ""
	source := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--version", "--source":
			if i+1 >= len(args) {
				return "", "", errors.New(args[i] + " needs a value")
			}

			if args[i] == "--version" {
				version = args[i+1]
			} else {
				source = args[i+1]
			}

			i++
		default:
			return "", "", errors.New("unknown argument: " + args[i])
		}
	}

	if !regexp.MustCompile(`^[0-9]{12}$`).MatchString(version) {
		return "", "", errors.New("--version must be 12 digits")
	}

	if source == "" {
		return "", "", errors.New("--source <dir> is required")
	}

	return version, source, nil
}

// install writes the partitions and the UKI with systemd-sysupdate, then
// prepares the next boot, as incus-osd's apply does.
func install(ctx context.Context, version string, newUKIName string) error {
	// In a private mount namespace with the raw /usr mounted (no sysexts),
	// the same workaround incus-osd uses.
	say("writing the partitions and the UKI with systemd-sysupdate")

	_, err := subprocess.RunCommandContext(ctx, "unshare", "-m", "--", "sh", "-c",
		"mount /dev/mapper/usr /usr && /usr/lib/systemd/systemd-sysupdate update "+version)
	if err != nil {
		return fmt.Errorf("systemd-sysupdate: %w", err)
	}

	// Flush writes so a forced reboot still sees a consistent ESP.
	unix.Sync()

	// Undo systemd's boot-counting rename for the new UKI, so index-based boot
	// selection picks it.
	err = clearBootCounting()
	if err != nil {
		return fmt.Errorf("clearing boot counting: %w", err)
	}

	// Select the new UKI for the next boot. incus-osd uses bootctl, but
	// bootctl refuses to write boot-loader EFI variables from a container, and
	// the migration runs from one (the node's PID namespace, which bootctl's
	// container check needs, cannot be joined from a descendant). So the
	// LoaderEntryOneShot variable is written straight to efivarfs. This is
	// best-effort: with no pinned default entry (IncusOS does not set one),
	// systemd-boot boots the newest UKI anyway, and the new build is newer.
	err = setBootOneShot(newUKIName)
	if err != nil {
		say("warning: could not set the one-shot boot entry (%v); relying on systemd-boot booting the newest UKI", err)
	}

	return nil
}

// loaderGUID is the systemd-boot vendor GUID for its EFI variables.
const loaderGUID = "4a67b082-0a4c-41cf-b6c7-440b29bb8c4f"

// fsImmutableFl is FS_IMMUTABLE_FL (efivarfs marks existing variables with it).
const fsImmutableFl = 0x00000010

// setBootOneShot writes the systemd-boot LoaderEntryOneShot EFI variable so the
// next boot selects entry (a UKI filename under /boot/EFI/Linux). The value is
// a NUL-terminated UTF-16LE string, prefixed in efivarfs by a 4-byte attribute
// word (non-volatile + boot-service + runtime access).
func setBootOneShot(entry string) error {
	path := "/sys/firmware/efi/efivars/LoaderEntryOneShot-" + loaderGUID

	// efivarfs marks existing variables immutable; clear that so it can be
	// rewritten. A missing variable is created by the write below.
	fd, err := unix.Open(path, unix.O_RDONLY, 0)
	if err == nil {
		flags, gerr := unix.IoctlGetInt(fd, unix.FS_IOC_GETFLAGS)
		if gerr == nil && flags&fsImmutableFl != 0 {
			_ = unix.IoctlSetPointerInt(fd, unix.FS_IOC_SETFLAGS, flags&^fsImmutableFl)
		}

		_ = unix.Close(fd)
	} else if !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("opening %s: %w", path, err)
	}

	// The value is a NUL-terminated UTF-16LE string. The entry name is a UKI
	// filename (ASCII), so each rune is one 16-bit code unit.
	runes := utf16.Encode([]rune(entry + "\x00"))

	buf := make([]byte, 4, 4+2*len(runes))
	buf[0] = 0x07 // NON_VOLATILE | BOOTSERVICE_ACCESS | RUNTIME_ACCESS

	for _, u := range runes {
		buf = append(buf, byte(u), byte(u>>8))
	}

	// efivarfs requires the attribute word and the data in a single write.
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644) //nolint:gosec
	if err != nil {
		return fmt.Errorf("opening %s for writing: %w", path, err)
	}

	_, werr := out.Write(buf)
	cerr := out.Close()

	if werr != nil {
		return fmt.Errorf("writing %s: %w", path, werr)
	}

	return cerr
}

// ukiPCRPublicKey returns the PEM public key in a UKI's .pcrpkey section.
func ukiPCRPublicKey(uki string) ([]byte, error) {
	f, err := pe.Open(uki)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", uki, err)
	}
	defer f.Close()

	section := f.Section(".pcrpkey")
	if section == nil {
		return nil, fmt.Errorf("%s has no .pcrpkey section", uki)
	}

	data, err := section.Data()
	if err != nil {
		return nil, fmt.Errorf("reading the .pcrpkey section of %s: %w", uki, err)
	}

	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("the .pcrpkey section of %s is not a PEM public key", uki)
	}

	return pem.EncodeToMemory(block), nil
}

// installedApps lists the applications the node runs: the <app>.raw entries
// incus-osd keeps in SystemExtensionsPath.
func installedApps() ([]string, error) {
	entries, err := os.ReadDir(systemd.SystemExtensionsPath)
	if err != nil {
		return nil, fmt.Errorf("listing installed applications: %w", err)
	}

	apps := []string{}

	for _, e := range entries {
		name, found := strings.CutSuffix(e.Name(), ".raw")
		if found && name != "" {
			apps = append(apps, name)
		}
	}

	if len(apps) == 0 {
		return nil, errors.New("no installed applications found in " + systemd.SystemExtensionsPath)
	}

	slices.Sort(apps)

	return apps, nil
}

// stagedOS names the files stageOS copied: how many, the UKI and the plain
// (non-verity) usr image, both under SystemUpdatesPath.
type stagedOS struct {
	count  int
	newUKI string
	newUsr string
}

// stageOS copies the build's UKI and usr images for version from source into
// SystemUpdatesPath (cleared first, as the provider's download does).
func stageOS(version string, source string) (stagedOS, error) {
	out := stagedOS{}

	err := os.RemoveAll(systemd.SystemUpdatesPath)
	if err != nil {
		return out, err
	}

	err = os.MkdirAll(systemd.SystemUpdatesPath, 0o700)
	if err != nil {
		return out, err
	}

	entries, err := os.ReadDir(source)
	if err != nil {
		return out, err
	}

	kinds := map[string]bool{}

	for _, e := range entries {
		name := e.Name()

		kind := ""

		switch {
		case strings.HasSuffix(name, "_"+version+".efi"):
			kind = "efi"
		case strings.Contains(name, "_"+version+".usr-") && strings.HasSuffix(name, ".raw"):
			rest := name[strings.Index(name, ".usr-")+1:]

			switch {
			case strings.Contains(rest, "-verity-sig."):
				kind = "usr-verity-sig"
			case strings.Contains(rest, "-verity."):
				kind = "usr-verity"
			default:
				kind = "usr"
			}
		default:
			continue
		}

		if kinds[kind] {
			return out, fmt.Errorf("more than one %s file for %s in %s", kind, version, source)
		}

		kinds[kind] = true

		dst := filepath.Join(systemd.SystemUpdatesPath, name)

		err := copyFile(filepath.Join(source, name), dst)
		if err != nil {
			return out, err
		}

		switch kind {
		case "efi":
			out.newUKI = dst
		case "usr":
			out.newUsr = dst
		}
	}

	for _, kind := range []string{"efi", "usr", "usr-verity", "usr-verity-sig"} {
		if !kinds[kind] {
			return out, fmt.Errorf("no %s file for %s in %s", kind, version, source)
		}
	}

	out.count = len(kinds)

	return out, nil
}

// espBootLoaders returns the ESP paths of the systemd-boot binary (both the
// systemd and the fallback BOOT location) and the path of the signed stub
// inside a mounted usr image, for the running architecture.
func espBootLoaders() (stub string, dests []string, err error) {
	switch runtime.GOARCH {
	case "amd64":
		return "lib/systemd/boot/efi/systemd-bootx64.efi.signed",
			[]string{"/boot/EFI/systemd/systemd-bootx64.efi", "/boot/EFI/BOOT/BOOTX64.EFI"}, nil
	case "arm64":
		return "lib/systemd/boot/efi/systemd-bootaa64.efi.signed",
			[]string{"/boot/EFI/systemd/systemd-bootaa64.efi", "/boot/EFI/BOOT/BOOTAA64.EFI"}, nil
	default:
		return "", nil, fmt.Errorf("architecture %s is not supported", runtime.GOARCH)
	}
}

// readBootLoaders reads the current ESP systemd-boot binaries, so they can be
// restored on rollback.
func readBootLoaders() (map[string][]byte, error) {
	_, dests, err := espBootLoaders()
	if err != nil {
		return nil, err
	}

	saved := map[string][]byte{}

	for _, d := range dests {
		b, err := os.ReadFile(d) //nolint:gosec
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", d, err)
		}

		saved[d] = b
	}

	return saved, nil
}

// restoreBootLoaders writes previously-read ESP systemd-boot binaries back.
func restoreBootLoaders(saved map[string][]byte) error {
	for path, data := range saved {
		err := writeFileSync(path, data)
		if err != nil {
			return err
		}
	}

	return nil
}

// updateBootLoaders replaces the ESP systemd-boot binaries with the signed stub
// from the new usr image. The new UKI's boot stub refuses to run under a
// systemd-boot binary not signed by a trusted key, so on a CA change the ESP
// binary must become the new build's. This runs before the PCR4 re-seal so the
// new binary is the one measured into PCR4.
func updateBootLoaders(newUsr string) error {
	stub, dests, err := espBootLoaders()
	if err != nil {
		return err
	}

	mountDir, err := os.MkdirTemp("/tmp", "arca-usr")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mountDir)

	err = unix.Mount(newUsr, mountDir, "erofs", unix.MS_RDONLY, "")
	if err != nil {
		return fmt.Errorf("mounting the new usr image: %w", err)
	}
	defer func() { _ = unix.Unmount(mountDir, 0) }()

	data, err := os.ReadFile(filepath.Join(mountDir, stub)) //nolint:gosec
	if err != nil {
		return fmt.Errorf("reading the signed boot stub from the new usr image: %w", err)
	}

	for _, d := range dests {
		err := writeFileSync(d, data)
		if err != nil {
			return err
		}
	}

	return nil
}

// writeFileSync writes data to path (truncating it) and flushes it to disk.
func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec
	if err != nil {
		return fmt.Errorf("opening %s for writing: %w", path, err)
	}

	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}

	cerr := f.Close()
	if werr != nil {
		return fmt.Errorf("writing %s: %w", path, werr)
	}

	return cerr
}

// stageApps copies <app>.raw for each installed application from source into
// dir (LocalExtensionsPath/<version>).
func stageApps(apps []string, source string, dir string) error {
	for _, app := range apps {
		_, err := os.Stat(filepath.Join(source, app+".raw"))
		if err != nil {
			return fmt.Errorf("the node runs application %s but %s has no %s.raw", app, source, app)
		}
	}

	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		return err
	}

	for _, app := range apps {
		err := copyFile(filepath.Join(source, app+".raw"), filepath.Join(dir, app+".raw"))
		if err != nil {
			return err
		}
	}

	return nil
}

func copyFile(src string, dst string) error {
	in, err := os.Open(src) //nolint:gosec
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec
	if err != nil {
		return err
	}

	_, err = out.ReadFrom(in)
	if err != nil {
		_ = out.Close()

		return err
	}

	err = out.Sync()
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
		if e.Type()&fs.ModeType != 0 {
			continue
		}

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
