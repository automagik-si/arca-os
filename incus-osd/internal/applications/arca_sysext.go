package applications

import (
	"slices"

	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// pickApplicationVersion adjusts the picked application version for the automatic updater. After
// an automatic OS rollback the version that ran before it stays, as going back could meet a newer
// database. A version that failed its health check is never picked while another one is on disk.
func pickApplicationVersion(s *state.State, name string, version *availableApplicationVersions, best string) string {
	s.LockUpdateHealth()
	defer s.UnlockUpdateHealth()

	h := &s.UpdateHealth
	failed := func(v string) bool {
		_, ok := h.FailedApplications[name+":"+v]

		return ok
	}

	kept := h.KeptApplications[name]
	if kept != "" && !failed(kept) && sysextImageExists(name, kept) {
		return kept
	}

	if best == "" || !failed(best) {
		return best
	}

	candidates := []string{version.appVersion, version.currentOSVersion, version.otherOSVersion, version.latestDiskVersion, h.PreviousApplications[name]}
	slices.Sort(candidates)
	slices.Reverse(candidates)

	for _, v := range candidates {
		if v != "" && !failed(v) && sysextImageExists(name, v) {
			return v
		}
	}

	return best
}

// keepApplicationVersions adds the versions the automatic updater may go back to, so their images stay on disk.
func keepApplicationVersions(s *state.State, name string, versions []string) []string {
	s.LockUpdateHealth()
	defer s.UnlockUpdateHealth()

	for _, v := range []string{s.UpdateHealth.PreviousApplications[name], s.UpdateHealth.KeptApplications[name]} {
		if v != "" && sysextImageExists(name, v) {
			versions = append(versions, v)
		}
	}

	return versions
}
