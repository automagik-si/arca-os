package applications

import (
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
