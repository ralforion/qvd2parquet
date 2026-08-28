package convert

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// progressETA renders a progress line for a phase whose total is known before
// it starts. The row total comes from the QVD header, so the share done and
// the time left are arithmetic on numbers already in hand, and on a table that
// takes a quarter of an hour they are the difference between watching a
// counter and knowing whether to wait.
type progressETA struct {
	total int64
	start time.Time
	// lastAt and lastRows are the previous observation, so each report can be
	// costed on its own interval.
	lastAt   time.Time
	lastRows int64
	// rate is an exponentially weighted rows per second, used only for the
	// estimate. The rate a run settles at differs from its average, because
	// the first batches carry the cost of starting up, and a projection from
	// the average stays pessimistic long after the run has found its speed.
	rate float64
}

// etaSmoothing weights the newest interval against the accumulated rate. Low
// enough that one slow batch does not swing the estimate, high enough that a
// genuine change in speed reaches it within a few reports.
const etaSmoothing = 0.4

func newProgressETA(total int64, start time.Time) *progressETA {
	return &progressETA{total: total, start: start, lastAt: start}
}

// Report records a cumulative row count and renders the shared part of a
// progress line: rows, share done, elapsed, throughput, and the estimate.
//
// The throughput shown is the average since the phase started, which is what
// it has always been. The estimate is not: it comes from the recent rate, so
// the two can disagree, and while a run is speeding up the estimate is the
// more accurate of them.
func (p *progressETA) Report(rows int64, now time.Time) string {
	p.observe(rows, now)

	elapsed := now.Sub(p.start)
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d", rows)
	if p.total > 0 {
		fmt.Fprintf(&sb, "/%d", p.total)
	}
	sb.WriteString(" rows")
	if pct := p.percent(rows); pct != "" {
		fmt.Fprintf(&sb, " (%s)", pct)
	}
	fmt.Fprintf(&sb, " in %s", elapsed.Round(time.Millisecond))

	var avg float64
	if elapsed > 0 {
		avg = float64(rows) / elapsed.Seconds()
	}
	fmt.Fprintf(&sb, " (%.0f rows/s", avg)
	if left, ok := p.remaining(rows); ok {
		if left < time.Second {
			// "about under 1s" is not English, and at this point the estimate
			// is not worth a number anyway.
			sb.WriteString(", under 1s left")
		} else {
			fmt.Fprintf(&sb, ", about %s left", shortDuration(left))
		}
	}
	sb.WriteString(")")
	return sb.String()
}

// observe folds one interval into the smoothed rate.
func (p *progressETA) observe(rows int64, now time.Time) {
	dt := now.Sub(p.lastAt).Seconds()
	drows := rows - p.lastRows
	if dt > 0 && drows > 0 {
		inst := float64(drows) / dt
		if p.rate == 0 {
			p.rate = inst
		} else {
			p.rate = etaSmoothing*inst + (1-etaSmoothing)*p.rate
		}
	}
	p.lastAt, p.lastRows = now, rows
}

// percent is the share of the total done, blank when there is no total to
// measure against.
//
// The share is floored rather than rounded, so 100% means finished. Rounding
// let 995 of 1000 rows print "100%" on a line that also said how long was
// left, which is a line contradicting itself.
func (p *progressETA) percent(rows int64) string {
	if p.total <= 0 || rows < 0 {
		return ""
	}
	pct := math.Floor(float64(rows) * 100 / float64(p.total))
	if pct > 100 {
		pct = 100
	}
	return fmt.Sprintf("%.0f%%", pct)
}

// maxProjection bounds what will be printed as an estimate. A rate low enough
// to project beyond this comes from a stalled or barely started phase, and the
// number would be noise; past roughly 292 years it would also overflow a
// time.Duration and come out negative.
const maxProjection = 48 * time.Hour

// remaining projects the time left from the recent rate. The second result is
// false when there is nothing to project from: no total, no rate yet, a phase
// that has reached its last row, or a projection too long to mean anything.
//
// The value cannot come out negative. Rows past the total return early rather
// than subtracting into a negative remainder, the rate only ever takes
// positive samples, and an absurd projection is refused above instead of
// wrapping a Duration around.
func (p *progressETA) remaining(rows int64) (time.Duration, bool) {
	if p.total <= 0 || p.rate <= 0 || rows >= p.total {
		return 0, false
	}
	seconds := float64(p.total-rows) / p.rate
	if seconds <= 0 || seconds > maxProjection.Seconds() {
		return 0, false
	}
	return time.Duration(seconds * float64(time.Second)), true
}

// shortDuration renders an estimate of a second or more at the precision it
// deserves. Go's own formatting keeps every unit, so an hour long conversion
// would report "1h2m0s", and a projection is not worth sub-second precision.
func shortDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		d = d.Round(time.Second)
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		d = d.Round(time.Minute)
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
