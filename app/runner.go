package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"blackforestbytes.com/jcc-mirror/engine"
	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/store"
)

// The operations the dashboard can start. They are the mirror commands of the
// CLI: the Synology's container shell is not somewhere an operator can easily
// get to, and the bootstrap adoption in particular has to happen exactly once,
// before anything else works.
const (
	RunPlan   = "plan"
	RunScan   = "scan"
	RunAdopt  = "adopt"
	RunSync   = "sync"
	RunDelete = "delete"
)

// runHistory is how many finished runs the page keeps. They live in memory only:
// the durable record is the events table, and this is just what the operator who
// pressed the button wants to read afterwards.
const runHistory = 6

// planSample is how many entries a plan started from the dashboard lists.
const planSample = 50

// Run is one operation, running or finished.
type Run struct {
	Kind       string       `json:"kind"`
	Auto       bool         `json:"auto,omitempty"` // started by the scheduler rather than by a person
	PairID     int64        `json:"pairId"`
	PairName   string       `json:"pair"`
	StartedAt  time.Time    `json:"startedAt"`
	FinishedAt *time.Time   `json:"finishedAt,omitempty"`
	Summary    string       `json:"summary,omitempty"`
	Error      string       `json:"error,omitempty"`
	Plan       *engine.Plan `json:"plan,omitempty"`
}

// Done reports whether the run has ended.
func (r Run) Done() bool { return r.FinishedAt != nil }

// Elapsed is how long the run has been going, or how long it took.
func (r Run) Elapsed() time.Duration {
	if r.FinishedAt != nil {
		return r.FinishedAt.Sub(r.StartedAt)
	}
	return time.Since(r.StartedAt)
}

// runner holds the one operation that may be in flight. One at a time is not a
// limitation to work around later: the transfer engine moves one file at a time
// by design, and the scheduler walks the pairs in priority order for the same
// reason.
type runner struct {
	mu      sync.Mutex
	current *Run
	engine  *engine.Engine
	cancel  context.CancelFunc
	reason  string // why the current run was stopped, for the record it leaves
	history []Run
}

// RunState is everything the dashboard shows about the mirror's activity.
type RunState struct {
	Current  *Run            `json:"current,omitempty"`
	Progress engine.Progress `json:"progress,omitempty"`
	Scan     *store.Scan     `json:"scan,omitempty"` // live counters of a running walk
	History  []Run           `json:"history,omitempty"`
}

// Busy reports whether an operation is in flight, which is what the page uses to
// decide whether to refresh itself.
func (s RunState) Busy() bool { return s.Current != nil }

// StartRun begins one operation in the background and returns as soon as it has
// started. The HTTP request that started it is long gone by the time a sync
// finishes - a transfer of the collection runs for days.
func (a *App) StartRun(ctx context.Context, kind string, pairID int64) (Run, error) {
	pair, err := a.store.PairByID(ctx, pairID)
	if err != nil {
		return Run{}, err
	}
	return a.startRun(ctx, kind, pair, false)
}

// startRun is StartRun with the pair already resolved, and with auto saying who
// asked. The scheduler only ever stops its own runs: a sync someone started by
// hand is a person at the console who wants bytes moved now, and a window
// boundary is not an answer to that.
func (a *App) startRun(ctx context.Context, kind string, pair store.Pair, auto bool) (Run, error) {
	switch kind {
	case RunPlan, RunScan, RunAdopt, RunSync, RunDelete:
	default:
		return Run{}, fmt.Errorf("unknown operation %q", kind)
	}
	if !pair.Enabled {
		return Run{}, fmt.Errorf("pair %q is disabled", pair.Name)
	}

	eng, err := a.newEngine(ctx)
	if err != nil {
		return Run{}, err
	}
	// Read outside the runner's lock: everything that reaches for both takes this
	// one second, and there is no reason for the exception to exist.
	base := a.runContext()

	a.runs.mu.Lock()
	defer a.runs.mu.Unlock()

	if c := a.runs.current; c != nil {
		return Run{}, fmt.Errorf("%s of %q has been running for %s; stop it first or wait for it",
			c.Kind, c.PairName, format.Duration(c.Elapsed()))
	}

	run := &Run{Kind: kind, Auto: auto, PairID: pair.ID, PairName: pair.Name, StartedAt: time.Now()}
	runCtx, cancel := context.WithCancel(base)
	a.runs.current, a.runs.engine, a.runs.cancel, a.runs.reason = run, eng, cancel, ""

	if !auto {
		a.log.Infof("run: %s of %q started from the dashboard", kind, pair.Name)
	}
	go a.execute(runCtx, eng, pair, run)

	return *run, nil
}

