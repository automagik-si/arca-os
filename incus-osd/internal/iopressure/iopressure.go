package iopressure

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
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
func Parse(data string) (Sample, error) {
	var s Sample

	hasSome := false

	for line := range strings.Lines(data) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		// Skip lines later kernels may add.
		if fields[0] != "some" && fields[0] != "full" {
			continue
		}

		avg10, avg60, err := parseAverages(fields[1:])
		if err != nil {
			return Sample{}, fmt.Errorf("bad %q pressure line: %w", fields[0], err)
		}

		if fields[0] == "some" {
			s.SomeAvg10, s.SomeAvg60, hasSome = avg10, avg60, true
		} else {
			s.FullAvg10, s.FullAvg60, s.HasFull = avg10, avg60, true
		}
	}

	if !hasSome {
		return Sample{}, errors.New(`no "some" pressure line`)
	}

	return s, nil
}

// parseAverages returns the avg10 and avg60 values of a pressure line's "key=value" fields.
func parseAverages(fields []string) (float64, float64, error) {
	values := map[string]float64{}

	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return 0, 0, fmt.Errorf("field %q isn't key=value", field)
		}

		if key != "avg10" && key != "avg60" {
			continue
		}

		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, 0, err
		}

		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, 0, fmt.Errorf("%s=%s is out of range", key, value)
		}

		values[key] = v
	}

	avg10, ok10 := values["avg10"]
	avg60, ok60 := values["avg60"]

	if !ok10 || !ok60 {
		return 0, 0, errors.New("missing avg10 or avg60")
	}

	return avg10, avg60, nil
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
type Watcher struct {
	paused    bool
	highSince time.Time
	lowSince  time.Time
}

// Observe feeds one sample taken at now and returns what to do, with a reason for a pause or resume.
func (w *Watcher) Observe(now time.Time, s Sample) (Decision, string) {
	if w.paused {
		return w.observePaused(now, s)
	}

	reason := ""

	if s.HasFull && s.FullAvg10 >= PauseFullAvg10 {
		if w.highSince.IsZero() {
			w.highSince = now
		}

		if now.Sub(w.highSince) >= PauseFullFor {
			reason = fmt.Sprintf("io pressure: full avg10 %.1f", s.FullAvg10)
		}
	} else {
		w.highSince = time.Time{}
	}

	if reason == "" && s.SomeAvg60 >= PauseSomeAvg60 {
		reason = fmt.Sprintf("io pressure: some avg60 %.1f", s.SomeAvg60)
	}

	if reason == "" {
		return Hold, ""
	}

	w.Set(true)

	return Pause, reason
}

// Set forces the paused state, for when acting on a decision failed, and restarts both streaks.
func (w *Watcher) Set(paused bool) {
	w.paused = paused
	w.highSince = time.Time{}
	w.lowSince = time.Time{}
}

func (w *Watcher) observePaused(now time.Time, s Sample) (Decision, string) {
	low := s.SomeAvg60 < ResumeSomeAvg60 && (!s.HasFull || s.FullAvg10 < ResumeFullAvg10)
	if !low {
		w.lowSince = time.Time{}

		return Hold, ""
	}

	if w.lowSince.IsZero() {
		w.lowSince = now
	}

	if now.Sub(w.lowSince) < ResumeLowFor {
		return Hold, ""
	}

	w.Set(false)

	if !s.HasFull {
		return Resume, fmt.Sprintf("io pressure low for %s: some avg60 %.1f", ResumeLowFor, s.SomeAvg60)
	}

	return Resume, fmt.Sprintf("io pressure low for %s: full avg10 %.1f, some avg60 %.1f", ResumeLowFor, s.FullAvg10, s.SomeAvg60)
}
