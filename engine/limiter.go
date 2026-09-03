package engine

import (
	"context"
	"sync"

	"golang.org/x/time/rate"
)

// The window a burst may cover. The floor is above io.Copy's 32 KiB buffer so a
// single read never has to be split, and the ceiling keeps a high cap from
// arriving in one lump that the link then has to absorb.
const (
	minBurst = 64 << 10
	maxBurst = 4 << 20
)

// Limiter is the bandwidth cap. One is shared by every chunk stream of the file
// in flight, so the cap is global rather than per-stream, and it is changed
// while a transfer runs - crossing a window boundary adjusts the number without
// touching the transfer (DESIGN.md §2.4).
//
// A nil *Limiter is an uncapped one, which is what makes it safe to hand to an
// engine that has no schedule.
type Limiter struct {
	mu    sync.Mutex
	limit int64
	rl    *rate.Limiter
}

// NewLimiter returns a limiter capped at bytesPerSecond; zero or less is no cap.
func NewLimiter(bytesPerSecond int64) *Limiter {
	l := &Limiter{}
	l.Set(bytesPerSecond)
	return l
}

// Set changes the cap in place. Zero or less removes it.
func (l *Limiter) Set(bytesPerSecond int64) {
	if l == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if bytesPerSecond <= 0 {
		l.limit, l.rl = 0, nil
		return
	}

	burst := bytesPerSecond
	if burst < minBurst {
		burst = minBurst
	}
	if burst > maxBurst {
		burst = maxBurst
	}

	l.limit = bytesPerSecond
	if l.rl == nil {
		l.rl = rate.NewLimiter(rate.Limit(bytesPerSecond), int(burst))
		return
	}
	l.rl.SetLimit(rate.Limit(bytesPerSecond))
	l.rl.SetBurst(int(burst))
}

// Limit is the cap in force, or zero for none.
func (l *Limiter) Limit() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit
}

// wait blocks until n bytes may pass. It takes them in burst-sized pieces rather
// than one reservation, because a read larger than the burst would otherwise be
// refused outright instead of throttled.
func (l *Limiter) wait(ctx context.Context, n int) error {
	if l == nil {
		return nil
	}

	for n > 0 {
		l.mu.Lock()
		rl, burst := l.rl, 0
		if rl != nil {
			burst = rl.Burst()
		}
		l.mu.Unlock()

		if rl == nil || burst <= 0 {
			return nil
		}

		take := n
		if take > burst {
			take = burst
		}
		if err := rl.WaitN(ctx, take); err != nil {
			return err
		}
		n -= take
	}
	return nil
}
