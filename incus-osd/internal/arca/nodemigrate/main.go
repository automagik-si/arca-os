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
// these nodes are in).
//
//	arca-node-migrate [--check] --version <12-digit> --source <dir>
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
// Both modes first run the same read-only preflight: Secure Boot off, the
// version newer, the files present, a recovery key file per encrypted volume,
// the TPM event log validated, the ESP's binaries reproducing the measured
// PCR4, a sound PCR4 prediction for the new boot loader and UKI, and room on
// every file system written. --check then also tests that the systemd-boot
// one-shot variable can be written (restoring it) and stops; it changes
// nothing else.
//
// The migration, in incus-osd's apply order: stage the OS files where
// sysupdate reads them and the applications where incus-osd keeps them;
// replace the ESP's systemd-boot with the new build's signed one (PCR4
// measures it, and the new UKI's stub refuses an untrusted one); re-seal the
// LUKS volumes to the PCR4 the new boot loader and UKI will produce and to
// the PCR11 policy of the new UKI's signing key; write the partitions and the
// UKI with systemd-sysupdate; undo boot counting; select the new UKI for the
// next boot.
//
// On a failure or an interrupt (SIGINT, SIGTERM) after the ESP changed, every
// rollback step runs even when an earlier one fails: the staged applications
// and the new UKI are removed, the boot loader and the PCR signing key are
// put back, the next boot is pointed at the running UKI and, last, the TPM is
// re-sealed to what is then on the ESP. A running step (the re-seal,
// sysupdate) always completes first. Hangups and broken pipes are ignored, so
// a dropped session does not stop it half way. Every line also goes to
// /run/arca-node-migrate.log on the node (until it reboots) and to the node's
// journal as arca-node-migrate, which persists (the initrd's root cleanup
// removes any other file under /var/log at boot) and which the /os API serves
// at /1.0/debug/log.
//
// Exit codes: 0 migrated (or --check passed); 1 failed, and the node boots
// its running build as before (nothing changed, or fully rolled back); 2
// usage; 3 failed and the rollback is incomplete: do not reboot the node.
package main

import (
	"bytes"
	"context"
	"debug/pe"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/lxc/incus/v7/shared/subprocess"
	"golang.org/x/sys/unix"

	"github.com/lxc/incus-os/incus-osd/internal/secureboot"
	"github.com/lxc/incus-os/incus-osd/internal/systemd"
)

const (
	exitOK                 = 0
	exitFailed             = 1
	exitUsage              = 2
	exitRollbackIncomplete = 3
)

// The paths that exist only in the node's own mount namespace.
var nodeContextPaths = []string{"/dev/mapper/usr", "/var/lib/incus-os/keys", "/boot/EFI/Linux", "/run/incus-os"}

// pcrPublicKeyPath holds the booted UKI's PCR11 policy signing key (systemd-stub
// puts it there); systemd-cryptenroll binds the PCR11 policy to whatever key it
// holds, so it must be the new UKI's key when re-sealing for the new UKI.
const pcrPublicKeyPath = "/run/systemd/tpm2-pcr-public-key.pem"

// logPath is the node-side log every line is appended to. It is on /run,
// because the initrd removes unlisted files from the root partition at boot;
// the journal keeps the lines across reboots.
const logPath = "/run/arca-node-migrate.log"

// journalSocket is journald's native protocol socket.
const journalSocket = "/run/systemd/journal/socket"

var (
	logFile *os.File
	journal *net.UnixConn
)

func main() {
	// A dropped exec session must not stop the helper half way: ignore
	// hangups, and broken pipes (output writes then fail, and say ignores
	// that).
	signal.Ignore(unix.SIGHUP, unix.SIGPIPE)

	os.Exit(run(os.Args[1:]))
}

