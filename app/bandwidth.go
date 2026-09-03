package app

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"blackforestbytes.com/jcc-mirror/store"
)

// How long each resolution of the bandwidth series is kept before it is folded
// into the next one. A per-minute series kept forever grows without bound, and
// nobody reads minutes from last spring (DESIGN.md §4).
const (
	minuteRetention = 7 * 24 * time.Hour
	hourRetention   = 90 * 24 * time.Hour
	dayRetention    = 3 * 365 * 24 * time.Hour
)

// maintenanceInterval is how often the rollups and the retention sweep run. They
// are all deletes over an indexed column, so the hour is about not doing it
// pointlessly rather than about cost.
const maintenanceInterval = time.Hour

// sampleInterval must be the minute the samples are bucketed by: the sampler
// writes the delta since it last looked, so a longer interval would put a whole
// period's bytes in the minute it happened to end in.
const sampleInterval = time.Minute

// meter counts what actually crosses the wire to the publisher. It sits on the
// transport's connections rather than on its round trips, so request lines,
// headers and PROPFIND bodies are all in the number - the Bandwidth view is meant
// to answer "what did this cost the link", not "how many file bytes landed".
type meter struct {
	in  atomic.Int64
	out atomic.Int64
}

// take reads the counters and zeroes them, which is what a per-minute sample is.
func (m *meter) take() (in, out int64) {
	return m.in.Swap(0), m.out.Swap(0)
}

type meteredConn struct {
	net.Conn
	m *meter
}

func (c *meteredConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.m.in.Add(int64(n))
	return n, err
}

func (c *meteredConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.m.out.Add(int64(n))
	return n, err
}

// meteredTransportLocked is the transport the WebDAV client rides on: the
// tunnel's when there is one, an ordinary one when there is not, with every
// connection counted either way.
func (a *App) meteredTransportLocked() *http.Transport {
	var tr *http.Transport
	if a.tunnel != nil {
		tr = a.tunnel.Transport()
	} else {
		tr = http.DefaultTransport.(*http.Transport).Clone()
	}

	dial := tr.DialContext
	if dial == nil {
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		dial = d.DialContext
	}
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &meteredConn{Conn: conn, m: &a.meter}, nil
	}
	return tr
}

// maintenanceLoop keeps the tables from growing without bound: the bandwidth
// series is rolled up, and the event and change logs are pruned to their
// retention (DESIGN.md §6).
func (a *App) maintenanceLoop(ctx context.Context) {
	sample := time.NewTicker(sampleInterval)
	defer sample.Stop()
	maintain := time.NewTicker(maintenanceInterval)
	defer maintain.Stop()

	a.maintain(ctx)

	for {
		select {
		case <-ctx.Done():
			// One last sample, so the bytes of the minute a shutdown lands in are
			// not simply lost.
			a.sampleBandwidth(context.WithoutCancel(ctx))
			return
		case <-sample.C:
			a.sampleBandwidth(ctx)
		case <-maintain.C:
			a.maintain(ctx)
		}
	}
}

func (a *App) sampleBandwidth(ctx context.Context) {
	in, out := a.meter.take()
	if err := a.store.AddBandwidth(ctx, time.Now(), in, out); err != nil {
		a.log.Errorf("bandwidth: %v", err)
	}
}

func (a *App) maintain(ctx context.Context) {
	now := time.Now()

	// Minutes into hours, then hours into days. In that order: a minute row older
	// than the hour retention would otherwise have to wait a quarter of a year
	// for the second fold to reach it.
	if _, err := a.store.RollupBandwidth(ctx, store.SpanMinute, store.SpanHour, now.Add(-minuteRetention)); err != nil {
		a.log.Errorf("bandwidth: %v", err)
	}
	if _, err := a.store.RollupBandwidth(ctx, store.SpanHour, store.SpanDay, now.Add(-hourRetention)); err != nil {
		a.log.Errorf("bandwidth: %v", err)
	}
	if _, err := a.store.PruneBandwidth(ctx, store.SpanDay, now.Add(-dayRetention)); err != nil {
		a.log.Errorf("bandwidth: %v", err)
	}

	values, err := a.store.Config(ctx)
	if err != nil {
		a.log.Errorf("retention: %v", err)
		return
	}

	if d := values.Duration(store.KeyRetainEvents); d > 0 {
		if n, err := a.store.PruneEvents(ctx, now.Add(-d)); err != nil {
			a.log.Errorf("retention: %v", err)
		} else if n > 0 {
			a.log.Infof("retention: %d event(s) past %s dropped", n, d)
		}
	}

	if d := values.Duration(store.KeyRetainChanges); d > 0 {
		if n, err := a.store.PruneChanges(ctx, now.Add(-d)); err != nil {
			a.log.Errorf("retention: %v", err)
		} else if n > 0 {
			a.log.Infof("retention: %d change(s) past %s dropped", n, d)
		}
		// A finished job is the same fact as the change row it produced, so it
		// goes on the same clock. Failed jobs are never pruned: they are what
		// waits for someone to look at them.
		if _, err := a.store.PruneJobs(ctx, now.Add(-d)); err != nil {
			a.log.Errorf("retention: %v", err)
		}
	}
}

