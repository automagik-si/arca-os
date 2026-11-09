package api

// ApplicationNVIDIAConfig selects the host driver and per-card uses, applied on reboot.
type ApplicationNVIDIAConfig struct {
	ApplicationConfig `yaml:",inline"`

	Driver string            `json:"driver"          yaml:"driver"`
	Cards  map[string]string `json:"cards,omitempty" yaml:"cards,omitempty"`
}

// ApplicationNVIDIAState records health failures for each OS release and driver version.
type ApplicationNVIDIAState struct {
	ApplicationState `yaml:",inline"`

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
