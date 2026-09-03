package store

import (
	"context"
	"testing"
	"time"
)

func TestBandwidthAccumulatesWithinAMinute(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	at := time.Date(2026, 3, 1, 12, 30, 10, 0, time.UTC)
	for _, sample := range []struct{ in, out int64 }{{100, 10}, {50, 5}} {
		if err := s.AddBandwidth(ctx, at, sample.in, sample.out); err != nil {
			t.Fatalf("AddBandwidth: %v", err)
		}
		at = at.Add(20 * time.Second)
	}
	// A different minute, so the first bucket is provably not a catch-all.
	if err := s.AddBandwidth(ctx, at.Add(time.Minute), 7, 0); err != nil {
		t.Fatalf("AddBandwidth: %v", err)
	}
	if err := s.AddBandwidth(ctx, at, 0, 0); err != nil {
		t.Fatalf("AddBandwidth of nothing: %v", err)
	}

	got, err := s.Bandwidth(ctx, SpanMinute, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Bandwidth: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d minute buckets, want 2: %+v", len(got), got)
	}
	if got[0].In != 150 || got[0].Out != 15 {
		t.Errorf("first bucket = %d/%d, want 150/15", got[0].In, got[0].Out)
	}
	if got[0].TS.Second() != 0 {
		t.Errorf("bucket start = %v, want it truncated to the minute", got[0].TS)
	}
	if !got[0].TS.Before(got[1].TS) {
		t.Errorf("samples are not oldest first: %v then %v", got[0].TS, got[1].TS)
	}
}

func TestBandwidthWindow(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		if err := s.AddBandwidth(ctx, base.Add(time.Duration(i)*time.Minute), 1, 0); err != nil {
			t.Fatalf("AddBandwidth: %v", err)
		}
	}

	got, err := s.Bandwidth(ctx, SpanMinute, base.Add(time.Minute), base.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("Bandwidth: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d samples in [1m, 3m), want 2", len(got))
	}
	if !got[0].TS.Equal(base.Add(time.Minute)) {
		t.Errorf("window starts at %v, want %v", got[0].TS, base.Add(time.Minute))
	}
}

func TestRollupBandwidth(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	base := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	for i := range 90 {
		if err := s.AddBandwidth(ctx, base.Add(time.Duration(i)*time.Minute), 10, 1); err != nil {
			t.Fatalf("AddBandwidth: %v", err)
		}
	}

	folded, err := s.RollupBandwidth(ctx, SpanMinute, SpanHour, base.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("RollupBandwidth: %v", err)
	}
	if folded != 90 {
		t.Fatalf("folded %d rows, want 90", folded)
	}

	left, err := s.Bandwidth(ctx, SpanMinute, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Bandwidth: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("%d minute rows survived the rollup", len(left))
	}

	hours, err := s.Bandwidth(ctx, SpanHour, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Bandwidth: %v", err)
	}
	if len(hours) != 2 {
		t.Fatalf("got %d hour buckets, want 2: %+v", len(hours), hours)
	}
	if hours[0].In != 600 || hours[1].In != 300 {
		t.Errorf("hour totals = %d and %d, want 600 and 300", hours[0].In, hours[1].In)
	}

	// Running it again must be a no-op rather than a doubling.
	again, err := s.RollupBandwidth(ctx, SpanMinute, SpanHour, base.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("RollupBandwidth twice: %v", err)
	}
	if again != 0 {
		t.Errorf("second rollup folded %d rows, want 0", again)
	}
}

func TestPruneBandwidth(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for i := range 3 {
		if err := s.AddBandwidth(ctx, base.Add(time.Duration(i)*time.Minute), 1, 0); err != nil {
			t.Fatalf("AddBandwidth: %v", err)
		}
	}

	n, err := s.PruneBandwidth(ctx, SpanMinute, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("PruneBandwidth: %v", err)
	}
	if n != 2 {
		t.Errorf("pruned %d rows, want 2", n)
	}
}

func TestBandwidthRejectsUnknownSpan(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.Bandwidth(ctx, "week", time.Time{}, time.Time{}); err == nil {
		t.Fatal("Bandwidth accepted an unknown span")
	}
	if _, err := s.RollupBandwidth(ctx, SpanMinute, "week", time.Now()); err == nil {
		t.Fatal("RollupBandwidth accepted an unknown span")
	}
}
