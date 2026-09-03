package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"
	"time"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/schedule"
	"blackforestbytes.com/jcc-mirror/store"
)

// cmdSchedule shows and sets the 7x24 grid: when the mirror may run and how fast
// (DESIGN.md §6). Printing it is the point of the command - a schedule is one
// line of text that decides what happens for a week, and reading it back as a
// grid is the only way to be sure it says what was meant.
func cmdSchedule(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("schedule")
	set := fs.String("set", "", "replace the window with these rules, e.g. \"* * = 5MiB; * 2-8 = full\"")
	scan := fs.Bool("scan", false, "work on the scan window instead of the transfer window")
	automatic := fs.Bool("automatic", false, "whether the daemon starts scans and syncs by itself")
	every := fs.Duration("every", 0, "how stale the manifest may get before the scheduler walks again")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := cfg.openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	// Only what was actually typed is written: the zero value of a flag is not a
	// setting, it is the absence of one.
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	write := map[string]string{}
	if given["set"] {
		key := store.KeySchedule
		if *scan {
			key = store.KeyScanSchedule
		}
		write[key] = *set
	}
	if given["automatic"] {
		write[store.KeyAutomatic] = fmt.Sprint(*automatic)
	}
	if given["every"] {
		write[store.KeyScanInterval] = every.String()
	}

	if len(write) > 0 {
		changed, err := st.ConfigSet(ctx, write, "cli")
		if err != nil {
			return err
		}
		if len(changed) == 0 {
			fmt.Println("nothing changed: that is already what it says")
		} else {
			fmt.Printf("changed %s\n", strings.Join(changed, ", "))
		}
	}

	values, err := st.Config(ctx)
	if err != nil {
		return err
	}
	return printSchedules(values)
}

func printSchedules(values store.Values) error {
	loc, err := time.LoadLocation(values.Get(store.KeyTimezone))
	if err != nil {
		return fmt.Errorf("timezone %q: %w", values.Get(store.KeyTimezone), err)
	}
	now := time.Now().In(loc)

	transfer, err := schedule.Parse(values.Get(store.KeySchedule))
	if err != nil {
		return fmt.Errorf("transfer window: %w", err)
	}
	scan, err := schedule.ParseGate(values.Get(store.KeyScanSchedule))
	if err != nil {
		return fmt.Errorf("scan window: %w", err)
	}

	fmt.Printf("\nthe grids below are drawn in %s, where it is %s now\n",
		loc, now.Format("Mon 2006-01-02 15:04"))

	printGrid("transfer window", transfer, now)
	printGrid("scan window", scan, now)

	fmt.Printf("\n  unattended     %s\n", yesNo(values.Bool(store.KeyAutomatic)))
	fmt.Printf("  scan every     %s\n", values.Duration(store.KeyScanInterval))
	fmt.Printf("  reserve        %s of free space kept on the destination\n", format.Bytes(values.Size(store.KeyReserve)))

	if !values.Bool(store.KeyAutomatic) {
		fmt.Printf("\n=> nothing runs unattended: `jcc-mirror schedule -data ... -automatic` turns that on.\n")
		fmt.Printf("   The caps above apply to a hand-run sync either way.\n")
	}
	return nil
}

// printGrid draws one schedule as seven rows of twenty-four hours. Distinct caps
// get a letter each rather than a colour, and the legend below spells them out.
func printGrid(name string, s schedule.Schedule, now time.Time) {
	days := []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday,
		time.Friday, time.Saturday, time.Sunday}

	var caps []int64
	for _, d := range days {
		for h := 0; h < schedule.Hours; h++ {
			if c := s.Cell(d, h); c.Open && c.Limit > 0 && !containsInt64(caps, c.Limit) {
				caps = append(caps, c.Limit)
			}
		}
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i] < caps[j] })

	w := s.At(now)
	fmt.Printf("\n  %s - %s until %s\n\n", name, w.Describe(), w.Until.Format("Mon 15:04"))

	var tens, units strings.Builder
	for h := 0; h < schedule.Hours; h++ {
		tens.WriteString(fmt.Sprintf("%d", h/10))
		units.WriteString(fmt.Sprintf("%d", h%10))
	}
	fmt.Printf("        %s\n        %s\n", tens.String(), units.String())

	for _, d := range days {
		var row strings.Builder
		for h := 0; h < schedule.Hours; h++ {
			row.WriteByte(cellSymbol(s.Cell(d, h), caps))
		}
		marker := " "
		if d == now.Weekday() {
			marker = ">"
		}
		fmt.Printf("  %s %s %s\n", marker, strings.ToLower(d.String()[:3]), row.String())
	}

	legend := []string{"# no limit", ". closed"}
	for i, c := range caps {
		legend = append(legend, fmt.Sprintf("%c %s/s (%s)", 'a'+i, format.Size(c), format.Rate(c, time.Second)))
	}
	fmt.Printf("\n        %s\n", strings.Join(legend, "   "))
	fmt.Printf("        rules: %s\n", s.String())
}

func cellSymbol(w schedule.Window, caps []int64) byte {
	switch {
	case !w.Open:
		return '.'
	case w.Limit == 0:
		return '#'
	}
	for i, c := range caps {
		if c == w.Limit {
			return byte('a' + i)
		}
	}
	return '?'
}

func containsInt64(in []int64, v int64) bool {
	for _, x := range in {
		if x == v {
			return true
		}
	}
	return false
}
