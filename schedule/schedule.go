// Package schedule is the 7x24 grid that says when jcc-mirror may move bytes and
// how fast: one cell per weekday-hour, each carrying both halves of the answer.
// Keeping them in one grid is what makes "unlimited 02:00-08:00, 5 MB/s
// otherwise" a single setting rather than two that can disagree (DESIGN.md §6).
//
// The grid is evaluated in whatever location the times handed to it carry, which
// is the configured timezone rather than the container's - a schedule that
// silently shifted twice a year would be worse than no schedule at all.
package schedule

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"blackforestbytes.com/jcc-mirror/format"
)

// The shape of the grid.
const (
	Days  = 7
	Hours = 24
	Cells = Days * Hours
)

// The two cell values that are not a rate. Closed is negative so that the zero
// value of a Schedule - every cell zero - reads as "always open, no limit",
// which is what an unset schedule has to mean.
const (
	closed    int64 = -1
	unlimited int64 = 0
)

// Schedule is the grid. The zero value is always open at full speed.
type Schedule struct {
	cells [Cells]int64
}

// Window is what the grid says about one moment: whether a transfer may run,
// how fast, and when that answer next changes.
type Window struct {
	Open  bool      `json:"open"`
	Limit int64     `json:"limit"` // bytes per second; 0 is no limit
	Until time.Time `json:"until"`
}

// Describe renders a window the way the dashboard and the log quote it.
func (w Window) Describe() string {
	switch {
	case !w.Open:
		return "closed"
	case w.Limit == 0:
		return "open, no limit"
	default:
		return "open, capped at " + format.Rate(w.Limit, time.Second)
	}
}

// Parse reads a schedule. The empty string is always open at full speed, which
// is what an install that has not been given one gets.
//
// The syntax is a list of rules, separated by semicolons or newlines, each
// naming days, hours and a limit:
//
//   - * = 5MiB ; * 2-8 = full ; sat-sun 8-18 = off
//
// Later rules win over earlier ones, so the first is the base and the rest are
// the exceptions - the order the schedule is usually thought about in.
func Parse(s string) (Schedule, error) { return parse(s, true) }

// ParseGate reads a schedule that only opens and closes. A scan moves metadata
// and there is nothing to limit, so a rate there is refused rather than quietly
// ignored (DESIGN.md §6: the scan schedule is the coarser one).
func ParseGate(s string) (Schedule, error) { return parse(s, false) }

func parse(s string, allowCaps bool) (Schedule, error) {
	var out Schedule // every cell open and uncapped

	for _, rule := range splitRules(s) {
		days, hours, limit, err := parseRule(rule, allowCaps)
		if err != nil {
			return Schedule{}, fmt.Errorf("%q: %w", rule, err)
		}
		for _, d := range days {
			for _, h := range hours {
				out.cells[int(d)*Hours+h] = limit
			}
		}
	}
	return out, nil
}

func splitRules(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == '\n' }) {
		if part = strings.TrimSpace(part); part != "" && !strings.HasPrefix(part, "#") {
			out = append(out, part)
		}
	}
	return out
}

func parseRule(rule string, allowCaps bool) ([]time.Weekday, []int, int64, error) {
	lhs, rhs, ok := strings.Cut(rule, "=")
	if !ok {
		return nil, nil, 0, errors.New(`expected "<days> <hours> = <limit>", e.g. "mon-fri 8-18 = 5MiB"`)
	}

	fields := strings.Fields(lhs)
	if len(fields) != 2 {
		return nil, nil, 0, errors.New(`expected days and hours before the "=", e.g. "mon-fri 8-18"`)
	}

	days, err := parseDays(fields[0])
	if err != nil {
		return nil, nil, 0, err
	}
	hours, err := parseHours(fields[1])
	if err != nil {
		return nil, nil, 0, err
	}
	limit, err := parseCap(rhs, allowCaps)
	if err != nil {
		return nil, nil, 0, err
	}
	return days, hours, limit, nil
}

// dayNames is indexed by time.Weekday, so Sunday is first; displayOrder is the
// order a human reads the week in and the one the grid is rendered in.
var dayNames = [Days]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

var displayOrder = [Days]time.Weekday{
	time.Monday, time.Tuesday, time.Wednesday, time.Thursday,
	time.Friday, time.Saturday, time.Sunday,
}

