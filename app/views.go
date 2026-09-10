package app

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"blackforestbytes.com/jcc-mirror/engine"
	"blackforestbytes.com/jcc-mirror/store"
)

// The read views the dashboard is drawn from beyond status and config: the
// per-file history, the transfer queue, the walks and the quarantine. Every one
// of them is a plain GET (DESIGN.md §4).

func (a *App) handleGetChanges(w http.ResponseWriter, r *http.Request) {
	f := store.ChangeFilter{Limit: intParam(r, "limit", 200)}
	if v := r.URL.Query()["op"]; len(v) > 0 {
		f.Ops = v
	}
	if id, ok, err := pairParam(r); err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	} else if ok {
		f.PairID = &id
	}
	if since, ok, err := sinceParam(r); err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	} else if ok {
		f.Since = since
	}

	changes, err := a.store.Changes(r.Context(), f)
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, changes)
}

func (a *App) handleGetJobs(w http.ResponseWriter, r *http.Request) {
	f := store.JobFilter{Limit: intParam(r, "limit", 200)}
	if v := r.URL.Query()["state"]; len(v) > 0 {
		f.States = v
	}
	if id, ok, err := pairParam(r); err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	} else if ok {
		f.PairID = &id
	}

	jobs, err := a.store.Jobs(r.Context(), f)
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

// handleGetScans is the walk history: how long a scan took and what it found,
// which is what the scan schedule is actually set from (DESIGN.md §2.3).
func (a *App) handleGetScans(w http.ResponseWriter, r *http.Request) {
	id, ok, err := pairParam(r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	if !ok {
		a.fail(w, r, http.StatusBadRequest, errors.New("which pair? send its id as `pair`"))
		return
	}

	scans, err := a.store.Scans(r.Context(), id, intParam(r, "limit", 20))
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, scans)
}

// TrashView is one pair's quarantine: what a deletion moved aside and how long it
// has left before it is really gone (DESIGN.md §2.5).
type TrashView struct {
	PairID int64             `json:"pairId"`
	Pair   string            `json:"pair"`
	Days   []engine.TrashDay `json:"days"`
}

func (a *App) handleGetTrash(w http.ResponseWriter, r *http.Request) {
	pairs, err := a.store.Pairs(r.Context())
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	values, err := a.store.Config(r.Context())
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	retention := values.Duration(store.KeyDeleteRetention)

	only, filtered, err := pairParam(r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	out := []TrashView{}
	for _, p := range pairs {
		if filtered && p.ID != only {
			continue
		}
		days, err := engine.Trash(p, retention)
		if err != nil {
			a.log.Errorf("dashboard: trash of %q: %v", p.Name, err)
			continue
		}
		if len(days) == 0 {
			continue
		}
		out = append(out, TrashView{PairID: p.ID, Pair: p.Name, Days: days})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRestoreTrash puts one quarantined file back. It is the other half of
// deleting into a dated directory rather than unlinking: without a way back the
// quarantine is only a slower delete.
func (a *App) handleRestoreTrash(w http.ResponseWriter, r *http.Request) {
	fields, err := readFields(w, r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	pair, err := a.pairFromFields(r.Context(), fields)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	path := strings.TrimSpace(fields["path"])
	if path == "" {
		a.fail(w, r, http.StatusBadRequest, errors.New("which file? send its pair-relative path as `path`"))
		return
	}

	res, err := engine.RestoreTrash(pair, strings.TrimSpace(fields["day"]), path)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	// Deliberately no files row: what came back is on disk but is not local truth
	// until a scan has seen the publisher still has it.
	a.log.Infof("trash: %s restored to %q by %s", res.Path, pair.Name, actorOf(r))
	id := pair.ID
	a.eventFor(r.Context(), &id, store.LevelWarn, store.KindTrashRestored,
		res.Path+" was restored from the quarantine",
		map[string]any{"path": res.Path, "from": res.From, "size": res.Size})
	writeJSON(w, http.StatusOK, res)
}

// pairParam reads the optional `pair` filter every list view accepts.
func pairParam(r *http.Request) (id int64, ok bool, err error) {
	raw := strings.TrimSpace(r.URL.Query().Get("pair"))
	if raw == "" {
		return 0, false, nil
	}
	id, err = strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false, errors.New("pair must be an id")
	}
	return id, true, nil
}

func sinceParam(r *http.Request) (t time.Time, ok bool, err error) {
	raw := strings.TrimSpace(r.URL.Query().Get("since"))
	if raw == "" {
		return time.Time{}, false, nil
	}
	t, err = time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false, errors.New("since must be RFC3339")
	}
	return t, true, nil
}
