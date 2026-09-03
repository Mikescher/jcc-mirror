package engine

import (
	"context"
	"testing"
	"time"
)

// TestLimiterShapesTheRate is the whole point of the cap: the bytes take the
// time the number says they should. The first burst passes for free, so the
// expectation is over what is left after it.
func TestLimiterShapesTheRate(t *testing.T) {
	const (
		limit = 1 << 20 // 1 MiB/s, so the burst is 1 MiB too
		want  = 500 * time.Millisecond
	)

	l := NewLimiter(limit)
	start := time.Now()
	for n := 0; n < 24; n++ { // 1.5 MiB in 64 KiB reads
		if err := l.wait(context.Background(), 64<<10); err != nil {
			t.Fatalf("wait: %v", err)
		}
	}

	// Generous on both sides: this is a rate, not a stopwatch, and a loaded
	// machine must not make it flake.
	if took := time.Since(start); took < want*2/3 || took > 10*want {
		t.Errorf("1.5 MiB at 1 MiB/s took %s, want around %s", took, want)
	}
}

// TestLimiterIsSharedAndLive is what a window boundary needs: the same limiter
// is handed to every stream, and the number changes under a transfer that is
// already running.
func TestLimiterIsSharedAndLive(t *testing.T) {
	l := NewLimiter(64 << 10)
	if l.Limit() != 64<<10 {
		t.Fatalf("Limit = %d", l.Limit())
	}

	l.Set(0)
	if l.Limit() != 0 {
		t.Fatalf("after Set(0), Limit = %d, want no cap", l.Limit())
	}

	// Uncapped means uncapped, however much is asked for: a boundary that opens
	// into an unlimited hour must not leave the transfer paying off the old rate.
	start := time.Now()
	if err := l.wait(context.Background(), 512<<20); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("an uncapped wait for 512 MiB took %s", took)
	}
}

// TestNilLimiterIsNoCap keeps the engine's zero configuration honest: an engine
// built without a schedule has no limiter at all.
func TestNilLimiterIsNoCap(t *testing.T) {
	var l *Limiter
	if l.Limit() != 0 {
		t.Errorf("a nil limiter reports a cap of %d", l.Limit())
	}
	l.Set(1 << 20)
	if err := l.wait(context.Background(), 1<<30); err != nil {
		t.Errorf("wait on a nil limiter: %v", err)
	}
}

// TestCappedTransferTakesItsTime is the limiter where it actually matters: in
// the read path of a transfer, shared across the chunk streams of one file.
func TestCappedTransferTakesItsTime(t *testing.T) {
	const size = 768 << 10

	h := newHarness(t, func(o *Options) {
		o.Chunks, o.ChunkSize = 4, 64<<10
		o.Limiter = NewLimiter(512 << 10) // burst 512 KiB, so 256 KiB is paid for
	})
	body := h.write("Filme/capped.mkv", size)
	h.scan()

	start := time.Now()
	h.sync()
	took := time.Since(start)

	h.wantFile("Filme/capped.mkv", body)
	if took < 300*time.Millisecond {
		t.Errorf("768 KiB at 512 KiB/s took %s, which is faster than the cap allows", took)
	}
}
