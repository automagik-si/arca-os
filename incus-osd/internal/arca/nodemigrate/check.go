package main

import (
	"bytes"
	"context"
	"crypto"
	"debug/pe"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/foxboron/go-uefi/authenticode"
	"github.com/google/go-eventlog/tcg"
	"golang.org/x/sys/unix"

	"github.com/lxc/incus-os/incus-osd/internal/secureboot"
	"github.com/lxc/incus-os/incus-osd/internal/systemd"
	"github.com/lxc/incus-os/incus-osd/internal/util"
)

// plan is what the read-only preflight found: everything the migration
// needs, read before anything on the node changes.
type plan struct {
	version string
	source  string
	ukis    util.UKIVersions
	apps    []string

	// The build's OS files in source, by kind (efi, usr, usr-verity,
	// usr-verity-sig).
	osFiles map[string]string

	oldKey     []byte
	newKey     []byte
	oldLoaders map[string][]byte
	newLoader  []byte
}

// newUKIName is the new UKI's filename, as sysupdate writes it to the ESP
// (before its boot-counting suffix).
func (p *plan) newUKIName() string {
	return filepath.Base(p.osFiles["efi"])
}

// systemdStubGUID is the vendor media GUID under which older systemd-stubs had
// the firmware measure the UKI's kernel into PCR4 (with Secure Boot off).
var systemdStubGUID = []byte{0xf8, 0xd1, 0xc5, 0x55, 0xcd, 0x4, 0xb5, 0x46, 0x8a, 0x20, 0xe5, 0x6c, 0xbb, 0x30, 0x52, 0xd0}

// preflight reads the node and the source and checks every precondition of
// the migration. It changes nothing on the node.
func preflight(ctx context.Context, version string, source string) (*plan, error) {
	p := &plan{version: version, source: source}

	sbEnabled, err := secureboot.Enabled()
	if err != nil {
		return nil, fmt.Errorf("checking Secure Boot state: %w", err)
	}

	if sbEnabled {
		return nil, errors.New("secure boot is enabled; this migration only supports the Secure-Boot-disabled case")
	}

	p.ukis, err = util.GetUKIVersions()
	if err != nil {
		return nil, fmt.Errorf("reading the running UKI: %w", err)
	}

	sayf("running %s (%s)", p.ukis.CurrentVersion, p.ukis.CurrentFilepath)

	if strings.Compare(version, p.ukis.CurrentVersion) <= 0 {
		return nil, fmt.Errorf("version %s is not newer than the running %s", version, p.ukis.CurrentVersion)
	}

	p.apps, err = installedApps()
	if err != nil {
		return nil, err
	}

	for _, app := range p.apps {
		_, err := os.Stat(filepath.Join(source, app+".raw"))
		if err != nil {
			return nil, fmt.Errorf("the node runs application %s but %s has no %s.raw", app, source, app)
		}
	}

	p.osFiles, err = findOSFiles(version, source)
	if err != nil {
		return nil, err
	}

	sayf("source: %d OS files and applications %s for %s", len(p.osFiles), strings.Join(p.apps, ","), version)

	p.oldKey, err = os.ReadFile(pcrPublicKeyPath)
	if err != nil {
		return nil, fmt.Errorf("reading the running PCR signing key: %w", err)
	}

	p.newKey, err = ukiPCRPublicKey(p.osFiles["efi"])
	if err != nil {
		return nil, err
	}

	if !bytes.Equal(bytes.TrimSpace(p.oldKey), bytes.TrimSpace(p.newKey)) {
		sayf("the new UKI signs PCR11 with a different key; the PCR11 policy will be bound to it")
	}

	err = checkRecoveryKeys(ctx)
	if err != nil {
		return nil, err
	}

	p.oldLoaders, err = readBootLoaders()
	if err != nil {
		return nil, fmt.Errorf("reading the current boot loader: %w", err)
	}

	p.newLoader, err = readNewBootLoader(p.osFiles["usr"])
	if err != nil {
		return nil, err
	}

	err = checkPCR4(p)
	if err != nil {
		return nil, err
	}

	err = checkBootDefault(p.newUKIName())
	if err != nil {
		return nil, err
	}

	credentials, _ := os.ReadDir("/boot/loader/credentials")
	if len(credentials) > 0 {
		sayf("the ESP holds %d pcrlock credential files; they are removed before the re-seal", len(credentials))
	}

	err = checkSpace(p)
	if err != nil {
		return nil, err
	}

	return p, nil
}

