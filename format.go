package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// humanBytes formats a byte count with a binary prefix. Sizes here span a lock
// file and thirty terabytes, so a raw number is unreadable in both directions.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 5; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// humanRate formats a transfer rate, in bits per second because that is the unit
// the line speed and the bandwidth schedule are quoted in.
func humanRate(bytes int64, d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	bps := float64(bytes) * 8 / d.Seconds()
	switch {
	case bps >= 1e9:
		return fmt.Sprintf("%.2f Gbit/s", bps/1e9)
	case bps >= 1e6:
		return fmt.Sprintf("%.2f Mbit/s", bps/1e6)
	case bps >= 1e3:
		return fmt.Sprintf("%.2f kbit/s", bps/1e3)
	default:
		return fmt.Sprintf("%.0f bit/s", bps)
	}
}

// humanDuration rounds a duration to something readable at its own scale.
func humanDuration(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return d.Round(time.Second).String()
	case d >= time.Minute:
		return d.Round(100 * time.Millisecond).String()
	case d >= time.Second:
		return d.Round(time.Millisecond).String()
	default:
		return d.Round(10 * time.Microsecond).String()
	}
}

// comma groups a number in thousands. Entry counts in this project run into the
// hundreds of thousands and are compared by eye between runs.
func comma(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")

	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