// newEngine builds an engine on the settings and the remote as they are now. It
// is rebuilt per run rather than held: a configuration change replaces the
// tunnel, and with it the transport every request rides on.
func (a *App) newEngine(ctx context.Context) (*engine.Engine, error) {
	client, err := a.Remote()
	if err != nil {
		return nil, err
	}
	values, err := a.store.Config(ctx)
	if err != nil {
		return nil, err
	}
	opts := engine.OptionsFrom(values)
	// The limiter is the daemon's, not the run's: the scheduler changes the cap
	// at a window boundary and the transfer already in flight has to feel it.
	opts.Limiter = a.limiter
	return engine.New(a.store, client, a.log, opts)
}

// runContext is what a run's lifetime hangs off: the daemon's own context, so a
// docker stop ends a transfer rather than orphaning it. Ending one costs
// nothing - every job keeps its resume watermark.
func (a *App) runContext() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.baseCtx == nil {
		return context.Background()
	}
	return a.baseCtx
}

func (a *App) execute(ctx context.Context, eng *engine.Engine, pair store.Pair, run *Run) {
	summary, plan, err := runOperation(ctx, eng, pair, run.Kind)

	a.runs.mu.Lock()
	defer a.runs.mu.Unlock()

	finished := time.Now()
	stopped := errors.Is(err, context.Canceled)
	run.FinishedAt = &finished
	run.Summary, run.Plan = summary, plan

	switch {
	case err == nil:
		a.log.Infof("run: %s of %q finished: %s", run.Kind, run.PairName, summary)
	case stopped && a.runs.reason != "":
		// A run that was stopped on purpose is not a failure, and reads better as
		// the reason it was stopped than as "context canceled".
		run.Error = a.runs.reason
		a.log.Infof("run: %s of %q %s", run.Kind, run.PairName, a.runs.reason)
	default:
		run.Error = err.Error()
		a.log.Errorf("run: %s of %q: %v", run.Kind, run.PairName, err)
	}

	a.runs.history = append([]Run{*run}, a.runs.history...)
	if len(a.runs.history) > runHistory {
		a.runs.history = a.runs.history[:runHistory]
	}
	a.runs.current, a.runs.engine, a.runs.cancel, a.runs.reason = nil, nil, nil, ""

	if run.Auto {
		a.noteScheduledRun(run.PairID, err != nil && !stopped)
	}
}

// runOperation is the switch the buttons come down to. The engine writes
// the durable record itself, so all that is wanted back is a line to read.
func runOperation(ctx context.Context, eng *engine.Engine, pair store.Pair, kind string) (string, *engine.Plan, error) {
	switch kind {
	case RunPlan:
		p, err := eng.Plan(ctx, pair, planSample)
		if err != nil {
			return "", nil, err
		}
		return planSummary(p), &p, nil

	case RunScan:
		res, err := eng.Scan(ctx, pair, true)
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("%s directories, %s files, %s; %s manifest rows swept",
			format.Comma(res.Scan.Dirs), format.Comma(res.Scan.Files),
			format.Bytes(res.Scan.Bytes), format.Comma(res.Swept)), nil, nil

	case RunAdopt:
		res, err := eng.Adopt(ctx, pair)
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("%s files matched on size (%s); %s differ in size, %s missing here, %s extra",
			format.Comma(int64(res.Matched)), format.Bytes(res.MatchedBytes),
			format.Comma(int64(res.SizeMismatch)), format.Comma(int64(res.Missing)),
			format.Comma(int64(res.Extra))), nil, nil

	case RunSync:
		p, err := eng.Enqueue(ctx, pair)
		if err != nil {
			return "", nil, err
		}
		res, syncErr := eng.Sync(ctx, pair)
		summary := fmt.Sprintf("%s queued; %s files transferred (%s), %s already here, %s failed, %s still queued",
			format.Comma(int64(p.Queued)), format.Comma(int64(res.Files)), format.Bytes(res.Bytes),
			format.Comma(int64(res.InPlace)), format.Comma(int64(res.Failed)), format.Comma(res.Pending))
		if syncErr != nil {
			return summary, nil, syncErr
		}

		// The deletion phase, and only after a sync that finished: the tree shrinks
		// once the additions have landed, never before (DESIGN.md §2.5).
		reaped, err := eng.Reap(ctx, pair)
		return summary + "; " + reapSummary(reaped), nil, err

	case RunDelete:
		res, err := eng.Reap(ctx, pair)
		if err != nil {
			return "", nil, err
		}
		return reapSummary(res), nil, nil
	}
	return "", nil, fmt.Errorf("unknown operation %q", kind)
}