// findOSFiles finds the build's UKI and usr images for version in source,
// named as the build wrote them (and as sysupdate's transfers match).
func findOSFiles(version string, source string) (map[string]string, error) {
	entries, err := os.ReadDir(source)
	if err != nil {
		return nil, err
	}

	found := map[string]string{}

	for _, e := range entries {
		name := e.Name()

		var kind string

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

		_, dup := found[kind]
		if dup {
			return nil, fmt.Errorf("more than one %s file for %s in %s", kind, version, source)
		}

		found[kind] = filepath.Join(source, name)
	}

	for _, kind := range []string{"efi", "usr", "usr-verity", "usr-verity-sig"} {
		_, ok := found[kind]
		if !ok {
			return nil, fmt.Errorf("no %s file for %s in %s", kind, version, source)
		}
	}

	return found, nil
}

// checkRecoveryKeys checks that every LUKS volume has the recovery key file
// the re-seal authorises with (incus-osd's UpdatePCR4Binding uses them).
func checkRecoveryKeys(ctx context.Context) error {
	volumes, err := util.GetLUKSVolumePartitions(ctx)
	if err != nil {
		return fmt.Errorf("listing the encrypted volumes: %w", err)
	}

	if len(volumes) == 0 {
		return errors.New("found no encrypted volumes")
	}

	names := []string{}

	for name := range volumes {
		_, err := os.Stat("/var/lib/incus-os/keys/recovery." + name + ".key")
		if err != nil {
			return fmt.Errorf("volume %s has no recovery key file: %w", name, err)
		}

		names = append(names, name)
	}

	sayf("encrypted volumes %s each have a recovery key file", strings.Join(names, ","))

	return nil
}

// readNewBootLoader reads the signed systemd-boot binary from the new usr
// image (mounted read-only for the read).
func readNewBootLoader(usrImage string) ([]byte, error) {
	stub, _, err := espBootLoaders()
	if err != nil {
		return nil, err
	}

	mountDir, err := os.MkdirTemp("/tmp", "arca-usr")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(mountDir)

	err = unix.Mount(usrImage, mountDir, "erofs", unix.MS_RDONLY, "")
	if err != nil {
		return nil, fmt.Errorf("mounting the new usr image: %w", err)
	}
	defer func() { _ = unix.Unmount(mountDir, 0) }()

	data, err := os.ReadFile(filepath.Join(mountDir, stub)) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("reading the signed boot loader from the new usr image: %w", err)
	}

	return data, nil
}

// pcr4Inputs says which binaries a PCR4 recomputation hashes in place of the
// ones on the ESP.
type pcr4Inputs struct {
	loader      []byte // the systemd-boot binary; nil hashes the ESP's
	uki         string // the UKI file; "" hashes the ESP's
	foldUKIPath bool   // match the UKI directory case-insensitively
}

// recomputePCR4 replays the PCR4 events of a validated event log, rehashing
// each boot application the firmware loaded from the ESP, as incus-osd's
// computeNewPCR4Value does. With in.foldUKIPath unset it matches the UKI by
// the exact /EFI/Linux/ prefix, as incus-osd does; set, it matches it the way
// the firmware's (case-insensitive) file system does. It returns the PCR4
// value and the boot application paths it saw.
func recomputePCR4(events []tcg.Event, in pcr4Inputs) ([]byte, []string, error) {
	_, loaders, err := espBootLoaders()
	if err != nil {
		return nil, nil, err
	}

	pcr := make([]byte, 32)
	paths := []string{}

	for _, e := range events {
		if e.Index != 4 {
			continue
		}

		digest := e.ReplayedDigest()

		if e.Type == tcg.EFIBootServicesApplication {
			peName, err := eventFilePath(e)
			if err != nil {
				return nil, nil, err
			}

			if peName != "" {
				paths = append(paths, peName)

				digest, err = bootApplicationDigest(peName, in, loaders, digest)
				if err != nil {
					return nil, nil, err
				}
			}
		}

		pcr = extend(pcr, digest)
	}

	return pcr, paths, nil
}

