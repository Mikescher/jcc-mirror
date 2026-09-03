package schedule

import (
	"testing"
	"time"
)

// berlin is the zone the schedule is meant to be read in, and the one whose
// clock changes twice a year - which is the case the boundary arithmetic exists
// for.
func berlin(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	return loc
}

func at(t *testing.T, s string, day time.Weekday, hour int) Window {
	t.Helper()
	sched, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	// 2026-06-01 is a Monday, so the offset from it lands on the wanted weekday.
	base := time.Date(2026, 6, 1, hour, 30, 0, 0, time.UTC)
	offset := (int(day) - int(time.Monday) + 7) % 7
	return sched.At(base.AddDate(0, 0, offset))
}

// TestUnsetIsAlwaysOpen is the property an install that has never been given a
// schedule depends on: nothing is gated until something is written.
func TestUnsetIsAlwaysOpen(t *testing.T) {
	var zero Schedule
	w := zero.At(time.Now())
	if !w.Open || w.Limit != 0 {
		t.Fatalf("the zero schedule = %+v, want open and uncapped", w)
	}

	parsed, err := Parse("   ")
	if err != nil {
		t.Fatalf("Parse(blank): %v", err)
	}
	if !parsed.Equal(zero) {
		t.Fatalf("Parse(blank) = %q, want the zero schedule", parsed)
	}
}

// TestTheDesignsExample is the one DESIGN.md §6 quotes: "unlimited 02:00-08:00,
// 5 MB/s otherwise", which the grid has to give for free.
func TestTheDesignsExample(t *testing.T) {
	const s = "* * = 5MiB; * 2-8 = full"

	night := at(t, s, time.Wednesday, 3)
	if !night.Open || night.Limit != 0 {
		t.Errorf("03:00 = %+v, want open and uncapped", night)
	}

	day := at(t, s, time.Wednesday, 14)
	if !day.Open || day.Limit != 5<<20 {
		t.Errorf("14:00 = %+v, want open at 5MiB/s", day)
	}
}

func TestParseCases(t *testing.T) {
	tests := []struct {
		schedule string
		day      time.Weekday
		hour     int
		open     bool
		limit    int64
	}{
		{"mon-fri 8-18 = off", time.Monday, 9, false, 0},
		{"mon-fri 8-18 = off", time.Monday, 19, true, 0},
		{"mon-fri 8-18 = off", time.Saturday, 9, true, 0},
		{"sat-sun * = off", time.Sunday, 4, false, 0},
		{"* 22-2 = full; * 2-22 = 1MiB", time.Tuesday, 23, true, 0},
		{"* 22-2 = full; * 2-22 = 1MiB", time.Tuesday, 1, true, 0},
		{"* 22-2 = full; * 2-22 = 1MiB", time.Tuesday, 12, true, 1 << 20},
		{"fri-mon * = off", time.Sunday, 12, false, 0},
		{"fri-mon * = off", time.Wednesday, 12, true, 0},
		{"* 13 = 500k", time.Thursday, 13, true, 500 << 10},
		{"* 08:00-12:00 = off", time.Thursday, 9, false, 0},
		{"* * = 0", time.Thursday, 9, false, 0},
		{"monday * = off", time.Monday, 9, false, 0},
		{"* * = 5MiB/s", time.Monday, 9, true, 5 << 20},
	}

	for _, tc := range tests {
		w := at(t, tc.schedule, tc.day, tc.hour)
		if w.Open != tc.open || w.Limit != tc.limit {
			t.Errorf("%q at %s %02d:00 = %+v, want open=%v limit=%d",
				tc.schedule, tc.day, tc.hour, w, tc.open, tc.limit)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, s := range []string{
		"mon 8-18",          // no cap
		"8-18 = off",        // no days
		"funday * = off",    // not a day
		"* 25-26 = off",     // not an hour
		"* 8-8 = off",       // an empty span
		"* * = fastish",     // not a rate
		"mon * = off; oops", // the second rule is not a rule
	} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", s)
		}
	}
}

// TestGateRefusesARate is why the scan schedule parses through ParseGate: a cap
// on a walk of metadata means nothing, and silently ignoring it would leave
// someone believing their scans were throttled.
func TestGateRefusesARate(t *testing.T) {
	if _, err := ParseGate("* 2-4 = 5MiB"); err == nil {
		t.Fatal("ParseGate accepted a rate")
	}
	if _, err := ParseGate("* 2-4 = on; * 4-24 = off"); err != nil {
		t.Fatalf("ParseGate(on/off): %v", err)
	}
}