// say prints one line and appends it to the node-side log. It never fails:
// a closed output or an unwritable log is ignored.
func sayf(format string, args ...any) {
	line := fmt.Sprintf("arca-node-migrate: "+format+"\n", args...)

	_, _ = os.Stdout.WriteString(line)

	if logFile != nil {
		_, _ = logFile.WriteString(time.Now().UTC().Format(time.RFC3339) + " " + line)
		_ = logFile.Sync()
	}

	if journal != nil {
		msg := strings.ReplaceAll(strings.TrimSuffix(line, "\n"), "\n", " ")

		_ = journal.SetWriteDeadline(time.Now().Add(time.Second))
		_, _ = journal.Write([]byte("SYSLOG_IDENTIFIER=arca-node-migrate\nPRIORITY=6\nMESSAGE=" + msg + "\n"))
	}
}

type options struct {
	check   bool
	version string
	source  string
}

func run(args []string) int {
	opts, err := parseArgs(args)
	if err != nil {
		sayf("usage: %v", err)

		return exitUsage
	}

	if os.Geteuid() != 0 {
		sayf("error: must run as root in the node's context")

		return exitFailed
	}

	for _, p := range nodeContextPaths {
		_, err := os.Stat(p)
		if err != nil {
			sayf("error: not in the node's own namespaces (%s: %v)", p, err)

			return exitFailed
		}
	}

	logFile, err = os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		logFile = nil

		sayf("warning: cannot write %s (%v)", logPath, err)
	}

	journal, err = net.DialUnix("unixgram", nil, &net.UnixAddr{Name: journalSocket, Net: "unixgram"})
	if err != nil {
		journal = nil

		sayf("warning: cannot log to the journal (%v)", err)
	}

	mode := "migrate"
	if opts.check {
		mode = "check"
	}

	sayf("start: %s to %s (log %s and the journal)", mode, opts.version, logPath)

	ctx, stop := signal.NotifyContext(context.Background(), unix.SIGINT, unix.SIGTERM)
	defer stop()

	p, err := preflight(ctx, opts.version, opts.source)
	if err != nil {
		sayf("error: %v", err)
		sayf("result: failed; nothing changed")

		return exitFailed
	}

	if opts.check {
		err := testBootOneShot(filepath.Base(p.ukis.CurrentFilepath))
		if err != nil {
			sayf("warning: the systemd-boot one-shot variable cannot be written from here (%v); the reboot through the /os API sets it from incus-osd, and systemd-boot boots the newest UKI without it", err)
		} else {
			sayf("the systemd-boot one-shot variable is writable (tested and restored)")
		}

		sayf("result: check passed; nothing changed")

		return exitOK
	}

	return migrate(ctx, p)
}

func parseArgs(args []string) (options, error) {
	opts := options{}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--check":
			opts.check = true
		case "--version", "--source":
			if i+1 >= len(args) {
				return opts, errors.New(args[i] + " needs a value")
			}

			if args[i] == "--version" {
				opts.version = args[i+1] //nolint:gosec // G602: i+1 is bounds-checked above
			} else {
				opts.source = args[i+1] //nolint:gosec // G602: i+1 is bounds-checked above
			}

			i++
		default:
			return opts, errors.New("unknown argument: " + args[i])
		}
	}

	if !regexp.MustCompile(`^[0-9]{12}$`).MatchString(opts.version) {
		return opts, errors.New("--version must be 12 digits")
	}

	if opts.source == "" {
		return opts, errors.New("--source <dir> is required")
	}

	return opts, nil
}