func planSummary(p engine.Plan) string {
	out := fmt.Sprintf("%s to add (%s), %s to replace (%s), %s vanished (%s, %s)",
		format.Comma(int64(p.Add)), format.Bytes(p.AddBytes),
		format.Comma(int64(p.Replace)), format.Bytes(p.ReplaceBytes),
		format.Comma(int64(p.Vanished)), format.Bytes(p.VanishedBytes), vanishedFate(p))
	if p.Shortfall > 0 {
		out += fmt.Sprintf(" — and it does not fit: %s short of the free-space reserve", format.Bytes(p.Shortfall))
	}
	return out
}

// vanishedFate is what a sync would do with the files the publisher no longer
// has, in the few words a summary line has room for.
func vanishedFate(p engine.Plan) string {
	switch {
	case p.Mode != store.ModeMirror:
		return "kept: the pair is additive"
	case p.Vanished == 0:
		return "nothing to delete"
	case p.Guard != "" && !p.Approved:
		return "held for approval: " + p.Guard
	default:
		return "to be quarantined"
	}
}

// reapSummary is the deletion phase in one line.
func reapSummary(r engine.ReapResult) string {
	switch {
	case r.Blocked != nil:
		return fmt.Sprintf("deletion of %s file(s) is waiting for approval: %s",
			format.Comma(r.Blocked.Files), r.Blocked.Reason)
	case r.Skipped != "":
		return "nothing deleted — " + r.Skipped
	default:
		out := fmt.Sprintf("%s file(s) quarantined (%s)", format.Comma(int64(r.Deleted)), format.Bytes(r.Bytes))
		if r.Failed > 0 {
			out += fmt.Sprintf(", %s could not be moved", format.Comma(int64(r.Failed)))
		}
		return out
	}
}

// CancelRun stops whatever is running, and records reason as what the run says
// it ended for. Stopping is routine and safe: a scan is left resumable and a
// transfer keeps its watermark, so the next run carries on rather than starting
// over.
func (a *App) CancelRun(reason string) error {
	a.runs.mu.Lock()
	cancel, current := a.runs.cancel, a.runs.current
	if cancel != nil {
		a.runs.reason = reason
	}
	a.runs.mu.Unlock()

	if cancel == nil {
		return errors.New("nothing is running")
	}
	a.log.Infof("run: %s of %q %s", current.Kind, current.PairName, reason)
	cancel()
	return nil
}

// Runs is what the dashboard reads.
func (a *App) Runs(ctx context.Context) RunState {
	a.runs.mu.Lock()
	st := RunState{History: append([]Run(nil), a.runs.history...)}
	if a.runs.current != nil {
		current := *a.runs.current
		st.Current = &current
		if a.runs.engine != nil {
			st.Progress = a.runs.engine.Progress()
		}
	}
	a.runs.mu.Unlock()

	// A walk reports itself through its scan row, which PutListing moves per
	// directory - there is nothing in the engine's progress for it to read.
	if st.Current != nil && st.Current.Kind == RunScan {
		if sc, ok, err := a.store.RunningScan(ctx, st.Current.PairID); err == nil && ok {
			st.Scan = &sc
		}
	}
	return st
}
