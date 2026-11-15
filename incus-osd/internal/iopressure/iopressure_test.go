package iopressure

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		data    string
		want    Sample
		wantErr bool
	}{
		{
			name: "Some and full",
			data: "some avg10=12.50 avg60=40.25 avg300=3.00 total=123456\nfull avg10=23.40 avg60=8.10 avg300=1.00 total=654321\n",
			want: Sample{SomeAvg10: 12.5, SomeAvg60: 40.25, FullAvg10: 23.4, FullAvg60: 8.1, HasFull: true},
		},
		{
			name: "Older kernel without a full line",
			data: "some avg10=1.00 avg60=2.00 avg300=3.00 total=4\n",
			want: Sample{SomeAvg10: 1, SomeAvg60: 2},
		},
		{
			name:    "Empty",
			data:    "",
			wantErr: true,
		},
		{
			name:    "Garbage",
			data:    "hello world\n",
			wantErr: true,
		},
		{
			name:    "Full without some",
			data:    "full avg10=1.00 avg60=2.00 avg300=3.00 total=4\n",
			wantErr: true,
		},
		{
			name:    "Bad number",
			data:    "some avg10=abc avg60=2.00 avg300=3.00 total=4\n",
			wantErr: true,
		},
		{
			name:    "Missing avg60",
			data:    "some avg10=1.00 avg300=3.00 total=4\n",
			wantErr: true,
		},
		{
			name:    "Negative average",
			data:    "some avg10=-1.00 avg60=2.00 avg300=3.00 total=4\n",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Parse(tc.data)
			if tc.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

var (
	quiet    = Sample{SomeAvg10: 1, SomeAvg60: 3, FullAvg10: 0.5, FullAvg60: 0.2, HasFull: true}
	fullHigh = Sample{SomeAvg10: 30, SomeAvg60: 20, FullAvg10: 23.4, FullAvg60: 12, HasFull: true}
	someHigh = Sample{SomeAvg10: 50, SomeAvg60: 45, FullAvg10: 1, FullAvg60: 1, HasFull: true}
	middling = Sample{SomeAvg10: 8, SomeAvg60: 12, FullAvg10: 5, FullAvg60: 3, HasFull: true}
)

// feed observes samples one Tick apart, starting at start, and returns the decisions.
func feed(w *Watcher, start time.Time, samples ...Sample) []Decision {
	decisions := make([]Decision, 0, len(samples))

	for i, s := range samples {
		d, _ := w.Observe(start.Add(time.Duration(i)*Tick), s)
		decisions = append(decisions, d)
	}

	return decisions
}

func repeat(s Sample, n int) []Sample {
	samples := make([]Sample, 0, n)
	for range n {
		samples = append(samples, s)
	}

	return samples
}

func count(decisions []Decision, want Decision) int {
	n := 0

	for _, d := range decisions {
		if d == want {
			n++
		}
	}

	return n
}

func TestWatcherShortSpikeDoesNotPause(t *testing.T) {
	t.Parallel()

	w := &Watcher{}

	// 50s of high "full" pressure, then quiet: one tick short of the 60s needed.
	samples := append(repeat(fullHigh, 6), repeat(quiet, 3)...)
	samples = append(samples, repeat(fullHigh, 6)...)

	require.Zero(t, count(feed(w, time.Unix(1000, 0), samples...), Pause))
}

func TestWatcherSustainedPressurePausesOnce(t *testing.T) {
	t.Parallel()

	w := &Watcher{}
	start := time.Unix(1000, 0)

	decisions := feed(w, start, repeat(fullHigh, 30)...)
	require.Equal(t, 1, count(decisions, Pause))
	require.Equal(t, Pause, decisions[6], "pause once full avg10 has stayed high for 60s")

	w = &Watcher{}
	d, reason := w.Observe(start, fullHigh)
	require.Equal(t, Hold, d)
	require.Empty(t, reason)

	d, reason = w.Observe(start.Add(PauseFullFor), fullHigh)
	require.Equal(t, Pause, d)
	require.Equal(t, "io pressure: full avg10 23.4", reason)
}

func TestWatcherSomeAvg60PausesAtOnce(t *testing.T) {
	t.Parallel()

	w := &Watcher{}

	d, reason := w.Observe(time.Unix(1000, 0), someHigh)
	require.Equal(t, Pause, d)
	require.Equal(t, "io pressure: some avg60 45.0", reason)
}

func TestWatcherOlderKernelUsesSomeOnly(t *testing.T) {
	t.Parallel()

	w := &Watcher{}
	start := time.Unix(1000, 0)

	d, _ := w.Observe(start, Sample{SomeAvg60: 41})
	require.Equal(t, Pause, d)

	decisions := feed(w, start.Add(Tick), repeat(Sample{SomeAvg60: 3}, 31)...)
	require.Equal(t, Resume, decisions[30])
}

func TestWatcherFlappingDoesNotThrash(t *testing.T) {
	t.Parallel()

	w := &Watcher{}
	start := time.Unix(1000, 0)

	// High and quiet every other tick never builds 60s of high pressure.
	flapping := make([]Sample, 0, 120)
	for range 60 {
		flapping = append(flapping, fullHigh, quiet)
	}

	require.Zero(t, count(feed(w, start, flapping...), Pause))

	// Once paused, the same flapping never builds 5 minutes of quiet.
	samples := append(repeat(fullHigh, 7), flapping...)
	decisions := feed(w, start.Add(time.Hour), samples...)
	require.Equal(t, 1, count(decisions, Pause))
	require.Zero(t, count(decisions, Resume))
}

func TestWatcherResumeNeedsFiveMinutesLow(t *testing.T) {
	t.Parallel()

	w := &Watcher{}
	start := time.Unix(1000, 0)

	d, _ := w.Observe(start, someHigh)
	require.Equal(t, Pause, d)

	// 4m50s of quiet, then one middling sample restarts the count.
	quietStart := start.Add(Tick)
	decisions := feed(w, quietStart, append(repeat(quiet, 30), middling)...)
	require.Zero(t, count(decisions, Resume))

	// Quiet again from here: no resume until the full 5 minutes have passed.
	again := quietStart.Add(31 * Tick)
	decisions = feed(w, again, repeat(quiet, 31)...)
	require.Zero(t, count(decisions[:30], Resume))
	require.Equal(t, Resume, decisions[30])

	_, reason := w.Observe(again.Add(ResumeLowFor+Tick), quiet)
	require.Empty(t, reason, "nothing to resume once running again")
}

func TestWatcherSetRestartsStreaks(t *testing.T) {
	t.Parallel()

	w := &Watcher{}
	start := time.Unix(1000, 0)

	d, _ := w.Observe(start, someHigh)
	require.Equal(t, Pause, d)

	// The pause failed: the next sustained pressure pauses again.
	w.Set(false)

	decisions := feed(w, start.Add(Tick), repeat(fullHigh, 7)...)
	require.Equal(t, Pause, decisions[6])
}
