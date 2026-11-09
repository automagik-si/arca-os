package systemd

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lxc/incus/v7/shared/subprocess"

	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// NVIDIAMissingReason explains why the host is using its in-kernel fallback.
const NVIDIAMissingReason = "the NVIDIA driver for this Arca OS version is not installed yet"

// NVIDIAExtensionReasonPath passes early extension selection status to boot preparation.
const NVIDIAExtensionReasonPath = "/run/incus-os/nvidia-extension-reason"

type nvidiaExtensions struct {
	links         string
	images        string
	reason        string
	validate      func(context.Context, string) error
	preferred     string
	branch        string
	inspectOnly   bool
	pinned        bool
	skipSelection bool
	lockedChoice  bool
	release       string
	badBuilds     map[string]string
	badVersions   map[string]string
	driverVersion func(context.Context, string) (string, error)
}

func newNVIDIAExtensions() nvidiaExtensions {
	m := nvidiaExtensions{links: SystemExtensionsPath, images: LocalExtensionsPath, reason: NVIDIAExtensionReasonPath, validate: ValidateNVIDIAExtension, pinned: true}

	body, err := os.ReadFile("/var/lib/incus-os/state.txt")
	if err != nil {
		return m
	}

	s := state.State{}

	err = state.Decode(body, nil, &s)
	if err == nil && len(s.UnrecognizedFields) == 0 {
		m = configureNVIDIAExtensions(m, &s)
	}

	return m
}

func configureNVIDIAExtensions(m nvidiaExtensions, s *state.State) nvidiaExtensions {
	app := &s.Applications.NVIDIA
	m.preferred, m.branch, m.pinned = app.State.Version, app.State.RequestedBranch, app.Config.PinCurrentVersions

	m.release, m.badBuilds, m.badVersions = s.OS.RunningRelease, app.State.BadBuilds, app.State.BadVersions
	if app.State.NeedsReboot {
		m.pinned, m.lockedChoice = true, true
	}

	branch := m.branch
	if branch == "" {
		branch = "nvidia"
	}

	key := m.release + ":" + branch + ":" + m.preferred
	interrupted := app.State.PendingHealth != "" || app.State.PendingAttempt != ""

	activeBranch := app.State.ActiveBranch
	if activeBranch == "" {
		activeBranch = "nvidia"
	}

	failedDriver := branch == activeBranch && m.preferred == app.State.ActiveExtensionVersion && m.badVersions[m.release+":"+app.State.DriverVersion] != ""

	failed := m.badBuilds[key] != "" || failedDriver
	if app.State.Previous != nil && (failed || interrupted) {
		m.branch, m.preferred, m.pinned, m.lockedChoice = app.State.Previous.Branch, app.State.Previous.Version, true, true
	}

	return m
}

func (m nvidiaExtensions) validateBuild(ctx context.Context, image string, branch string, version string) error {
	reason := m.badBuilds[m.release+":"+branch+":"+version]
	if reason != "" {
		return errors.New(reason)
	}

	err := m.validate(ctx, image)
	if err != nil {
		return err
	}

	if len(m.badVersions) == 0 {
		return nil
	}

	readVersion := m.driverVersion
	if readVersion == nil {
		readVersion = func(ctx context.Context, image string) (string, error) {
			return subprocess.RunCommandContext(ctx, "systemd-dissect", "--copy-from", image, "/usr/share/nvidia/version")
		}
	}

	driver, err := readVersion(ctx, image)
	if err != nil {
		return err
	}

	reason = m.badVersions[m.release+":"+strings.TrimSpace(driver)]
	if reason != "" {
		return errors.New(reason)
	}

	return nil
}

// ValidateNVIDIAExtensions excludes incompatible GPU images without blocking other applications.
func ValidateNVIDIAExtensions(ctx context.Context) error {
	manager := newNVIDIAExtensions()
	manager.check(ctx)

	return nil
}

// SelectNVIDIAExtension keeps a compatible image linked while retaining pending OS-specific images.
func SelectNVIDIAExtension(ctx context.Context, preferred string, pinned bool) (string, []string, string) {
	manager := newNVIDIAExtensions()

	if manager.lockedChoice {
		preferred, pinned = manager.preferred, manager.pinned
	}

	return manager.selectImage(ctx, preferred, pinned)
}

