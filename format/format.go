// Package format renders the numbers this project deals in - sizes spanning a
// lock file and thirty terabytes, rates quoted against a line speed, entry counts
// compared by eye between runs - and reads back the ones an operator types.
package format

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Bytes formats a byte count with a binary prefix.
func Bytes(n int64) string {
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

// Rate formats a transfer rate, in bits per second because that is the unit the
// line speed and the bandwidth schedule are quoted in.
func Rate(bytes int64, d time.Duration) string {
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

// Duration rounds a duration to something readable at its own scale. Go's own
// rendering stops at hours, which is not a scale this project stops at: a
// quarantine retention and the ETA of a 30 TB transfer are both counted in days.
func Duration(d time.Duration) string {
	const day = 24 * time.Hour

	switch {
	case d >= day:
		d = d.Round(time.Hour)
		if hours := d % day / time.Hour; hours > 0 {
			return fmt.Sprintf("%dd%dh", d/day, hours)
		}
		return fmt.Sprintf("%dd", d/day)
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

// Comma groups a number in thousands. Entry counts in this project run into the
// hundreds of thousands and are compared by eye between runs.
func Comma(n int64) string {
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

// Size renders a byte count so that ParseSize reads it back unchanged. It is the
// form a setting is stored and edited in, where Bytes is the form it is read in:
// "5MiB" round-trips, "5.00 MiB" does not.
func Size(n int64) string {
	if n == 0 {
		return "0"
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	out, unit := n, ""
	for i := 0; i < len(units); i++ {
		if out%1024 != 0 {
			break
		}
		out, unit = out/1024, units[i]
	}
	return strconv.FormatInt(out, 10) + unit
}

// ParseSize reads a byte count, with or without a binary suffix: "67108864",
// "64MiB" and "64M" are the same number. Sizes in this project are quoted in
// whichever of the two an operator happens to reach for.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("required")
	}

	digits := strings.TrimRight(s, "kKmMgGtTiIbB \t")
	suffix := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(s, digits)))
	suffix = strings.TrimSuffix(strings.TrimSuffix(suffix, "b"), "i")

	n, err := strconv.ParseInt(strings.TrimSpace(digits), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("not a size like \"64MiB\": %w", err)
	}

	mult := int64(1)
	switch suffix {
	case "":
	case "k":
		mult = 1 << 10
	case "m":
		mult = 1 << 20
	case "g":
		mult = 1 << 30
	case "t":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("unknown size suffix %q", suffix)
	}
	if n <= 0 {
		return 0, errors.New("must be positive")
	}
	return n * mult, nil
}
