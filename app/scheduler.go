package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"blackforestbytes.com/jcc-mirror/engine"
	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/schedule"
	"blackforestbytes.com/jcc-mirror/store"
)

// schedulerInterval is the longest the scheduler sleeps. It wakes earlier for a
// window boundary; this is the floor under everything else - a pair that became
// due, a queue that a manual run left behind, a remote that came back.
const schedulerInterval = 30 * time.Second

// failureBackoff holds a pair back after a scheduled run of it failed. Without
// it a remote that answers but is broken produces a failed run, and an event,
// every half minute.
const failureBackoff = 15 * time.Minute

// databaseRetry is how long the scheduler waits before probing a lock again. The
// gate's answer to a held lock is "not this cycle" (DESIGN.md §3), and nothing
// else brings the pair back around: its queue is empty and its manifest is
// unchanged, so without this the database would wait for the next walk.
const databaseRetry = 10 * time.Minute

// scheduler is the daemon's own hand on the buttons: it walks the pairs in
// priority order inside the windows the 7x24 grid opens, and it carries the
// current cell's cap into the transfer that is already running (DESIGN.md §6).
//
// It has a lock of its own rather than sharing the App's, because a run that has
// just finished records itself here while the runner's lock is held.
type scheduler struct {
	mu     sync.Mutex
	cfg    scheduleSettings
	cfgErr string

	window   schedule.Window
	observed bool // the first look reports, it does not raise an edge

	// planned is the scan a pair's last scheduled sync worked from, so a pair
	// with nothing new does not get its manifest re-planned every half minute.
	planned map[int64]int64

	// held is when a pair whose scheduled run failed may be tried again.
	held map[int64]time.Time

	// dbRetry is when a jcc pair whose database the gate deferred may be probed
	// again.
	dbRetry map[int64]time.Time
}

// scheduleSettings is the schedule half of the configuration, resolved into the
// values the tick works with.
type scheduleSettings struct {
	automatic bool
	loc       *time.Location
	transfer  schedule.Schedule
	scan      schedule.Schedule
	scanEvery time.Duration
}

// loadSchedule re-reads the grids. A value that will not parse can only come
// from a hand-edited table - the registry validates on write - and is answered
// by closing the transfer window rather than by ignoring it: a cap that silently
// stopped applying is the failure nobody would notice.
func (a *App) loadSchedule(v store.Values) {
	cfg := scheduleSettings{
		automatic: v.Bool(store.KeyAutomatic),
		loc:       time.Local,
		scanEvery: v.Duration(store.KeyScanInterval),
	}

	var problem string
	if loc, err := time.LoadLocation(v.Get(store.KeyTimezone)); err == nil {
		cfg.loc = loc
	} else {
		problem = fmt.Sprintf("timezone %q: %v", v.Get(store.KeyTimezone), err)
	}

	if s, err := schedule.Parse(v.Get(store.KeySchedule)); err == nil {
		cfg.transfer = s
	} else {
		problem = "transfer window: " + err.Error()
		cfg.transfer, _ = schedule.Parse("* * = off")
	}

	if s, err := schedule.ParseGate(v.Get(store.KeyScanSchedule)); err == nil {
		cfg.scan = s
	} else {
		problem = "scan window: " + err.Error()
	}

	a.sched.mu.Lock()
	repeat := a.sched.cfgErr == problem
	a.sched.cfg, a.sched.cfgErr = cfg, problem
	a.sched.mu.Unlock()

	if problem != "" && !repeat {
		a.log.Errorf("schedule: %s", problem)
	}
}

func (a *App) scheduleConfig() scheduleSettings {
	a.sched.mu.Lock()
	defer a.sched.mu.Unlock()
	return a.sched.cfg
}

// location is the zone everything with an hour in it is expressed in: the
// schedule grid, and the heatmap the bandwidth series is folded into. It is a
// setting of its own rather than the container's TZ (DESIGN.md §6).
func (a *App) location() *time.Location {
	a.sched.mu.Lock()
	defer a.sched.mu.Unlock()
	if a.sched.cfg.loc == nil {
		return time.Local
	}
	return a.sched.cfg.loc
}