// TestUntilIsTheBoundaryNotTheHour is what keeps the scheduler asleep: six hours
// of the same cap are one window, not six.
func TestUntilIsTheBoundaryNotTheHour(t *testing.T) {
	loc := berlin(t)
	s, err := Parse("* * = 5MiB; * 2-8 = full")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	w := s.At(time.Date(2026, 6, 3, 3, 15, 0, 0, loc))
	want := time.Date(2026, 6, 3, 8, 0, 0, 0, loc)
	if !w.Until.Equal(want) {
		t.Errorf("Until = %s, want %s", w.Until, want)
	}

	// A grid that never changes still has to answer, and a week ahead is the
	// furthest the question can be asked.
	uniform, _ := Parse("* * = full")
	if u := uniform.At(time.Date(2026, 6, 3, 3, 15, 0, 0, loc)); !u.Until.After(time.Date(2026, 6, 10, 3, 0, 0, 0, loc)) {
		t.Errorf("a uniform schedule ends at %s, want a week out", u.Until)
	}
}

// TestBoundariesSurviveTheClockChange is the reason hourAfter does calendar
// arithmetic: on the day the clocks go forward the local day has 23 hours, and a
// window that ended at 03:00 must still end at 03:00.
func TestBoundariesSurviveTheClockChange(t *testing.T) {
	loc := berlin(t)
	s, err := Parse("* * = off; * 0-3 = full")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// 2026-03-29 is the Sunday the clocks go forward in Europe/Berlin: 02:00
	// becomes 03:00 and the 02:00 hour does not exist.
	w := s.At(time.Date(2026, 3, 29, 1, 30, 0, 0, loc))
	if !w.Open {
		t.Fatalf("01:30 on the switch day = %+v, want open", w)
	}
	if got := w.Until.In(loc).Hour(); got != 3 {
		t.Errorf("the window ends at %s (hour %d), want 03:00", w.Until.In(loc), got)
	}

	// And back again in October, where 02:00 happens twice: the walk forward must
	// terminate rather than sit on the repeated hour.
	autumn := s.At(time.Date(2026, 10, 25, 1, 30, 0, 0, loc))
	if !autumn.Until.After(time.Date(2026, 10, 25, 1, 30, 0, 0, loc)) {
		t.Errorf("Until = %s, want a time after the start", autumn.Until)
	}
}

func TestNextOpen(t *testing.T) {
	loc := berlin(t)
	s, err := Parse("* * = off; * 2-8 = full")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	next, ok := s.NextOpen(time.Date(2026, 6, 3, 20, 0, 0, 0, loc))
	if !ok {
		t.Fatal("NextOpen said never")
	}
	want := time.Date(2026, 6, 4, 2, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Errorf("NextOpen = %s, want %s", next, want)
	}

	shut, _ := Parse("* * = off")
	if _, ok := shut.NextOpen(time.Now()); ok {
		t.Error("a schedule that is closed all week reported a next open window")
	}
}

// TestStringIsCanonical is what the dashboard writes back and the CLI prints:
// two schedules that mean the same thing have to render identically, or every
// save rewrites the setting and fills the audit trail with noise.
func TestStringIsCanonical(t *testing.T) {
	tests := map[string]string{
		"":                                    "* * = full",
		"* * = full":                          "* * = full",
		"* * = 5MiB; * 2-8 = full":            "* 0-2,8-24 = 5MiB; * 2-8 = full",
		"mon-fri 8-18 = off":                  "mon-fri 0-8,18-24 = full; mon-fri 8-18 = off; sat-sun * = full",
		"sun * = off; mon * = off; tue * = 0": "mon-tue,sun * = off; wed-sat * = full",
	}

	for in, want := range tests {
		s, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if got := s.String(); got != want {
			t.Errorf("Parse(%q).String() = %q, want %q", in, got, want)
			continue
		}
		back, err := Parse(want)
		if err != nil {
			t.Errorf("Parse(%q): %v", want, err)
			continue
		}
		if !back.Equal(s) {
			t.Errorf("%q did not round-trip through %q", in, want)
		}
	}
}