func parseDays(spec string) ([]time.Weekday, error) {
	if spec == "*" {
		return displayOrder[:], nil
	}

	var out []time.Weekday
	for _, part := range strings.Split(spec, ",") {
		from, to, isRange := strings.Cut(strings.TrimSpace(part), "-")
		start, err := parseDay(from)
		if err != nil {
			return nil, err
		}
		if !isRange {
			out = append(out, displayOrder[start])
			continue
		}
		end, err := parseDay(to)
		if err != nil {
			return nil, err
		}
		// Wrapping is allowed, so "fri-mon" is the four days it reads as rather
		// than an error nobody expected.
		for i := start; ; i = (i + 1) % Days {
			out = append(out, displayOrder[i])
			if i == end {
				break
			}
		}
	}
	return out, nil
}

// parseDay returns an index into displayOrder.
func parseDay(name string) (int, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if len(name) > 3 {
		name = name[:3] // "monday" and "mon" are the same day
	}
	for i, wd := range displayOrder {
		if dayNames[wd] == name {
			return i, nil
		}
	}
	return 0, fmt.Errorf("%q is not a weekday: want mon, tue, wed, thu, fri, sat, sun or *", name)
}

func parseHours(spec string) ([]int, error) {
	if spec == "*" {
		spec = "0-24"
	}

	var out []int
	for _, part := range strings.Split(spec, ",") {
		from, to, isRange := strings.Cut(strings.TrimSpace(part), "-")
		start, err := parseHour(from, Hours-1)
		if err != nil {
			return nil, err
		}
		if !isRange {
			out = append(out, start)
			continue
		}
		end, err := parseHour(to, Hours)
		if err != nil {
			return nil, err
		}
		if start == end {
			return nil, fmt.Errorf("%q spans no hours at all", part)
		}
		// The end is exclusive, so 2-8 is 02:00 until 08:00 - six hours, the way
		// it is said out loud. An end before the start wraps over midnight, and a
		// span that comes back to where it started - 0-24 - is the whole day.
		span := (end - start + Hours) % Hours
		if span == 0 {
			span = Hours
		}
		for i := 0; i < span; i++ {
			out = append(out, (start+i)%Hours)
		}
	}
	return out, nil
}

func parseHour(s string, max int) (int, error) {
	// "08:00" and "8" are the same hour; anything past the colon is noise, since
	// the grid has no finer resolution than an hour.
	s = strings.TrimSpace(s)
	if h, _, ok := strings.Cut(s, ":"); ok {
		s = h
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not an hour", s)
	}
	if n < 0 || n > max {
		return 0, fmt.Errorf("hour %d is outside 0-%d", n, max)
	}
	return n, nil
}

func parseCap(spec string, allowCaps bool) (int64, error) {
	spec = strings.TrimSpace(strings.ToLower(spec))
	spec = strings.TrimSuffix(strings.TrimSuffix(spec, "/s"), "ps")

	switch strings.TrimSpace(spec) {
	case "":
		return 0, errors.New(`nothing after the "=": want off, full, or a rate like 5MiB`)
	case "off", "closed", "no", "false", "0":
		return closed, nil
	case "full", "on", "yes", "true", "unlimited", "max":
		return unlimited, nil
	}

	n, err := format.ParseSize(spec)
	if err != nil {
		return 0, fmt.Errorf("limit %q: want off, full, or a rate like 5MiB: %w", spec, err)
	}
	if !allowCaps {
		return 0, fmt.Errorf("limit %q: this schedule only opens and closes - write on or off", spec)
	}
	return n, nil
}

// At reports what the grid says at t, evaluated in t's own location.
func (s Schedule) At(t time.Time) Window {
	cur := s.cellAt(t)

	// The boundary is where the answer changes, not where the hour does: a limit
	// that holds for six hours must not wake the scheduler five times for nothing.
	end := hourAfter(t)
	for i := 0; i < Cells && s.cellAt(end) == cur; i++ {
		end = hourAfter(end)
	}

	w := Window{Open: cur != closed, Until: end}
	if cur > 0 {
		w.Limit = cur
	}
	return w
}

// NextOpen is when transfers may run again, which is what a closed window has to
// be able to say. ok is false for a grid that is closed the whole week.
func (s Schedule) NextOpen(t time.Time) (time.Time, bool) {
	if s.cellAt(t) != closed {
		return t, true
	}
	next := hourAfter(t)
	for i := 0; i < Cells; i++ {
		if s.cellAt(next) != closed {
			return next, true
		}
		next = hourAfter(next)
	}
	return time.Time{}, false
}

