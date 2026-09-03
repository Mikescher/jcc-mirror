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
	RunPlan  = "plan"
	RunScan  = "scan"
	RunAdopt = "adopt"
	RunSync  = "sync"
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
// by design, and M3's scheduler walks the pairs in priority order for the same
// reason.
type runner struct {
	mu      sync.Mutex
	current *Run
	engine  *engine.Engine
	cancel  context.CancelFunc
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
	switch kind {
	case RunPlan, RunScan, RunAdopt, RunSync:
	default:
		return Run{}, fmt.Errorf("unknown operation %q", kind)
	}

	pair, err := a.store.PairByID(ctx, pairID)
	if err != nil {
		return Run{}, err
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

	run := &Run{Kind: kind, PairID: pair.ID, PairName: pair.Name, StartedAt: time.Now()}
	runCtx, cancel := context.WithCancel(base)
	a.runs.current, a.runs.engine, a.runs.cancel = run, eng, cancel

	a.log.Infof("run: %s of %q started from the dashboard", kind, pair.Name)
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
	return engine.New(a.store, client, a.log, engine.OptionsFrom(values))
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
	run.FinishedAt = &finished
	run.Summary, run.Plan = summary, plan
	if err != nil {
		run.Error = err.Error()
		a.log.Errorf("run: %s of %q: %v", run.Kind, run.PairName, err)
	} else {
		a.log.Infof("run: %s of %q finished: %s", run.Kind, run.PairName, summary)
	}

	a.runs.history = append([]Run{*run}, a.runs.history...)
	if len(a.runs.history) > runHistory {
		a.runs.history = a.runs.history[:runHistory]
	}
	a.runs.current, a.runs.engine, a.runs.cancel = nil, nil, nil
}

// runOperation is the switch the four buttons come down to. The engine writes
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
		res, err := eng.Sync(ctx, pair)
		summary := fmt.Sprintf("%s queued; %s files transferred (%s), %s already here, %s failed, %s still queued",
			format.Comma(int64(p.Queued)), format.Comma(int64(res.Files)), format.Bytes(res.Bytes),
			format.Comma(int64(res.InPlace)), format.Comma(int64(res.Failed)), format.Comma(res.Pending))
		return summary, nil, err
	}
	return "", nil, fmt.Errorf("unknown operation %q", kind)
}

func planSummary(p engine.Plan) string {
	return fmt.Sprintf("%s to add (%s), %s to replace (%s), %s vanished (%s, never deleted before M4)",
		format.Comma(int64(p.Add)), format.Bytes(p.AddBytes),
		format.Comma(int64(p.Replace)), format.Bytes(p.ReplaceBytes),
		format.Comma(int64(p.Vanished)), format.Bytes(p.VanishedBytes))
}

// CancelRun stops whatever is running. Stopping is routine and safe: a scan is
// left resumable and a transfer keeps its watermark, so the next run carries on
// rather than starting over.
func (a *App) CancelRun() error {
	a.runs.mu.Lock()
	cancel, current := a.runs.cancel, a.runs.current
	a.runs.mu.Unlock()

	if cancel == nil {
		return errors.New("nothing is running")
	}
	a.log.Infof("run: stopping %s of %q on request", current.Kind, current.PairName)
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