// migrate stages, re-seals and installs the build the preflight checked.
func migrate(ctx context.Context, p *plan) int {
	appDir := filepath.Join(systemd.LocalExtensionsPath, p.version)

	unstage := func() {
		_ = os.RemoveAll(appDir)
		_ = os.RemoveAll(systemd.SystemUpdatesPath)
	}

	// Stage the OS files where systemd-sysupdate looks for an update, and the
	// applications as version <version>. Nothing that boots has changed yet.
	newUKI, err := stageOS(p)
	if err == nil {
		sayf("staged %d OS files into %s", len(p.osFiles), systemd.SystemUpdatesPath)

		err = stageApps(p.apps, p.source, appDir)
	}

	if err == nil {
		sayf("staged applications %s into %s", strings.Join(p.apps, ","), appDir)
	}

	if err == nil && ctx.Err() != nil {
		err = errors.New("interrupted")
	}

	if err != nil {
		unstage()
		sayf("error: staging: %v", err)
		sayf("result: failed; nothing changed")

		return exitFailed
	}

	// From here on the ESP and the TPM binding change, so every failure, and
	// an interrupt between steps, rolls back.
	steps := []struct {
		what string
		do   func() error
	}{
		{"removing pcrlock credentials", func() error {
			// So they are not measured into PCR12 on the next boot
			// (incus-osd's apply does the same).
			_, err := secureboot.RemovePCRLockCredentials()

			return err
		}},
		{"updating the ESP boot loader to the new build", func() error {
			// Before the re-seal, because PCR4 measures the boot loader.
			return updateBootLoaders(p.newLoader)
		}},
		{"binding the PCR11 policy to the new UKI's signing key", func() error {
			return writeFileAtomic(pcrPublicKeyPath, p.newKey, 0o444)
		}},
		{"re-sealing the TPM for the new boot loader and UKI", func() error {
			// Binds the LUKS volumes to the PCR4 the new boot loader and
			// UKI will produce (computeNewPCR4Value reads the loader from
			// the ESP, just updated, and the UKI from the staged file,
			// which is what lands on the ESP), the unchanged PCR7, the
			// recomputed PCR12, an empty PCR15 and the PCR11 policy of the
			// new key, authorised with the recovery key files. It is not
			// cancelled: an interrupt rolls back once it has finished.
			return secureboot.UpdatePCR4Binding(context.WithoutCancel(ctx), newUKI)
		}},
		{"writing the partitions and the UKI with systemd-sysupdate", func() error {
			return install(context.WithoutCancel(ctx), p.version, filepath.Base(newUKI))
		}},
	}

	for _, s := range steps {
		sayf("%s", s.what)

		err := s.do()
		if err != nil {
			return rollback(ctx, p, appDir, fmt.Errorf("%s: %w", s.what, err))
		}

		if ctx.Err() != nil {
			return rollback(ctx, p, appDir, errors.New("interrupted"))
		}
	}

	sayf("result: migrated to %s; reboot to boot it (the TPM unlocks it)", p.version)

	return exitOK
}

// rollback puts the node back the way it was booting. Every step runs, even
// when an earlier one failed, and the TPM re-seal runs last, against what is
// then on the ESP.
func rollback(ctx context.Context, p *plan, appDir string, cause error) int {
	sayf("error: %v", cause)
	sayf("rolling back to the running UKI %s", filepath.Base(p.ukis.CurrentFilepath))

	failed := []string{}

	step := func(what string, err error) {
		if err != nil {
			sayf("rollback: %s FAILED: %v", what, err)

			failed = append(failed, what)

			return
		}

		sayf("rollback: %s", what)
	}

	step("removing the staged applications", os.RemoveAll(appDir))

	err := os.RemoveAll(systemd.SystemUpdatesPath)
	if err != nil {
		sayf("rollback: warning: removing the staged OS files: %v", err)
	}

	ukiErr := removeUKI(p.newUKIName())
	step("removing the new UKI from the ESP", ukiErr)

	step("restoring the ESP boot loader", restoreBootLoaders(p.oldLoaders))
	step("restoring the PCR signing key", writeFileAtomic(pcrPublicKeyPath, p.oldKey, 0o444))

	// With the new UKI gone systemd-boot cannot pick it; the one-shot only
	// matters when its removal failed.
	err = setBootOneShot(filepath.Base(p.ukis.CurrentFilepath))
	if ukiErr != nil {
		step("pointing the next boot at the running UKI", err)
	} else if err != nil {
		sayf("rollback: warning: pointing the next boot at the running UKI: %v (the new UKI is gone, so it boots anyway)", err)
	}

	unix.Sync()

	step("re-sealing the TPM for the running UKI", secureboot.UpdatePCR4Binding(context.WithoutCancel(ctx), p.ukis.CurrentFilepath))

	if len(failed) > 0 {
		sayf("result: ROLLBACK INCOMPLETE (%s failed); DO NOT REBOOT the node: it may ask for its recovery passphrase or not boot its build; see %s or the journal (arca-node-migrate)", strings.Join(failed, ", "), logPath)

		return exitRollbackIncomplete
	}

	sayf("result: failed and rolled back; the node keeps booting %s", p.ukis.CurrentVersion)

	return exitFailed
}

