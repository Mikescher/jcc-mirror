package app

import (
	"context"
	"net"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"blackforestbytes.com/jcc-mirror/smb"
	"blackforestbytes.com/jcc-mirror/store"
)

// How long the minute and hour series are kept before they are folded into the
// next resolution. A per-minute series kept forever grows without bound, and
// nobody reads minutes from last spring (DESIGN.md §4). Days are never dropped:
// they are 365 rows a year, and they are what the calendar draws.
const (
	minuteRetention = 7 * 24 * time.Hour
	hourRetention   = 90 * 24 * time.Hour
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
// connections rather than on the operations they carry, so SMB's own framing and
// its directory listings are in the number too - the Bandwidth view is meant to
// answer "what did this cost the link", not "how many file bytes landed".
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

// meteredDialLocked is how the SMB client reaches the publisher: through the
// tunnel when there is one, out of the host network when there is not, with every
// connection counted either way. Metering at the dial is the only place it can
// happen now that the remote is not HTTP - there is no round trip to hook.
func (a *App) meteredDialLocked() smb.DialFunc {
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		d := net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		return d.DialContext(ctx, network, address)
	}
	if a.tunnel != nil {
		dial = a.tunnel.DialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &meteredConn{Conn: conn, m: &a.meter}, nil
	}
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

// BandwidthView is the Bandwidth view's answer: a series at one resolution, the
// 7x24 shape of it in the configured timezone, and every day on record for the
// calendar.
type BandwidthView struct {
	Span     string           `json:"span"`
	Timezone string           `json:"timezone"`
	From     time.Time        `json:"from"`
	To       time.Time        `json:"to"`
	Samples  []store.BWSample `json:"samples"`
	TotalIn  int64            `json:"totalIn"`
	TotalOut int64            `json:"totalOut"`
	Heatmap  [][]int64        `json:"heatmap"` // [weekday][hour] bytes in, Monday first
	// HeatmapBuckets counts the samples behind each heatmap cell. Only buckets
	// in which something moved are stored, so bytes over buckets times the
	// bucket length is the rate while the link was busy, not a long-run average.
	HeatmapBuckets [][]int64         `json:"heatmapBuckets"`
	Live           BandwidthLiveRate `json:"live"`
	// Days is every day on which something moved, oldest first, whatever the
	// span; the calendar runs from the first of them to Today.
	Days  []BandwidthDay `json:"days"`
	Today string         `json:"today"`
}

// BandwidthLiveRate is the bucket that just closed, which is as close to "right
// now" as a series bucketed by the minute gets. It is zero when that bucket has
// no row: a minute in which nothing moved is not written at all, so the newest
// row can be hours old and reporting it would show a rate that stopped long ago.
type BandwidthLiveRate struct {
	In  int64 `json:"in"`
	Out int64 `json:"out"`
}

// BandwidthDay is one calendar day in the configured timezone. The date is sent
// as YYYY-MM-DD rather than as a timestamp so a browser in another zone cannot
// move it across midnight.
type BandwidthDay struct {
	Date string `json:"date"`
	In   int64  `json:"in"`
	Out  int64  `json:"out"`
}

// Bandwidth collects the series for the view. The heatmap and the days are built
// here rather than in SQL because the weekday, the hour and the date depend on
// the configured timezone, and that must not be decided in two places.
func (a *App) Bandwidth(ctx context.Context, span string, since time.Time) (BandwidthView, error) {
	loc := a.location()
	now := time.Now()

	days, err := a.bandwidthDays(ctx, loc)
	if err != nil {
		return BandwidthView{}, err
	}

	var samples []store.BWSample
	if span == store.SpanDay {
		samples = daySamples(days, since, loc)
	} else if samples, err = a.store.Bandwidth(ctx, span, since, time.Time{}); err != nil {
		return BandwidthView{}, err
	}

	view := BandwidthView{
		Span: span, Timezone: loc.String(), From: since, To: now,
		Samples: samples, Heatmap: newHeatmap(), HeatmapBuckets: newHeatmap(),
		Days: days, Today: now.In(loc).Format(time.DateOnly),
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
			day, hour := mondayFirst(local.Weekday()), local.Hour()
			view.Heatmap[day][hour] += s.In
			view.HeatmapBuckets[day][hour]++
		}
	}

	after := liveAfter(span, now, loc)
	for i := len(samples) - 1; i >= 0; i-- {
		s := samples[i]
		if s.TS.Before(after) {
			break
		}
		// The day series ends with today, which is still filling; the closed
		// bucket is yesterday.
		if span == store.SpanDay && !s.TS.Equal(after) {
			continue
		}
		view.Live = BandwidthLiveRate{In: s.In, Out: s.Out}
		break
	}
	return view, nil
}

// bandwidthDays folds all three resolutions into days. The day rows alone are
// not the daily series: a rollup only writes them once an hour is past the hour
// retention, so the most recent 90 days are still minutes and hours.
func (a *App) bandwidthDays(ctx context.Context, loc *time.Location) ([]BandwidthDay, error) {
	byDate := map[string]BandwidthDay{}
	for _, span := range []string{store.SpanDay, store.SpanHour, store.SpanMinute} {
		samples, err := a.store.Bandwidth(ctx, span, time.Time{}, time.Time{})
		if err != nil {
			return nil, err
		}
		for _, s := range samples {
			// A day row was cut at midnight UTC, so it is dated in UTC; read in a
			// zone west of UTC it would land on the day before.
			at := s.TS.In(loc)
			if span == store.SpanDay {
				at = s.TS.UTC()
			}
			date := at.Format(time.DateOnly)
			d := byDate[date]
			d.Date = date
			d.In += s.In
			d.Out += s.Out
			byDate[date] = d
		}
	}

	out := make([]BandwidthDay, 0, len(byDate))
	for _, d := range byDate {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out, nil
}

// daySamples is the day series as the chart reads it: one bucket per day,
// starting at local midnight, from the day since falls in.
func daySamples(days []BandwidthDay, since time.Time, loc *time.Location) []store.BWSample {
	out := []store.BWSample{}
	for _, d := range days {
		start, err := time.ParseInLocation(time.DateOnly, d.Date, loc)
		if err != nil {
			continue
		}
		if !since.IsZero() && !start.AddDate(0, 0, 1).After(since) {
			continue
		}
		out = append(out, store.BWSample{TS: start, In: d.In, Out: d.Out})
	}
	return out
}

// liveAfter is the oldest bucket start that still counts as the current rate:
// the one before the bucket that is still filling. Days start at local midnight,
// the other two at the UTC truncation the store uses.
func liveAfter(span string, now time.Time, loc *time.Location) time.Time {
	if span == store.SpanDay {
		y, m, d := now.In(loc).Date()
		return time.Date(y, m, d, 0, 0, 0, 0, loc).AddDate(0, 0, -1)
	}
	start, err := store.Truncate(span, now)
	if err != nil {
		return now
	}
	if span == store.SpanHour {
		return start.Add(-time.Hour)
	}
	return start.Add(-time.Minute)
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
