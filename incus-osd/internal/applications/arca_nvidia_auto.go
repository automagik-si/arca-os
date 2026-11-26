package applications

import (
	"context"
	"slices"

	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// RememberOwnerRemoval records that the owner removed the given application, and saves the state when it did.
func RememberOwnerRemoval(s *state.State, name string) error {
	if !markOwnerRemoval(s, name) {
		return nil
	}

	return s.Save()
}

// markOwnerRemoval keeps the NVIDIA driver from being installed again by itself; it reports whether it changed anything.
func markOwnerRemoval(s *state.State, name string) bool {
	if name != nvidiaStable && name != nvidiaLTS {
		return false
	}

	s.Applications.NVIDIA.State.Removed = true

	return true
}

// MissingNVIDIA returns the NVIDIA driver and the dependencies it lacks when the system should get it by itself and has not yet.
func MissingNVIDIA(ctx context.Context, s *state.State) ([]string, error) {
	names, err := GetInstallApplications(ctx, s)
	if err != nil {
		return nil, err
	}

	missing := []string{}

	for _, name := range names {
		if name != nvidiaStable && name != "gpu-support" {
			continue
		}

		app, err := Load(ctx, s, name)
		if err != nil {
			return nil, err
		}

		if app.IsInstalled() {
			continue
		}

		missing = append(missing, name)
	}

	if !slices.Contains(missing, nvidiaStable) {
		return nil, nil
	}

	return missing, nil
}
