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
)

// NVIDIAMissingReason explains why the host is using its in-kernel fallback.
const NVIDIAMissingReason = "the NVIDIA driver for this Arca OS version is not installed yet"

// NVIDIAExtensionReasonPath passes early extension selection status to boot preparation.
const NVIDIAExtensionReasonPath = "/run/incus-os/nvidia-extension-reason"

type nvidiaExtensions struct {
	links    string
	images   string
	reason   string
	validate func(context.Context, string) error
}

func newNVIDIAExtensions() nvidiaExtensions {
	return nvidiaExtensions{SystemExtensionsPath, LocalExtensionsPath, NVIDIAExtensionReasonPath, ValidateNVIDIAExtension}
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
	entries, err := os.ReadDir(m.links)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.Name() != "nvidia.raw" && (!strings.HasPrefix(entry.Name(), "nvidia-") || !strings.HasSuffix(entry.Name(), ".raw")) {
			continue
		}

		path := filepath.Join(m.links, entry.Name())

		err := m.validate(ctx, path)
		if err == nil {
			continue
		}

		excludeNVIDIAImage(path)
		m.record(NVIDIAMissingReason)
	}
}

func (m nvidiaExtensions) selectImage(ctx context.Context, preferred string, pinned bool) (string, []string, string) {
	link := filepath.Join(m.links, "nvidia.raw")
	versions := []string{}

	entries, _ := os.ReadDir(m.images)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		_, err := os.Stat(filepath.Join(m.images, entry.Name(), "nvidia.raw"))
		if err == nil {
			versions = append(versions, entry.Name())
		}
	}

	slices.Sort(versions)

	target, _ := os.Readlink(link)
	current := filepath.Base(filepath.Dir(target))
	candidates := []string{preferred, current}

	if !pinned {
		for _, version := range slices.Backward(versions) {
			candidates = append(candidates, version)
		}
	}

	for _, version := range candidates {
		if version == "" || version == "." || !slices.Contains(versions, version) {
			continue
		}

		image := filepath.Join(m.images, version, "nvidia.raw")

		err := m.validate(ctx, image)
		if err != nil {
			continue
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

		m.record("")

		return version, versions, ""
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