// bootApplicationDigest returns the digest PCR4 extends for one boot application:
// the binary the firmware loaded, rehashed, or the logged digest when the binary
// is not on the ESP (an early firmware or BMC binary), as incus-osd does.
func bootApplicationDigest(peName string, in pcr4Inputs, loaders []string, logged []byte) ([]byte, error) {
	isUKI := strings.HasPrefix(peName, "/boot/EFI/Linux/")
	if in.foldUKIPath {
		isUKI = strings.HasPrefix(strings.ToLower(peName), "/boot/efi/linux/")
	}

	isLoader := false

	for _, l := range loaders {
		if strings.EqualFold(peName, l) {
			isLoader = true
		}
	}

	var (
		h   []byte
		err error
	)

	switch {
	case isUKI && in.uki != "":
		h, err = fileAuthenticode(in.uki)
	case isLoader && in.loader != nil:
		h, err = bytesAuthenticode(in.loader)
	default:
		h, err = fileAuthenticode(peName)
	}

	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return logged, nil
		}

		return nil, err
	}

	return h, nil
}

// eventFilePath returns the ESP path ("/boot/...") of a boot application
// event's EFI file path, or "" when it has none.
func eventFilePath(e tcg.Event) (string, error) {
	load, err := tcg.ParseEFIImageLoad(bytes.NewReader(e.Data))
	if err != nil {
		return "", err
	}

	devPaths, err := load.DevicePath()
	if err != nil {
		return "", err
	}

	for _, dev := range devPaths {
		if dev.Type == tcg.MediaDevice && dev.Subtype == 4 {
			name, err := util.UTF16ToString(dev.Data)
			if err != nil {
				return "", err
			}

			return "/boot" + strings.ReplaceAll(name, "\\", "/"), nil
		}
	}

	return "", nil
}

// stubKernelEvents returns the digests of PCR4 events in which the firmware
// measured a UKI's kernel for systemd-stub (stubs before systemd 258).
func stubKernelEvents(events []tcg.Event) [][]byte {
	digests := [][]byte{}

	for _, e := range events {
		if e.Index != 4 || e.Type != tcg.EFIBootServicesApplication {
			continue
		}

		load, err := tcg.ParseEFIImageLoad(bytes.NewReader(e.Data))
		if err != nil {
			continue
		}

		devPaths, err := load.DevicePath()
		if err != nil {
			continue
		}

		for _, dev := range devPaths {
			if dev.Type == tcg.MediaDevice && dev.Subtype == 3 && bytes.Equal(dev.Data, systemdStubGUID) {
				digests = append(digests, e.ReplayedDigest())
			}
		}
	}

	return digests
}

