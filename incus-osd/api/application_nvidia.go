package api

// ApplicationNVIDIAConfig selects the host driver and per-card uses, applied on reboot.
type ApplicationNVIDIAConfig struct {
	ApplicationConfig `yaml:",inline"`

	ResetBadVersions bool `json:"reset_bad_versions,omitempty" yaml:"reset_bad_versions,omitempty"`

	Driver string            `json:"driver"          yaml:"driver"`
	Cards  map[string]string `json:"cards,omitempty" yaml:"cards,omitempty"`
}

// ApplicationNVIDIAState records health failures for each OS release and driver version.
type ApplicationNVIDIAState struct {
	ApplicationState `yaml:",inline"`

	PendingAttempt string `json:"pending_attempt,omitempty"  yaml:"pending_attempt,omitempty"`
	AttemptBootID  string `json:"attempt_boot_id,omitempty"  yaml:"attempt_boot_id,omitempty"`
	FallbackBootID string `json:"fallback_boot_id,omitempty" yaml:"fallback_boot_id,omitempty"`
	PendingHealth  string `json:"pending_health,omitempty"   yaml:"pending_health,omitempty"`
	HealthBootID   string `json:"health_boot_id,omitempty"   yaml:"health_boot_id,omitempty"`

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
