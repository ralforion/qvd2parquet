package convert

import (
	"strings"
	"testing"
	"time"
)

// The line has to carry the share done and a projection, since both come from
// numbers the run already has: the total is read from the QVD header before a
// single record is decoded.
func TestProgressReportsShareAndTimeLeft(t *testing.T) {
	start := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	p := newProgressETA(1_000_000, start)

	got := p.Report(250_000, start.Add(10*time.Second))
	want := "250000/1000000 rows (25%) in 10s (25000 rows/s, about 30s left)"
	if got != want {
		t.Errorf("first report =\n  %q\nwant\n  %q", got, want)
	}

	// Steady speed: the projection shrinks by the time that passed.
	got = p.Report(500_000, start.Add(20*time.Second))
	want = "500000/1000000 rows (50%) in 20s (25000 rows/s, about 20s left)"
	if got != want {
		t.Errorf("second report =\n  %q\nwant\n  %q", got, want)
	}
}

// The estimate follows the recent rate rather than the average, because a run
// carries the cost of starting up in its average long after it has found its
// speed, and a projection from the average stays pessimistic for the rest of
// the run.
func TestProgressEstimateFollowsRecentRate(t *testing.T) {
	start := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	p := newProgressETA(1_000_000, start)

	// A slow first 100k, then a steady 50000 rows/s: a report every 100k rows
	// is 2 seconds apart. The truth after 500k rows is 10 seconds left.
	p.Report(100_000, start.Add(10*time.Second))
	var line string
	for i, at := range []time.Duration{12, 14, 16, 18} {
		line = p.Report(int64(200_000+i*100_000), start.Add(at*time.Second))
	}

	// The average at that point is 27778 rows/s and would project 18 seconds,
	// nearly twice the truth, because the slow start is still in it.
	if !strings.Contains(line, "(27778 rows/s") {
		t.Errorf("the throughput shown should stay the average: %q", line)
	}
	if left := estimateSeconds(t, line); left < 10 || left > 13 {
		t.Errorf("estimate %ds should be near the true 10s, not the average's 18s: %q", left, line)
	}
}

// A phase that is nearly done is not done. Rounding the share let the last
// stretch print "100%" beside an estimate of the time still left, which is a
// line arguing with itself.
func TestProgressReachesFullOnlyWhenFinished(t *testing.T) {
	start := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	p := newProgressETA(1000, start)

	line := p.Report(995, start.Add(10*time.Second))
	if !strings.Contains(line, "(99%)") {
		t.Errorf("995 of 1000 rows should read 99%%: %q", line)
	}
	if !strings.Contains(line, "left") {
		t.Errorf("a line short of the total should still project: %q", line)
	}

	line = p.Report(1000, start.Add(11*time.Second))
	if !strings.Contains(line, "(100%)") || strings.Contains(line, "left") {
		t.Errorf("the last report should read 100%% with nothing left: %q", line)
	}
}

// The estimate must never be negative, and the way it could become one is
// overflow: a rate low enough to project past roughly 292 years wraps a
// time.Duration around into a negative number. A stalled phase produces
// exactly such a rate.
func TestProgressRefusesAnAbsurdProjection(t *testing.T) {
	start := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	p := newProgressETA(20_000_000, start)

	// One row in three hours: 20M rows at that rate is about seven thousand
	// years, which as nanoseconds does not fit an int64.
	line := p.Report(1, start.Add(3*time.Hour))
	if strings.Contains(line, "left") {
		t.Errorf("a stalled phase should project nothing: %q", line)
	}
	if !strings.Contains(line, "(0%)") {
		t.Errorf("the share should still be reported: %q", line)
	}

	// Nothing the tracker can be fed produces a negative estimate.
	for _, rows := range []int64{0, 1, 19_999_999, 20_000_000, 20_000_001} {
		if d, ok := p.remaining(rows); ok && d < 0 {
			t.Errorf("remaining(%d) = %s, which is in the past", rows, d)
		}
	}
}

// An estimate below a second is not worth a number, and "about under 1s" is
// not a sentence.
func TestProgressPhrasesASubSecondEstimate(t *testing.T) {
	start := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	p := newProgressETA(3_000_000, start)
	line := p.Report(2_818_048, start.Add(912*time.Millisecond))
	if !strings.Contains(line, ", under 1s left)") || strings.Contains(line, "about under") {
		t.Errorf("line = %q", line)
	}
}

// Nothing to project from means no projection, rather than a wrong one.
func TestProgressOmitsWhatItCannotKnow(t *testing.T) {
	start := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)

	// The last report of a phase has nothing left to wait for.
	p := newProgressETA(1000, start)
	if line := p.Report(1000, start.Add(time.Second)); strings.Contains(line, "left") {
		t.Errorf("a finished phase should not project: %q", line)
	}

	// A total of zero, which no QVD has but a caller could pass, must not
	// produce a percentage of infinity.
	p = newProgressETA(0, start)
	line := p.Report(500, start.Add(time.Second))
	if strings.Contains(line, "%") || strings.Contains(line, "left") {
		t.Errorf("without a total there is nothing to project: %q", line)
	}
	if !strings.Contains(line, "500 rows") {
		t.Errorf("the row count should still be reported: %q", line)
	}
}

func TestShortDuration(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{8 * time.Second, "8s"},
		{90 * time.Second, "1m30s"},
		{9*time.Minute + 30*time.Second, "9m30s"},
		{62 * time.Minute, "1h02m"},
		{3*time.Hour + 5*time.Minute + 40*time.Second, "3h06m"},
	} {
		if got := shortDuration(tc.in); got != tc.want {
			t.Errorf("shortDuration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// estimateSeconds pulls the projected seconds back out of a rendered line.
func estimateSeconds(t *testing.T, line string) int {
	t.Helper()
	_, after, ok := strings.Cut(line, "about ")
	if !ok {
		t.Fatalf("no estimate in %q", line)
	}
	spec, _, _ := strings.Cut(after, " left")
	d, err := time.ParseDuration(spec)
	if err != nil {
		t.Fatalf("estimate %q in %q: %v", spec, line, err)
	}
	return int(d.Seconds())
}
