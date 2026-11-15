package iopressure

import (
	"errors"
	"time"
)

const (
	// Path is where the kernel reports IO pressure.
	Path = "/proc/pressure/io"

	// Tick is how often IO pressure is sampled.
	Tick = 10 * time.Second

	// PauseFullAvg10 pauses once "full" avg10 has stayed at or above it for PauseFullFor.
	PauseFullAvg10 = 10.0
	// PauseFullFor is how long "full" avg10 must stay high before a pause.
	PauseFullFor = 60 * time.Second
	// PauseSomeAvg60 pauses as soon as "some" avg60 reaches it.
	PauseSomeAvg60 = 40.0

	// ResumeFullAvg10 is the level "full" avg10 must stay below to resume.
	ResumeFullAvg10 = 2.0
	// ResumeSomeAvg60 is the level "some" avg60 must stay below to resume.
	ResumeSomeAvg60 = 10.0
	// ResumeLowFor is how long pressure must stay low before a resume.
	ResumeLowFor = 5 * time.Minute
)

// Sample holds the averages of one read of a pressure file.
type Sample struct {
	SomeAvg10 float64
	SomeAvg60 float64
	FullAvg10 float64
	FullAvg60 float64
	// HasFull is false on kernels that don't report a "full" line.
	HasFull bool
}

// Parse reads the contents of a pressure file such as /proc/pressure/io.
func Parse(_ string) (Sample, error) {
	return Sample{}, errors.New("not implemented")
}

// Decision is what a Watcher asks of the activity it guards.
type Decision int

const (
	// Hold keeps the activity as it is.
	Hold Decision = iota
	// Pause asks to pause the running activity.
	Pause
	// Resume asks to resume the paused activity.
	Resume
)

// Watcher applies the pause and resume thresholds to a stream of samples.
type Watcher struct{}

// Observe feeds one sample taken at now and returns what to do, with a reason for a pause or resume.
func (*Watcher) Observe(_ time.Time, _ Sample) (Decision, string) {
	return Hold, ""
}

// Set forces the paused state, for when acting on a decision failed, and restarts both streaks.
func (*Watcher) Set(_ bool) {}