// install writes the partitions and the UKI with systemd-sysupdate, then
// prepares the next boot, as incus-osd's apply does.
func install(ctx context.Context, version string, newUKIName string) error {
	// In a private mount namespace with the raw /usr mounted (no sysexts),
	// the same workaround incus-osd uses.
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
	// best-effort: with no pinned default entry (the preflight refuses one),
	// systemd-boot boots the newest UKI anyway, and the new build is newer;
	// the reboot through the /os API also sets it, from incus-osd.
	err = setBootOneShot(newUKIName)
	if err != nil {
		sayf("warning: could not set the one-shot boot entry (%v); relying on systemd-boot booting the newest UKI", err)
	}

	return nil
}

// loaderGUID is the systemd-boot vendor GUID for its EFI variables.
const loaderGUID = "4a67b082-0a4c-41cf-b6c7-440b29bb8c4f"

// fsImmutableFl is FS_IMMUTABLE_FL (efivarfs marks existing variables with it).
const fsImmutableFl = 0x00000010

func efiVarPath(name string) string {
	return "/sys/firmware/efi/efivars/" + name + "-" + loaderGUID
}

// oneShotValue is the efivarfs content of LoaderEntryOneShot selecting entry:
// a 4-byte attribute word (non-volatile + boot-service + runtime access), then
// the NUL-terminated UTF-16LE entry name.
func oneShotValue(entry string) []byte {
	// The entry name is a UKI filename (ASCII), so each rune is one 16-bit
	// code unit.
	runes := utf16.Encode([]rune(entry + "\x00"))

	buf := make([]byte, 4, 4+2*len(runes))
	buf[0] = 0x07 // NON_VOLATILE | BOOTSERVICE_ACCESS | RUNTIME_ACCESS

	for _, u := range runes {
		buf = append(buf, byte(u), byte(u>>8)) //nolint:gosec // G115: u is 16 bits, so both bytes fit
	}

	return buf
}

// setBootOneShot writes the systemd-boot LoaderEntryOneShot EFI variable so the
// next boot selects entry (a UKI filename under /boot/EFI/Linux).
func setBootOneShot(entry string) error {
	return writeEFIVar(efiVarPath("LoaderEntryOneShot"), oneShotValue(entry))
}

// clearImmutable clears the immutable flag efivarfs puts on a variable, so it
// can be rewritten or removed. A missing variable is not an error.
func clearImmutable(path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	} else if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer unix.Close(fd) //nolint:errcheck

	flags, err := unix.IoctlGetInt(fd, unix.FS_IOC_GETFLAGS)
	if err == nil && flags&fsImmutableFl != 0 {
		_ = unix.IoctlSetPointerInt(fd, unix.FS_IOC_SETFLAGS, flags&^fsImmutableFl)
	}

	return nil
}

// writeEFIVar writes raw (attribute word and data) to an efivarfs variable.
func writeEFIVar(path string, raw []byte) error {
	err := clearImmutable(path)
	if err != nil {
		return err
	}

	// efivarfs requires the attribute word and the data in a single write.
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644) //nolint:gosec
	if err != nil {
		return fmt.Errorf("opening %s for writing: %w", path, err)
	}

	_, werr := out.Write(raw)
	cerr := out.Close()

	if werr != nil {
		return fmt.Errorf("writing %s: %w", path, werr)
	}

	return cerr
}

