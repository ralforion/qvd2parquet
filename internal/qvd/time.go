package qvd

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// QlikEpochOffset is the Qlik/spreadsheet serial day number of 1970-01-01.
const QlikEpochOffset = 25569

const (
	millisPerDay = 86400000
	microsPerDay = 86400000000
)

// ParseLocation resolves the --timezone flag value. The second result reports
// whether timestamps should be written with no timezone at all, which is what
// a QVD actually holds: a naive wall-clock reading that names no zone.
func ParseLocation(name string) (*time.Location, bool, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "none", "naive":
		// Reinterpreting a wall clock in UTC is the identity mapping, so UTC
		// is the location to compute with; the output type carries no zone.
		return time.UTC, true, nil
	case "", "local":
		return time.Local, false, nil
	case "utc":
		return time.UTC, false, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, false, fmt.Errorf("unknown timezone %q: %w", name, err)
	}
	return loc, false, nil
}

// QlikDaysToDate32 converts a Qlik serial day number to days since the Unix
// epoch. The fractional part is discarded, so the result is timezone
// independent.
func QlikDaysToDate32(v float64) (int32, bool) {
	d := math.Floor(v) - QlikEpochOffset
	if math.IsNaN(d) || d < math.MinInt32 || d > math.MaxInt32 {
		return 0, false
	}
	return int32(d), true
}

// QlikDaysToTimestampMicros converts a Qlik serial timestamp to microseconds
// since the Unix epoch, interpreting the serial value as wall-clock time in
// loc.
//
// Microseconds are the finest unit a Qlik serial can carry any signal in: one
// float64 ulp is about 0.63us at present-day serials, so the value simply does
// not resolve below that. Rounding to the microsecond therefore removes the
// encoding noise (measured at up to 210ns on real Qlik output, which is what
// makes a stored 07:15:00 read back as 07:14:59.999999) without discarding
// anything the source could have expressed.
func QlikDaysToTimestampMicros(v float64, loc *time.Location) (int64, bool) {
	wall := (v - QlikEpochOffset) * microsPerDay
	if math.IsNaN(wall) || math.IsInf(wall, 0) || math.Abs(wall) > 1e18 {
		return 0, false
	}
	us := int64(math.Round(wall))
	if loc == nil || loc == time.UTC {
		return us, true
	}
	// Reinterpret the wall clock in loc rather than in UTC.
	t := time.UnixMicro(us).UTC()
	local := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc)
	return local.UnixMicro(), true
}

// QlikFractionToTimeMillis converts a Qlik time value (a fraction of one day)
// to milliseconds since midnight.
func QlikFractionToTimeMillis(v float64) (int32, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	_, frac := math.Modf(v)
	if frac < 0 {
		frac += 1
	}
	ms := int64(math.Round(frac * millisPerDay))
	// Rounding can push 23:59:59.9995 onto the next day.
	if ms >= millisPerDay {
		ms -= millisPerDay
	}
	return int32(ms), true
}