// checkPCR4 checks that the PCR4 the re-seal binds to is the one the next
// boot produces. It validates the event log against the TPM, checks that
// rehashing the ESP's binaries reproduces the measured PCR4 (so the
// recomputation is sound on this firmware), and that incus-osd's prediction
// for the new boot loader and UKI agrees with a case-insensitive one.
func checkPCR4(p *plan) error {
	events, err := secureboot.GetValidatedTPMEventLog()
	if err != nil {
		return fmt.Errorf("the TPM event log does not validate: %w", err)
	}

	actual, err := secureboot.ReadPCR("4")
	if err != nil {
		return fmt.Errorf("reading PCR4: %w", err)
	}

	again, paths, err := recomputePCR4(events, pcr4Inputs{})
	if err != nil {
		return fmt.Errorf("recomputing PCR4: %w", err)
	}

	sayf("event log: validated; PCR4 %s; boot applications %s", short(actual), strings.Join(paths, " "))

	if !bytes.Equal(again, actual) {
		return fmt.Errorf("rehashing the ESP's boot binaries gives PCR4 %s, not the measured %s; the re-seal could not predict the next boot", short(again), short(actual))
	}

	predicted, _, err := recomputePCR4(events, pcr4Inputs{loader: p.newLoader, uki: p.osFiles["efi"]})
	if err != nil {
		return fmt.Errorf("predicting PCR4: %w", err)
	}

	folded, _, err := recomputePCR4(events, pcr4Inputs{loader: p.newLoader, uki: p.osFiles["efi"], foldUKIPath: true})
	if err != nil {
		return fmt.Errorf("predicting PCR4: %w", err)
	}

	if !bytes.Equal(predicted, folded) {
		return errors.New("the firmware logged the UKI under a path incus-osd's re-seal does not recognise (letter case); the re-seal would mispredict PCR4")
	}

	if bytes.Equal(predicted, actual) {
		return errors.New("the predicted PCR4 equals the running one; the event log has no measurement of the boot loader or UKI to rebind")
	}

	for _, d := range stubKernelEvents(events) {
		h, err := ukiKernelAuthenticode(p.osFiles["efi"])
		if err != nil {
			return err
		}

		if !bytes.Equal(d, h) {
			return errors.New("the firmware measured the UKI's kernel into PCR4 and the new kernel differs; incus-osd's re-seal replays that measurement, so it would mispredict PCR4")
		}
	}

	sayf("PCR4 for the new boot loader and UKI: %s", short(predicted))

	return nil
}

// checkBootDefault refuses when a pinned systemd-boot default names another
// entry: it would boot that entry from the second boot on, which the TPM is
// no longer sealed to.
func checkBootDefault(newUKIName string) error {
	raw, err := os.ReadFile(efiVarPath("LoaderEntryDefault"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("reading LoaderEntryDefault: %w", err)
	}

	if len(raw) <= 4 {
		return nil
	}

	entry, err := util.UTF16ToString(raw[4:])
	if err != nil {
		return fmt.Errorf("reading LoaderEntryDefault: %w", err)
	}

	entry = strings.TrimRight(entry, "\x00")
	if entry == "" || entry == newUKIName || strings.ContainsAny(entry, "*?[") {
		return nil
	}

	return fmt.Errorf("systemd-boot's default entry is pinned to %s, which the TPM is no longer sealed to after the migration; clear it first", entry)
}

// spaceNeed is the room one step needs on the file system holding path,
// counting what it frees there first.
type spaceNeed struct {
	what    string
	path    string
	need    int64
	reclaim int64
}

const mib = 1 << 20

// checkSpace checks that the staging directories and the ESP have room for
// the migration's writes, before anything is written.
func checkSpace(p *plan) error {
	osSize := int64(0)

	for _, f := range p.osFiles {
		osSize += fileSize(f)
	}

	appSize := int64(0)

	for _, app := range p.apps {
		appSize += fileSize(filepath.Join(p.source, app+".raw"))
	}

	// The ESP gets the new UKI and, per boot loader file, a temporary copy
	// that replaces it. sysupdate first removes UKIs other than the booted
	// one, to keep its InstancesMax.
	espNeed := fileSize(p.osFiles["efi"]) + int64(len(p.newLoader)) + mib

	for _, old := range p.oldLoaders {
		espNeed += max(0, int64(len(p.newLoader)-len(old)))
	}

	espReclaim := int64(0)

	ukiEntries, _ := os.ReadDir("/boot/EFI/Linux")
	for _, e := range ukiEntries {
		path := filepath.Join("/boot/EFI/Linux", e.Name())
		if strings.HasSuffix(e.Name(), ".efi") && path != p.ukis.CurrentFilepath {
			espReclaim += fileSize(path)
		}
	}

	needs := []spaceNeed{
		{"the OS files", systemd.SystemUpdatesPath, osSize + 64*mib, dirSize(systemd.SystemUpdatesPath)},
		{"the applications", filepath.Join(systemd.LocalExtensionsPath, p.version), appSize + 64*mib, 0},
		{"the ESP", "/boot/EFI/Linux", espNeed, espReclaim},
	}

	// Steps on one file system add up.
	type fsTotal struct {
		what          []string
		need, reclaim int64
		path          string
	}

	totals := map[uint64]*fsTotal{}
	order := []uint64{}

	for _, n := range needs {
		dir := existingAncestor(n.path)

		st, err := os.Stat(dir)
		if err != nil {
			return err
		}

		dev := st.Sys().(*syscall.Stat_t).Dev //nolint:forcetypeassert

		t, ok := totals[dev]
		if !ok {
			t = &fsTotal{path: dir}
			totals[dev] = t
			order = append(order, dev)
		}

		t.what = append(t.what, n.what)
		t.need += n.need
		t.reclaim += n.reclaim
	}

	for _, dev := range order {
		t := totals[dev]

		var sfs unix.Statfs_t

		err := unix.Statfs(t.path, &sfs)
		if err != nil {
			return fmt.Errorf("checking free space on %s: %w", t.path, err)
		}

		free := int64(sfs.Bavail) * sfs.Bsize //nolint:gosec

		sayf("space on %s for %s: needs %d MiB, %d MiB free (+%d MiB freed first)",
			t.path, strings.Join(t.what, " and "), t.need/mib, free/mib, t.reclaim/mib)

		if free+t.reclaim < t.need {
			return fmt.Errorf("not enough space on %s for %s", t.path, strings.Join(t.what, " and "))
		}
	}

	return nil
}

// testBootOneShot checks that the systemd-boot one-shot variable can be
// written from here: it writes entry, reads it back, then restores the
// variable as it was (or removes it).
func testBootOneShot(entry string) error {
	path := efiVarPath("LoaderEntryOneShot")

	orig, err := os.ReadFile(path) //nolint:gosec
	existed := err == nil

	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	err = setBootOneShot(entry)
	if err != nil {
		return err
	}

	got, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return err
	}

	want := oneShotValue(entry)

	var restoreErr error

	if existed {
		restoreErr = writeEFIVar(path, orig)
	} else {
		restoreErr = removeEFIVar(path)
	}

	if !bytes.Equal(got, want) {
		return errors.New("the variable read back differs from what was written")
	}

	if restoreErr != nil {
		return fmt.Errorf("restoring the variable: %w", restoreErr)
	}

	return nil
}

