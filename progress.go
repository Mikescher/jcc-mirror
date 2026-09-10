package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/logs"
)

// countingReader counts bytes and records when the last one arrived. The
// timestamp is what makes a stalled transfer distinguishable from a slow one:
// a publisher that stops sending without closing the connection produces no
// error at all, and only the gap gives it away.
type countingReader struct {
	r     io.Reader
	bytes *atomic.Int64
	last  *atomic.Int64 // unix nanos
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.bytes.Add(int64(n))
		c.last.Store(time.Now().UnixNano())
	}
	return n, err
}

// reportBytes prints a throughput line every interval until the returned function
// is called. It reports the instantaneous rate as well as the average, because a
// transfer that has degraded looks fine on the average for a long time.
func reportBytes(ctx context.Context, log *logs.Logger, label string, done *atomic.Int64, total int64, interval time.Duration) func() {
	if interval <= 0 {
		return func() {}
	}

	stop := make(chan struct{})
	var once sync.Once

	go func() {
		tick := time.NewTicker(interval)
		defer tick.Stop()

		start := time.Now()
		lastAt, lastBytes := start, int64(0)

		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case now := <-tick.C:
				n := done.Load()
				inst := format.Rate(n-lastBytes, now.Sub(lastAt))
				avg := format.Rate(n, now.Sub(start))
				lastAt, lastBytes = now, n

				if total > 0 {
					eta := "-"
					if n > 0 {
						remaining := time.Duration(float64(total-n) / float64(n) * float64(now.Sub(start)))
						eta = format.Duration(remaining)
					}
					log.Infof("%s: %s / %s (%.1f%%), %s now, %s avg, eta %s",
						label, format.Bytes(n), format.Bytes(total), 100*float64(n)/float64(total), inst, avg, eta)
					continue
				}
				log.Infof("%s: %s, %s now, %s avg", label, format.Bytes(n), inst, avg)
			}
		}
	}()

	return func() { once.Do(func() { close(stop) }) }
}

// transferSummary is the line every transfer command ends with.
func transferSummary(label string, n int64, d time.Duration) string {
	return fmt.Sprintf("%s: %s in %s (%s)", label, format.Bytes(n), format.Duration(d), format.Rate(n, d))
}
