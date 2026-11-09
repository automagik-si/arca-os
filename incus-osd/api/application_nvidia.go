package api

// ApplicationNVIDIAConfig selects the host driver and per-card uses.
type ApplicationNVIDIAConfig struct {
	ApplicationConfig `yaml:",inline"`

	ResetBadVersions bool `json:"reset_bad_versions,omitempty" yaml:"reset_bad_versions,omitempty"`

	Driver string            `json:"driver"          yaml:"driver"`
	Cards  map[string]string `json:"cards,omitempty" yaml:"cards,omitempty"`
}

// ApplicationNVIDIABuild identifies an offline driver pack.
type ApplicationNVIDIABuild struct {
	Branch  string `json:"branch"  yaml:"branch"`
	Version string `json:"version" yaml:"version"`
}

// ApplicationNVIDIARecovery explains a boot-time return to a previous build.
type ApplicationNVIDIARecovery struct {
	Build  ApplicationNVIDIABuild `json:"build"  yaml:"build"`
	Reason string                 `json:"reason" yaml:"reason"`
}

// ApplicationNVIDIAState records health failures for each OS release and driver version.
type ApplicationNVIDIAState struct {
	ApplicationState `yaml:",inline"`

	PendingAttempt string `json:"pending_attempt,omitempty"  yaml:"pending_attempt,omitempty"`
	AttemptBootID  string `json:"attempt_boot_id,omitempty"  yaml:"attempt_boot_id,omitempty"`
	FallbackBootID string `json:"fallback_boot_id,omitempty" yaml:"fallback_boot_id,omitempty"`
	PendingHealth  string `json:"pending_health,omitempty"   yaml:"pending_health,omitempty"`
	HealthBootID   string `json:"health_boot_id,omitempty"   yaml:"health_boot_id,omitempty"`

	// AttemptBuild is the build the pending startup or health markers refer to.
	AttemptBuild *ApplicationNVIDIABuild    `json:"attempt_build,omitempty" yaml:"attempt_build,omitempty"`
	Recovery     *ApplicationNVIDIARecovery `json:"recovery,omitempty"      yaml:"recovery,omitempty"`

	RequestedBranch string                  `json:"requested_branch,omitempty" yaml:"requested_branch,omitempty"`
	ActiveBranch    string                  `json:"active_branch,omitempty"    yaml:"active_branch,omitempty"`
	Previous        *ApplicationNVIDIABuild `json:"previous,omitempty"         yaml:"previous,omitempty"`
	NeedsReboot     bool                    `json:"needs_reboot"               yaml:"needs_reboot"`
	BadBuilds       map[string]string       `json:"bad_builds,omitempty"       yaml:"bad_builds,omitempty"`

	ActiveExtensionVersion string `json:"active_extension_version,omitempty" yaml:"active_extension_version,omitempty"`

	DriverVersion string            `json:"driver_version"         yaml:"driver_version"`
	ActiveDriver  string            `json:"active_driver"          yaml:"active_driver"`
	Reason        string            `json:"reason"                 yaml:"reason"`
	BadVersions   map[string]string `json:"bad_versions,omitempty" yaml:"bad_versions,omitempty"`
}

// ApplicationNVIDIA represents the shared state of NVIDIA application variants.
type ApplicationNVIDIA struct {
	Config ApplicationNVIDIAConfig `json:"config" yaml:"config"`
	State  ApplicationNVIDIAState  `json:"state"  yaml:"state"`
}