func fileAuthenticode(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return nil, err
	}
	defer f.Close()

	a, err := authenticode.Parse(f)
	if err != nil {
		return nil, fmt.Errorf("hashing %s: %w", path, err)
	}

	return a.Hash(crypto.SHA256), nil
}

func bytesAuthenticode(data []byte) ([]byte, error) {
	a, err := authenticode.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("hashing a PE binary: %w", err)
	}

	return a.Hash(crypto.SHA256), nil
}

// ukiKernelAuthenticode hashes the kernel in a UKI's .linux section, as the
// firmware measured it for systemd-stubs before 258.
func ukiKernelAuthenticode(uki string) ([]byte, error) {
	f, err := pe.Open(uki)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	s := f.Section(".linux")
	if s == nil {
		return nil, fmt.Errorf("%s has no .linux section", uki)
	}

	data, err := s.Data()
	if err != nil {
		return nil, err
	}

	if int(s.VirtualSize) > len(data) {
		return nil, fmt.Errorf("short .linux section in %s", uki)
	}

	return bytesAuthenticode(data[:s.VirtualSize])
}

// extend is the TPM's SHA-256 PCR extend.
func extend(pcr []byte, digest []byte) []byte {
	h := crypto.SHA256.New()
	_, _ = h.Write(pcr)
	_, _ = h.Write(digest)

	return h.Sum(nil)
}

func short(b []byte) string {
	s := hex.EncodeToString(b)
	if len(s) > 16 {
		return s[:16]
	}

	return s
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}

	return st.Size()
}

func dirSize(dir string) int64 {
	total := int64(0)

	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			info, err := d.Info()
			if err == nil {
				total += info.Size()
			}
		}

		return nil
	})

	return total
}

// existingAncestor returns path or its nearest existing parent.
func existingAncestor(path string) string {
	for {
		_, err := os.Stat(path)
		if err == nil || path == "/" {
			return path
		}

		path = filepath.Dir(path)
	}
}
