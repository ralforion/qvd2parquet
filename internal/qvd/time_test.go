package qvd

import (
	"testing"
	"time"
)

func TestQlikDaysToDate32(t *testing.T) {
	tests := []struct {
		serial float64
		want   int32
	}{
		{25569, 0},       // 1970-01-01
		{25570, 1},       // 1970-01-02
		{25568, -1},      // 1969-12-31
		{45000, 19431},   // 2023-03-15
		{25569.99, 0},    // fraction discarded
		{44927.5, 19358}, // 2023-01-01 midday
	}
	for _, tc := range tests {
		got, ok := QlikDaysToDate32(tc.serial)
		if !ok {
			t.Fatalf("QlikDaysToDate32(%v) not ok", tc.serial)
		}
		if got != tc.want {
			t.Errorf("QlikDaysToDate32(%v) = %d, want %d", tc.serial, got, tc.want)
		}
	}
	if _, ok := QlikDaysToDate32(1e30); ok {
		t.Error("out-of-range serial should not convert")
	}
}

func TestQlikDaysToTimestampMicrosUTC(t *testing.T) {
	tests := []struct {
		serial float64
		want   string
	}{
		{25569, "1970-01-01T00:00:00Z"},
		{25569.5, "1970-01-01T12:00:00Z"},
		{45000.25, "2023-03-15T06:00:00Z"},
	}
	for _, tc := range tests {
		us, ok := QlikDaysToTimestampMicros(tc.serial, time.UTC)
		if !ok {
			t.Fatalf("QlikDaysToTimestampMicros(%v) not ok", tc.serial)
		}
		if got := time.UnixMicro(us).UTC().Format(time.RFC3339); got != tc.want {
			t.Errorf("QlikDaysToTimestampMicros(%v) = %s, want %s", tc.serial, got, tc.want)
		}
	}
}

// A serial value is a wall-clock reading, so in a non-UTC zone it must map to
// the instant at which that zone shows those clock digits.
func TestQlikDaysToTimestampMicrosZoned(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	// 2023-07-01 12:00 serial: 2023-07-01 is day 45108.
	serial := 45108 + 0.5
	us, ok := QlikDaysToTimestampMicros(serial, berlin)
	if !ok {
		t.Fatal("conversion failed")
	}
	got := time.UnixMicro(us).In(berlin)
	if got.Hour() != 12 || got.Day() != 1 || got.Month() != time.July || got.Year() != 2023 {
		t.Errorf("got %s, want 2023-07-01 12:00 local Berlin time", got)
	}
	// CEST is UTC+2 in July.
	if utc := time.UnixMicro(us).UTC(); utc.Hour() != 10 {
		t.Errorf("UTC hour = %d, want 10", utc.Hour())
	}
}

func TestQlikFractionToTimeMillis(t *testing.T) {
	tests := []struct {
		v    float64
		want int32
	}{
		{0, 0},
		{0.5, 43200000},      // 12:00:00
		{0.25, 21600000},     // 06:00:00
		{45000.75, 64800000}, // 18:00:00, whole days ignored
		{1.0, 0},             // exactly midnight
	}
	for _, tc := range tests {
		got, ok := QlikFractionToTimeMillis(tc.v)
		if !ok {
			t.Fatalf("QlikFractionToTimeMillis(%v) not ok", tc.v)
		}
		if got != tc.want {
			t.Errorf("QlikFractionToTimeMillis(%v) = %d, want %d", tc.v, got, tc.want)
		}
	}
}

func TestParseLocation(t *testing.T) {
	if loc, naive, err := ParseLocation("UTC"); err != nil || loc != time.UTC || naive {
		t.Errorf("UTC -> %v, %v", loc, err)
	}
	if loc, naive, err := ParseLocation(""); err != nil || loc != time.Local || naive {
		t.Errorf("empty -> %v, %v", loc, err)
	}
	if loc, naive, err := ParseLocation("Local"); err != nil || loc != time.Local || naive {
		t.Errorf("Local -> %v, %v", loc, err)
	}
	if _, _, err := ParseLocation("Mars/Olympus"); err == nil {
		t.Error("expected an error for an unknown timezone")
	}
}

func TestTimestampZoneAnomaly(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	tests := []struct {
		serial float64
		loc    *time.Location
		want   ZoneAnomaly
	}{
		{45011.1041666667, berlin, ZoneRelocated}, // 2023-03-26 02:30, skipped
		{45228.1041666667, berlin, ZoneAmbiguous}, // 2023-10-29 02:30, repeated
		{45000.5, berlin, ZoneOK},                 // an ordinary midday
		{45011.1041666667, time.UTC, ZoneOK},      // UTC never shifts
		{45011.1041666667, nil, ZoneOK},           // naive never shifts
	}
	for _, tc := range tests {
		if got := TimestampZoneAnomaly(tc.serial, tc.loc); got != tc.want {
			t.Errorf("TimestampZoneAnomaly(%v, %v) = %v, want %v", tc.serial, tc.loc, got, tc.want)
		}
	}
}
