package api

// SystemUpdateHealth tracks automatic OS and application updates: what is running, what was
// marked good or failed, and any update waiting for its restart or being checked.
type SystemUpdateHealth struct {
	CurrentVersion  string `json:"current_version"            yaml:"current_version"`
	PreviousVersion string `json:"previous_version,omitempty" yaml:"previous_version,omitempty"`
	GoodVersion     string `json:"good_version,omitempty"     yaml:"good_version,omitempty"`
	PendingVersion  string `json:"pending_version,omitempty"  yaml:"pending_version,omitempty"`

	// FailedVersions maps an OS version to why it failed; it is never applied again automatically.
	FailedVersions map[string]string `json:"failed_versions,omitempty" yaml:"failed_versions,omitempty"`

	// FailedApplications maps "<application>:<version>" to why that application version failed.
	FailedApplications map[string]string `json:"failed_applications,omitempty" yaml:"failed_applications,omitempty"`

	// PreviousApplications maps an application to the version it had before its last update.
	PreviousApplications map[string]string `json:"previous_applications,omitempty" yaml:"previous_applications,omitempty"`

	NextWindow string `json:"next_window,omitempty" yaml:"next_window,omitempty"`
	LastResult string `json:"last_result,omitempty" yaml:"last_result,omitempty"`
	LastReason string `json:"last_reason,omitempty" yaml:"last_reason,omitempty"`

	// NeedsOwner is set when automatic recovery stopped; automatic updates stay off until the owner turns them on.
	NeedsOwner string `json:"needs_owner,omitempty" yaml:"needs_owner,omitempty"`

	Bake *SystemUpdateBake `json:"bake,omitempty" yaml:"bake,omitempty"`

	// Instances are the "<project>/<name>" instances running before the update restart.
	Instances []string `json:"instances,omitempty" yaml:"instances,omitempty"`

	// Addresses are the IPv4 addresses the machine had before the update restart.
	Addresses []string `json:"addresses,omitempty" yaml:"addresses,omitempty"`

	// RollbackBootID is the boot an automatic rollback reboot was issued from; there is one per boot.
	RollbackBootID string `json:"rollback_boot_id,omitempty" yaml:"rollback_boot_id,omitempty"`

	// DeferUntil (RFC 3339) is the latest time a restart deferred by a busy machine may still happen.
	DeferUntil string `json:"defer_until,omitempty" yaml:"defer_until,omitempty"`
	Waiting    string `json:"waiting,omitempty"     yaml:"waiting,omitempty"`
}

// SystemUpdateBake is the health check of one OS version after it was installed.
type SystemUpdateBake struct {
	Version  string `json:"version"  yaml:"version"`
	Previous string `json:"previous" yaml:"previous"`

	// StagedBootID is the boot the version was installed from, before its restart.
	StagedBootID string `json:"staged_boot_id,omitempty" yaml:"staged_boot_id,omitempty"`

	// Started (RFC 3339) is when the version first ran; empty until then.
	Started string `json:"started,omitempty" yaml:"started,omitempty"`

	// Strikes counts failed checks, Passes passed ones; LastFailure says what failed last.
	Strikes     int    `json:"strikes,omitempty"      yaml:"strikes,omitempty"`
	Passes      int    `json:"passes,omitempty"       yaml:"passes,omitempty"`
	LastFailure string `json:"last_failure,omitempty" yaml:"last_failure,omitempty"`

	// Starts counts incus-osd starts in boot StartsBootID, to catch a daemon that never comes up.
	Starts       int    `json:"starts,omitempty"         yaml:"starts,omitempty"`
	StartsBootID string `json:"starts_boot_id,omitempty" yaml:"starts_boot_id,omitempty"`

	// Rollback is set when this checks the version an automatic rollback went back to.
	Rollback bool `json:"rollback,omitempty" yaml:"rollback,omitempty"`
}
