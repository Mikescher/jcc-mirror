package app

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"blackforestbytes.com/jcc-mirror/engine"
	"blackforestbytes.com/jcc-mirror/store"
)

// dryRunPage is how many entries one request returns when it does not say.
const dryRunPage = 200

// dryRunList is one finished dry run with every entry of its plan, sorted by
// what would happen and then by path.
type dryRunList struct {
	StartedAt  time.Time
	FinishedAt time.Time
	Plan       engine.Plan
	Entries    []engine.PlanEntry
}

// DryRunPage is one page of a dry run's list. Total counts the entries the
// filter matches, not the whole plan; the plan's own counters are the whole.
type DryRunPage struct {
	PairID     int64              `json:"pairId"`
	PairName   string             `json:"pair"`
	StartedAt  time.Time          `json:"startedAt"`
	FinishedAt time.Time          `json:"finishedAt"`
	Plan       engine.Plan        `json:"plan"`
	Total      int                `json:"total"`
	Offset     int                `json:"offset"`
	Entries    []engine.PlanEntry `json:"entries"`
}

var opOrder = map[string]int{store.OpAdd: 0, store.OpReplace: 1, store.OpDelete: 2}

// keepDryRun replaces the pair's stored list. The caller holds the runner's lock.
func (a *App) keepDryRun(run *Run, p engine.Plan) {
	entries := p.Entries
	sort.SliceStable(entries, func(i, j int) bool {
		if oi, oj := opOrder[entries[i].Op], opOrder[entries[j].Op]; oi != oj {
			return oi < oj
		}
		return entries[i].Path < entries[j].Path
	})

	head := p
	head.Entries = nil
	if a.runs.dryRuns == nil {
		a.runs.dryRuns = map[int64]*dryRunList{}
	}
	a.runs.dryRuns[run.PairID] = &dryRunList{
		StartedAt: run.StartedAt, FinishedAt: *run.FinishedAt, Plan: head, Entries: entries,
	}
}

// handleGetDryRun pages through the latest dry run of one pair. Filters: op
// (repeatable), q (a case-insensitive substring of the path), offset, limit.
func (a *App) handleGetDryRun(w http.ResponseWriter, r *http.Request) {
	id, ok, err := pairParam(r)
	if err != nil || !ok {
		a.fail(w, r, http.StatusBadRequest, errors.New("which pair? send its id as `pair`"))
		return
	}

	a.runs.mu.Lock()
	list := a.runs.dryRuns[id]
	a.runs.mu.Unlock()
	if list == nil {
		a.fail(w, r, http.StatusNotFound, errors.New("this pair has no finished dry run since the daemon started"))
		return
	}

	ops := map[string]bool{}
	for _, op := range r.URL.Query()["op"] {
		ops[op] = true
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	offset := int(intParam64(r, "offset", 0))
	limit := intParam(r, "limit", dryRunPage)

	// The list is never written to after it is stored, so it is read unlocked.
	out := DryRunPage{
		PairID: id, PairName: list.Plan.PairName, StartedAt: list.StartedAt, FinishedAt: list.FinishedAt,
		Plan: list.Plan, Offset: offset, Entries: []engine.PlanEntry{},
	}
	for _, e := range list.Entries {
		if len(ops) > 0 && !ops[e.Op] {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(e.Path), q) {
			continue
		}
		if out.Total >= offset && len(out.Entries) < limit {
			out.Entries = append(out.Entries, e)
		}
		out.Total++
	}
	writeJSON(w, http.StatusOK, out)
}