// BandwidthView is the Bandwidth view's answer: a series at one resolution, plus
// the 7x24 shape of it in the configured timezone.
type BandwidthView struct {
	Span     string            `json:"span"`
	Timezone string            `json:"timezone"`
	From     time.Time         `json:"from"`
	To       time.Time         `json:"to"`
	Samples  []store.BWSample  `json:"samples"`
	TotalIn  int64             `json:"totalIn"`
	TotalOut int64             `json:"totalOut"`
	Heatmap  [][]int64         `json:"heatmap"` // [weekday][hour] bytes in, Monday first
	Live     BandwidthLiveRate `json:"live"`
}

// BandwidthLiveRate is the bucket that just closed, which is as close to "right
// now" as a series bucketed by the minute gets. It is zero when that bucket has
// no row: a minute in which nothing moved is not written at all, so the newest
// row can be hours old and reporting it would show a rate that stopped long ago.
type BandwidthLiveRate struct {
	In  int64 `json:"in"`
	Out int64 `json:"out"`
}

// Bandwidth collects the series for the view. The heatmap is built here rather
// than in SQL because the weekday and the hour depend on the configured
// timezone, and that must not be decided in two places.
func (a *App) Bandwidth(ctx context.Context, span string, since time.Time) (BandwidthView, error) {
	samples, err := a.store.Bandwidth(ctx, span, since, time.Time{})
	if err != nil {
		return BandwidthView{}, err
	}

	loc := a.location()
	view := BandwidthView{
		Span: span, Timezone: loc.String(), From: since, To: time.Now(),
		Samples: samples, Heatmap: newHeatmap(),
	}
	for _, s := range samples {
		view.TotalIn += s.In
		view.TotalOut += s.Out
	}

	// The heatmap only ever comes from the hour and minute series: a daily row
	// has no hour-of-day left in it to draw.
	if span != store.SpanDay {
		for _, s := range samples {
			local := s.TS.In(loc)
			view.Heatmap[mondayFirst(local.Weekday())][local.Hour()] += s.In
		}
	}

	if len(samples) > 0 {
		if last := samples[len(samples)-1]; !last.TS.Before(liveAfter(span, view.To)) {
			view.Live = BandwidthLiveRate{In: last.In, Out: last.Out}
		}
	}
	return view, nil
}

// liveAfter is the oldest bucket start that still counts as the current rate:
// the one before the bucket that is still filling.
func liveAfter(span string, now time.Time) time.Time {
	start, err := store.Truncate(span, now)
	if err != nil {
		return now
	}
	switch span {
	case store.SpanHour:
		return start.Add(-time.Hour)
	case store.SpanDay:
		return start.AddDate(0, 0, -1)
	default:
		return start.Add(-time.Minute)
	}
}

func newHeatmap() [][]int64 {
	out := make([][]int64, 7)
	for i := range out {
		out[i] = make([]int64, 24)
	}
	return out
}

// mondayFirst reindexes a weekday so the heatmap reads the way a week does here,
// rather than the way time.Weekday numbers one.
func mondayFirst(d time.Weekday) int { return (int(d) + 6) % 7 }

func (a *App) handleGetBandwidth(w http.ResponseWriter, r *http.Request) {
	span := r.URL.Query().Get("span")
	if span == "" {
		span = store.SpanMinute
	}

	// The default window is what the span is kept at, so asking for a span and
	// asking for its whole series are the same request.
	var since time.Time
	switch span {
	case store.SpanMinute:
		since = time.Now().Add(-minuteRetention)
	case store.SpanHour:
		since = time.Now().Add(-hourRetention)
	}
	if hours := intParam(r, "hours", 0); hours > 0 {
		since = time.Now().Add(-time.Duration(hours) * time.Hour)
	}

	view, err := a.Bandwidth(r.Context(), span, since)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}
