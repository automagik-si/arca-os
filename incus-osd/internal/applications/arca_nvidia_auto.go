package applications

import (
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// MarkOwnerRemoval keeps the NVIDIA driver from being installed again by itself; the uninstall that follows saves it.
func MarkOwnerRemoval(s *state.State, name string) bool {
	if name != nvidiaStable && name != nvidiaLTS {
		return false
	}

	s.Applications.NVIDIA.State.Removed = true

	return true
}

// updateIsSettled reports whether no OS update is being checked and the updater is not waiting for the owner.
func updateIsSettled(s *state.State) bool {
	s.LockUpdateHealth()
	defer s.UnlockUpdateHealth()

	return s.UpdateHealth.Bake == nil && s.UpdateHealth.NeedsOwner == ""
}
