package systemd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lxc/incus/v7/shared/subprocess"
)

// ValidateNVIDIAExtensions refuses kernel-specific GPU images before any sysext merge.
func ValidateNVIDIAExtensions(ctx context.Context) error {
	entries, err := os.ReadDir(SystemExtensionsPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.Name() != "nvidia.raw" && !strings.HasPrefix(entry.Name(), "nvidia-") {
			continue
		}

		err := ValidateNVIDIAExtension(ctx, filepath.Join(SystemExtensionsPath, entry.Name()))
		if err != nil {
			return err
		}
	}

	return nil
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

func validateNVIDIAKernel(kernel string, running string) error {
	if strings.TrimSpace(kernel) == "" || strings.TrimSpace(kernel) != strings.TrimSpace(running) {
		return fmt.Errorf("NVIDIA sysext kernel %q does not match running kernel %q", strings.TrimSpace(kernel), strings.TrimSpace(running))
	}

	return nil
}