// removeEFIVar deletes an efivarfs variable.
func removeEFIVar(path string) error {
	err := clearImmutable(path)
	if err != nil {
		return err
	}

	err = os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	return err
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

// stageOS copies the build's UKI and usr images into SystemUpdatesPath
// (cleared first, as the provider's download does) and returns the staged
// UKI.
func stageOS(p *plan) (string, error) {
	err := os.RemoveAll(systemd.SystemUpdatesPath)
	if err != nil {
		return "", err
	}

	err = os.MkdirAll(systemd.SystemUpdatesPath, 0o700)
	if err != nil {
		return "", err
	}

	for _, src := range p.osFiles {
		err := copyFile(src, filepath.Join(systemd.SystemUpdatesPath, filepath.Base(src)))
		if err != nil {
			return "", err
		}
	}

	return filepath.Join(systemd.SystemUpdatesPath, p.newUKIName()), nil
}

// espBootLoaders returns the ESP paths of the systemd-boot binary (both the
// systemd and the fallback BOOT location) and the path of the signed stub
// inside a mounted usr image, for the running architecture.
func espBootLoaders() (string, []string, error) {
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

// restoreBootLoaders writes previously-read ESP systemd-boot binaries back,
// each one replaced atomically, or rewritten in place should the ESP have no
// room for the temporary copy.
func restoreBootLoaders(saved map[string][]byte) error {
	errs := []error{}

	for path, data := range saved {
		cur, err := os.ReadFile(path) //nolint:gosec
		if err == nil && bytes.Equal(cur, data) {
			continue
		}

		err = writeFileAtomic(path, data, 0o644)
		if err != nil {
			err = writeFileInPlace(path, data)
		}

		if err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// updateBootLoaders replaces the ESP systemd-boot binaries with the new
// build's signed one. The new UKI's boot stub refuses to run under a
// systemd-boot binary not signed by a trusted key, so on a CA change the ESP
// binary must become the new build's.
func updateBootLoaders(data []byte) error {
	_, dests, err := espBootLoaders()
	if err != nil {
		return err
	}

	for _, d := range dests {
		err := writeFileAtomic(d, data, 0o644)
		if err != nil {
			return err
		}
	}

	return nil
}

// writeFileAtomic replaces path with data: it writes and flushes a temporary
// file next to it, renames that over path and flushes the directory, so path
// holds either its old or its new content, never a partial one.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, ".arca-tmp-"+filepath.Base(path))

	err := writeFileInPlace(tmp, data)
	if err == nil {
		err = os.Chmod(tmp, mode)
	}

	if err == nil {
		err = os.Rename(tmp, path)
	}

	if err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("replacing %s: %w", path, err)
	}

	d, err := os.Open(dir) //nolint:gosec
	if err == nil {
		_ = d.Sync()
		_ = d.Close()
	}

	return nil
}

// writeFileInPlace writes data to path (truncating it) and flushes it.
func writeFileInPlace(path string, data []byte) error {
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

// ukiDir is where systemd-boot finds the UKIs.
const ukiDir = "/boot/EFI/Linux"

// isUKIFile reports whether name is the UKI <stem>.efi or one of its
// boot-counted names (<stem>+<left>[-<done>].efi).
func isUKIFile(name string, ukiName string) bool {
	stem := strings.TrimSuffix(ukiName, ".efi")

	return name == ukiName || (strings.HasPrefix(name, stem+"+") && strings.HasSuffix(name, ".efi"))
}

// removeUKI deletes a UKI from the ESP under any of its boot-counted names.
func removeUKI(ukiName string) error {
	entries, err := os.ReadDir(ukiDir)
	if err != nil {
		return err
	}

	errs := []error{}

	for _, e := range entries {
		if !isUKIFile(e.Name(), ukiName) {
			continue
		}

		err := os.Remove(filepath.Join(ukiDir, e.Name()))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}

	unix.Sync()

	return errors.Join(errs...)
}

// clearBootCounting renames any "<uki>+3-0.efi" back to "<uki>.efi", undoing
// systemd's boot-counting rename so index-based boot selection is stable.
func clearBootCounting() error {
	entries, err := os.ReadDir(ukiDir)
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

		err := os.Rename(filepath.Join(ukiDir, e.Name()), filepath.Join(ukiDir, base+".efi"))
		if err != nil {
			return err
		}
	}

	return nil
}