// Cell is one square of the grid, for rendering it.
func (s Schedule) Cell(day time.Weekday, hour int) Window {
	c := s.cells[int(day)*Hours+hour%Hours]
	w := Window{Open: c != closed}
	if c > 0 {
		w.Limit = c
	}
	return w
}

// Uniform reports whether every cell says the same thing, which is the case for
// an install that has never set a schedule.
func (s Schedule) Uniform() bool {
	for _, c := range s.cells {
		if c != s.cells[0] {
			return false
		}
	}
	return true
}

// Equal compares two grids cell by cell. Two schedules written differently but
// meaning the same thing are equal, which is what makes the canonical form of
// String worth having.
func (s Schedule) Equal(o Schedule) bool { return s.cells == o.cells }

func (s Schedule) cellAt(t time.Time) int64 {
	return s.cells[int(t.Weekday())*Hours+t.Hour()]
}

// hourAfter is the start of the hour following t, in t's own location. It is
// calendar arithmetic rather than t.Add(time.Hour) because a zone that changes
// offset does not have 24 equal hours in the day it changes on.
func hourAfter(t time.Time) time.Time {
	y, m, d := t.Date()
	next := time.Date(y, m, d, t.Hour()+1, 0, 0, 0, t.Location())
	if !next.After(t) {
		// The hour repeated - the clocks went back - and the calendar answer is
		// behind us. Stepping by the elapsed hour instead always moves forward.
		next = t.Add(time.Hour)
	}
	return next
}

// String renders the grid back into the syntax Parse reads, in a canonical form:
// days that agree are one rule, and hours that agree are one span. Two schedules
// that mean the same thing therefore print the same way.
func (s Schedule) String() string {
	var (
		out    []string
		done   [Days]bool
		byDay  [Days][Hours]int64
		byRule = func(days []int, hours []int, limit int64) {
			out = append(out, renderDays(days)+" "+renderHours(hours)+" = "+renderCap(limit))
		}
	)

	for i, wd := range displayOrder {
		for h := 0; h < Hours; h++ {
			byDay[i][h] = s.cells[int(wd)*Hours+h]
		}
	}

	for i := range displayOrder {
		if done[i] {
			continue
		}
		group := []int{i}
		for j := i + 1; j < Days; j++ {
			if !done[j] && byDay[j] == byDay[i] {
				group = append(group, j)
				done[j] = true
			}
		}
		done[i] = true

		// One rule per distinct limit in the group, in the order the day first
		// reaches it, so a grid reads left to right like the day does.
		var seen []int64
		for h := 0; h < Hours; h++ {
			limit := byDay[i][h]
			if containsCap(seen, limit) {
				continue
			}
			seen = append(seen, limit)

			var hours []int
			for k := 0; k < Hours; k++ {
				if byDay[i][k] == limit {
					hours = append(hours, k)
				}
			}
			byRule(group, hours, limit)
		}
	}
	return strings.Join(out, "; ")
}

func containsCap(seen []int64, c int64) bool {
	for _, s := range seen {
		if s == c {
			return true
		}
	}
	return false
}

// renderDays writes indices into displayOrder as "*", "mon-fri" or "mon,thu".
func renderDays(days []int) string {
	if len(days) == Days {
		return "*"
	}

	var parts []string
	for i := 0; i < len(days); {
		j := i
		for j+1 < len(days) && days[j+1] == days[j]+1 {
			j++
		}
		switch {
		case j == i:
			parts = append(parts, dayNames[displayOrder[days[i]]])
		default:
			parts = append(parts, dayNames[displayOrder[days[i]]]+"-"+dayNames[displayOrder[days[j]]])
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

func renderHours(hours []int) string {
	if len(hours) == Hours {
		return "*"
	}

	var parts []string
	for i := 0; i < len(hours); {
		j := i
		for j+1 < len(hours) && hours[j+1] == hours[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, strconv.Itoa(hours[i]))
		} else {
			parts = append(parts, strconv.Itoa(hours[i])+"-"+strconv.Itoa(hours[j]+1))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

func renderCap(c int64) string {
	switch {
	case c == closed:
		return "off"
	case c == unlimited:
		return "full"
	default:
		return format.Size(c)
	}
}