// schedulerLoop is the daemon's clock. It sleeps until the next boundary rather
// than on a fixed tick, so a cap that changes at 08:00 changes at 08:00.
func (a *App) schedulerLoop(ctx context.Context) {
	for {
		wait := a.schedulerTick(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// schedulerTick applies the grid as it stands at now and returns how long to
// sleep. It is one function taking the time rather than a loop reading the clock
// so that a whole week of schedule can be exercised in a test in no time at all.
func (a *App) schedulerTick(ctx context.Context, now time.Time) time.Duration {
	// Read the grid every tick rather than only on Reload: the same sqlite is
	// what `jcc-mirror schedule` writes to, and a window that only took effect
	// after a restart would be a trap. It is a handful of rows twice a minute.
	if values, err := a.store.Config(ctx); err == nil {
		a.loadSchedule(values)
	}

	cfg := a.scheduleConfig()
	local := now.In(cfg.loc)
	transfer, scan := cfg.transfer.At(local), cfg.scan.At(local)

	// The cap applies to every transfer, scheduled or not: it is there to protect
	// the link, and the link does not care who pressed the button. Only the
	// starting and stopping of runs is what "run unattended" switches off.
	a.applyWindow(ctx, transfer)

	wait := untilNext(now, transfer.Until, scan.Until)
	if !cfg.automatic {
		// A run this scheduler started belongs to it, so switching unattended runs
		// off stops the one in flight rather than leaving it going. Two closed
		// windows say exactly that.
		a.tendRun(schedule.Window{}, schedule.Window{})
		return wait
	}
	if a.tendRun(transfer, scan) {
		return wait
	}
	a.startDueRun(ctx, cfg, now, transfer, scan)
	return wait
}

// applyWindow moves the limiter to the current cell and reports the change once,
// on the edge, rather than every tick - the same discipline the notifications
// need in M6 (DESIGN.md §4.1).
func (a *App) applyWindow(ctx context.Context, w schedule.Window) {
	a.limiter.Set(w.Limit)

	a.sched.mu.Lock()
	was, seen := a.sched.window, a.sched.observed
	a.sched.window, a.sched.observed = w, true
	a.sched.mu.Unlock()

	if seen && was.Open == w.Open && was.Limit == w.Limit {
		return
	}
	if !seen {
		a.log.Infof("schedule: transfer window is %s", w.Describe())
		return
	}

	a.log.Infof("schedule: transfer window is now %s", w.Describe())
	a.Event(ctx, store.LevelInfo, store.KindWindow, "transfer window: "+w.Describe(),
		map[string]any{"open": w.Open, "limit": w.Limit, "until": w.Until})
}

// tendRun stops a scheduled run whose window has closed, and reports whether the
// runner is busy either way. Stopping costs nothing: the walk stays resumable
// and the transfer keeps its watermark, which is what makes a window boundary
// mid-transfer an ordinary event rather than a lost hour.
func (a *App) tendRun(transfer, scan schedule.Window) (busy bool) {
	a.runs.mu.Lock()
	current := a.runs.current
	if current == nil {
		a.runs.mu.Unlock()
		return false
	}
	kind, auto := current.Kind, current.Auto
	a.runs.mu.Unlock()

	if !auto {
		return true // someone is at the console; the grid is not an answer to that
	}

	switch {
	case kind == RunSync && !transfer.Open:
		_ = a.CancelRun("stopped: the transfer window closed")
	case kind == RunScan && !scan.Open:
		_ = a.CancelRun("stopped: the scan window closed")
	}
	return true
}

// startDueRun starts the first thing that is due, in the pairs' priority order.
// One at a time, as everywhere else: the transfer engine moves one file at a
// time by design, and two pairs racing for the same link would only make both
// slower.
func (a *App) startDueRun(ctx context.Context, cfg scheduleSettings, now time.Time, transfer, scan schedule.Window) {
	all, err := a.store.Pairs(ctx) // already in priority order
	if err != nil {
		a.log.Errorf("schedule: %v", err)
		return
	}

	// A pair whose remote cannot be reached is passed over rather than holding up
	// the rest: the others may read from a remote that is fine.
	pairs := make([]store.Pair, 0, len(all))
	for _, p := range all {
		if !p.Enabled {
			continue
		}
		if _, err := a.RemoteFor(p); err != nil {
			a.log.Debugf("schedule: %q cannot run yet: %v", p.Name, err)
			continue
		}
		pairs = append(pairs, p)
	}

	// Scanning first, and not only because the windows may differ: a sync plans
	// against the manifest, so a stale one means transferring yesterday's answer.
	if scan.Open {
		for _, p := range pairs {
			if !a.eligible(p, now) {
				continue
			}
			if why, due := a.scanDue(ctx, p, cfg, now); due {
				a.startScheduled(ctx, RunScan, p, why)
				return
			}
		}
	}

	if !transfer.Open {
		return
	}
	for _, p := range pairs {
		if !a.eligible(p, now) {
			continue
		}
		if why, scanID, due := a.syncDue(ctx, p); due {
			a.markPlanned(p.ID, scanID)
			a.startScheduled(ctx, RunSync, p, why)
			return
		}
	}

	// Last, because it is the cheapest thing here and the only one that comes
	// back on its own: a database the gate deferred is retried until the lock
	// clears, without anything else about the pair having changed.
	for _, p := range pairs {
		if !a.eligible(p, now) || p.Type != store.PairJCC {
			continue
		}
		if a.databaseDue(p, now) {
			a.startScheduled(ctx, RunDatabase, p, "its database was left alone last time a lock was probed")
			return
		}
	}
}

// databaseDue reports whether a deferred lock probe is worth repeating yet.
func (a *App) databaseDue(p store.Pair, now time.Time) bool {
	a.sched.mu.Lock()
	defer a.sched.mu.Unlock()

	at, ok := a.sched.dbRetry[p.ID]
	return ok && !now.Before(at)
}

// noteDatabaseRun records what the gate made of a pair's database: a deferred one
// is put on the retry clock, and anything else takes it off again.
func (a *App) noteDatabaseRun(pairID int64, res *engine.DBResult) {
	if res == nil {
		return
	}

	a.sched.mu.Lock()
	defer a.sched.mu.Unlock()

	if res.Deferred == "" {
		delete(a.sched.dbRetry, pairID)
		return
	}
	if a.sched.dbRetry == nil {
		a.sched.dbRetry = map[int64]time.Time{}
	}
	a.sched.dbRetry[pairID] = time.Now().Add(databaseRetry)
}

// eligible drops the pairs the scheduler must not touch right now: the disabled
// ones, and the ones whose last scheduled run failed recently.
func (a *App) eligible(p store.Pair, now time.Time) bool {
	if !p.Enabled {
		return false
	}
	a.sched.mu.Lock()
	defer a.sched.mu.Unlock()
	return !now.Before(a.sched.held[p.ID])
}

// scanDue reports whether the manifest is old enough to walk again. A pair that
// has never been scanned is always due: nothing else can happen until it has.
func (a *App) scanDue(ctx context.Context, p store.Pair, cfg scheduleSettings, now time.Time) (string, bool) {
	sc, ok, err := a.store.LastCompletedScan(ctx, p.ID)
	if err != nil {
		a.log.Errorf("schedule: %v", err)
		return "", false
	}
	if !ok {
		return "there is no manifest yet", true
	}
	if sc.FinishedAt == nil {
		return "", false
	}
	if age := now.Sub(*sc.FinishedAt); age >= cfg.scanEvery {
		return "the manifest is " + format.Duration(age) + " old", true
	}
	return "", false
}

// syncDue reports whether there is anything to transfer, and which walk the
// answer came from. Queued work always is; beyond that, a pair is only planned
// again once a newer walk has something to say - the join is cheap but it is not
// free, and re-running it every half minute against a manifest nothing has
// changed would buy nothing.
func (a *App) syncDue(ctx context.Context, p store.Pair) (string, int64, bool) {
	q, err := a.store.Queue(ctx, p.ID)
	if err != nil {
		a.log.Errorf("schedule: %v", err)
		return "", 0, false
	}

	sc, ok, err := a.store.LastCompletedScan(ctx, p.ID)
	if err != nil {
		a.log.Errorf("schedule: %v", err)
		return "", 0, false
	}
	if !ok {
		return "", 0, false // nothing to plan against
	}

	if n := q.Counts[store.JobPending]; n > 0 {
		return fmt.Sprintf("%s file(s) are queued, %s to go", format.Comma(n), format.Bytes(q.PendingBytes)), sc.ID, true
	}

	a.sched.mu.Lock()
	planned := a.sched.planned[p.ID]
	a.sched.mu.Unlock()

	if planned == sc.ID {
		return "", 0, false
	}
	return "there is a walk it has not been planned against", sc.ID, true
}

func (a *App) startScheduled(ctx context.Context, kind string, pair store.Pair, why string) {
	if _, err := a.startRun(ctx, kind, pair, true, false); err != nil {
		a.log.Warnf("schedule: %s of %q could not start: %v", kind, pair.Name, err)
		a.noteScheduledRun(pair.ID, true)
		return
	}
	a.log.Infof("schedule: %s of %q started - %s", kind, pair.Name, why)
}

func (a *App) markPlanned(pairID, scanID int64) {
	a.sched.mu.Lock()
	defer a.sched.mu.Unlock()
	if a.sched.planned == nil {
		a.sched.planned = map[int64]int64{}
	}
	a.sched.planned[pairID] = scanID
}

// noteScheduledRun records how a scheduled run ended. A failure holds the pair
// back for a while and forgets which walk it planned against, so the next
// attempt starts from scratch rather than concluding there was nothing to do.
// A run that was stopped - a window boundary, a shutdown - is not a failure.
func (a *App) noteScheduledRun(pairID int64, failed bool) {
	a.sched.mu.Lock()
	defer a.sched.mu.Unlock()

	if !failed {
		delete(a.sched.held, pairID)
		return
	}
	if a.sched.held == nil {
		a.sched.held = map[int64]time.Time{}
	}
	a.sched.held[pairID] = time.Now().Add(failureBackoff)
	delete(a.sched.planned, pairID)
}

// untilNext is how long to sleep: to the nearest boundary, but never longer than
// the interval and never so short that a boundary landing on the tick spins.
func untilNext(now time.Time, boundaries ...time.Time) time.Duration {
	wait := schedulerInterval
	for _, b := range boundaries {
		// Past the boundary rather than on it, so the next tick reads the cell on
		// the other side of it.
		if d := b.Sub(now) + 100*time.Millisecond; d > 0 && d < wait {
			wait = d
		}
	}
	if wait < time.Second {
		wait = time.Second
	}
	return wait
}
