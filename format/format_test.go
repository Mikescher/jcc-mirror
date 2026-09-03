package format

import "testing"

func TestParseSize(t *testing.T) {
	sizes := map[string]int64{
		"64MiB":    64 << 20,
		"64M":      64 << 20,
		"64 GB":    64 << 30,
		"67108864": 67108864,
		"1k":       1 << 10,
		"2T":       2 << 40,
		" 8 MiB ":  8 << 20,
	}
	for in, want := range sizes {
		got, err := ParseSize(in)
		if err != nil {
			t.Errorf("ParseSize(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSize(%q) = %d, want %d", in, got, want)
		}
	}

	for _, in := range []string{"", "   ", "sixty-four megs", "64XB", "0", "-5", "64MiB extra"} {
		if got, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) = %d, want an error", in, got)
		}
	}
}

// TestSizeRoundTrips is the property the schedule needs: a stored cap is
// rendered back into the syntax it was written in.
func TestSizeRoundTrips(t *testing.T) {
	for _, want := range []int64{1, 1023, 1024, 5 << 20, 64 << 20, 50 << 30, 3 << 40} {
		got, err := ParseSize(Size(want))
		if err != nil {
			t.Errorf("ParseSize(Size(%d) = %q): %v", want, Size(want), err)
			continue
		}
		if got != want {
			t.Errorf("ParseSize(Size(%d) = %q) = %d", want, Size(want), got)
		}
	}
	if got := Size(0); got != "0" {
		t.Errorf("Size(0) = %q, want %q", got, "0")
	}
}