// ValidateNVIDIAExtension checks a signed GPU image before selecting it for a merge.
func ValidateNVIDIAExtension(ctx context.Context, image string) error {
	err := VerifyExtension(ctx, image)
	if err != nil {
		return err
	}

	kernel, err := subprocess.RunCommandContext(ctx, "systemd-dissect", "--copy-from", image, "/usr/share/nvidia/kernel")
	if err != nil {
		return err
	}

	running, err := subprocess.RunCommandContext(ctx, "uname", "-r")
	if err != nil {
		return err
	}

	return validateNVIDIAKernel(kernel, running)
}

func (m nvidiaExtensions) check(ctx context.Context) {
	entries, _ := os.ReadDir(m.links)
	for _, entry := range entries {
		if entry.Name() != "nvidia.raw" && (!strings.HasPrefix(entry.Name(), "nvidia-") || !strings.HasSuffix(entry.Name(), ".raw")) {
			continue
		}

		path := filepath.Join(m.links, entry.Name())

		target, err := filepath.EvalSymlinks(path)
		if err == nil {
			err = m.validateBuild(ctx, target, strings.TrimSuffix(filepath.Base(target), ".raw"), filepath.Base(filepath.Dir(target)))
		}

		if err == nil {
			continue
		}

		excludeNVIDIAImage(path)
		m.record(NVIDIAMissingReason)
	}

	if m.preferred != "" && !m.skipSelection {
		_, _, _ = m.selectImage(ctx, m.preferred, m.pinned)
	}
}

// FindNVIDIAExtension selects a verified pack without changing the running extension.
func FindNVIDIAExtension(ctx context.Context, branch string, preferred string, pinned bool) (string, []string, string) {
	manager := newNVIDIAExtensions()
	manager.branch, manager.inspectOnly = branch, true

	return manager.selectImage(ctx, preferred, pinned)
}

func (m nvidiaExtensions) selectImage(ctx context.Context, preferred string, pinned bool) (string, []string, string) {
	branch := m.branch
	if branch == "" {
		branch = "nvidia"
	}

	link := filepath.Join(m.links, branch+".raw")
	versions := []string{}

	entries, _ := os.ReadDir(m.images)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		_, err := os.Stat(filepath.Join(m.images, entry.Name(), branch+".raw"))
		if err == nil {
			versions = append(versions, entry.Name())
		}
	}

	slices.Sort(versions)

	target, _ := os.Readlink(link)
	current := filepath.Base(filepath.Dir(target))
	candidates := []string{preferred, current}

	if !pinned {
		candidates = nil
		for _, version := range slices.Backward(versions) {
			candidates = append(candidates, version)
		}
	}

	for _, version := range candidates {
		if version == "" || version == "." || !slices.Contains(versions, version) {
			continue
		}

		image := filepath.Join(m.images, version, branch+".raw")

		err := m.validateBuild(ctx, image, branch, version)
		if err != nil {
			continue
		}

		if m.inspectOnly {
			return version, versions, ""
		}

		// Never remove the working link before a verified replacement is ready.
		err = os.MkdirAll(m.links, 0o700)
		if err == nil {
			_ = os.Remove(link + ".new")
			err = os.Symlink(image, link+".new")
		}

		if err == nil {
			err = os.Rename(link+".new", link)
		}

		if err != nil {
			m.record("the NVIDIA application link could not be updated")

			return "", versions, "the NVIDIA application link could not be updated"
		}

		other := "nvidia"
		if branch == other {
			other = "nvidia-lts"
		}

		_ = os.Remove(filepath.Join(m.links, other+".raw"))
		m.record("")

		return version, versions, ""
	}

	if m.inspectOnly {
		return "", versions, NVIDIAMissingReason
	}

	excludeNVIDIAImage(link)
	m.record(NVIDIAMissingReason)

	return "", versions, NVIDIAMissingReason
}

func excludeNVIDIAImage(path string) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}

	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		err = os.Remove(path)
	} else if err == nil {
		err = os.Rename(path, path+".disabled")
	}

	if err != nil {
		slog.Warn("Unable to exclude the NVIDIA extension; other applications will still be attempted")
	}
}

func (m nvidiaExtensions) record(reason string) {
	if reason == "" {
		_ = os.Remove(m.reason)

		return
	}

	_ = os.MkdirAll(filepath.Dir(m.reason), 0o700)
	_ = os.WriteFile(m.reason, []byte(reason), 0o600)
	slog.Warn(reason)
}

func validateNVIDIAKernel(kernel string, running string) error {
	if strings.TrimSpace(kernel) == "" || strings.TrimSpace(kernel) != strings.TrimSpace(running) {
		return errors.New(NVIDIAMissingReason)
	}

	return nil
}
