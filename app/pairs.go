package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"blackforestbytes.com/jcc-mirror/engine"
	"blackforestbytes.com/jcc-mirror/store"
)

// PairView is a pair plus the numbers that say how far behind it is. The counts
// are three cheap aggregates rather than a diff: a plan is a join over the whole
// manifest and is not something to run on every page load.
type PairView struct {
	store.Pair
	RemoteName  string `json:"remoteName,omitempty"`
	RemoteFiles int64
	RemoteBytes int64
	LocalFiles  int64
	LocalBytes  int64
	Queue       store.JobQueue
	LastScan    *time.Time
	Space       engine.Space

	// Approval is the deletion this pair is waiting to be allowed to make. It is
	// on the page rather than in an inbox because a mirror that has stopped
	// deleting is otherwise invisible until someone reads the event log
	// (DESIGN.md §2.5).
	Approval *store.DeleteApproval

	// Backups are the kept copies of a jcc pair's database, newest first. They
	// are read from the table rather than probed over the tunnel: what the
	// publisher holds and whether a lock is out is a question with a cost, and
	// the answer belongs to the db run rather than to a page load (DESIGN.md §3).
	Backups []store.DBBackup
}

// IncludesText and ExcludesText render the globs for the form field they are
// edited in.
func (p PairView) IncludesText() string { return strings.Join(p.Includes, ", ") }
func (p PairView) ExcludesText() string { return strings.Join(p.Excludes, ", ") }

// Behind is how many files the publisher has that are not recorded here. It is
// an estimate from the two totals, not a diff, so it says nothing about files
// that changed - but it is the number that answers "is this pair keeping up".
func (p PairView) Behind() int64 {
	if n := p.RemoteFiles - p.LocalFiles; n > 0 {
		return n
	}
	return 0
}

// PairViews collects every pair with its state.
func (a *App) PairViews(ctx context.Context) ([]PairView, error) {
	pairs, err := a.store.Pairs(ctx)
	if err != nil {
		return nil, err
	}
	remotes, err := a.store.Remotes(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[int64]string, len(remotes))
	for _, r := range remotes {
		names[r.ID] = r.Name
	}

	out := make([]PairView, 0, len(pairs))
	for _, p := range pairs {
		v := PairView{Pair: p, RemoteName: names[p.RemoteID]}
		if v.RemoteFiles, v.RemoteBytes, err = a.store.ManifestStats(ctx, p.ID); err != nil {
			return nil, err
		}
		if v.LocalFiles, v.LocalBytes, err = a.store.FileStats(ctx, p.ID); err != nil {
			return nil, err
		}
		if v.Queue, err = a.store.Queue(ctx, p.ID); err != nil {
			return nil, err
		}
		// The volume, not the directory: what a sync is refused for is the room
		// left where the files land (DESIGN.md §2.6).
		if space, err := engine.FreeSpace(p.LocalPath); err == nil {
			v.Space = space
		}
		if sc, ok, err := a.store.LastCompletedScan(ctx, p.ID); err != nil {
			return nil, err
		} else if ok {
			v.LastScan = sc.FinishedAt
		}
		if ap, ok, err := a.store.OpenApproval(ctx, p.ID); err != nil {
			return nil, err
		} else if ok {
			v.Approval = &ap
		}
		if p.Type == store.PairJCC {
			if v.Backups, err = a.store.DBBackups(ctx, p.ID, 0); err != nil {
				return nil, err
			}
		}
		out = append(out, v)
	}
	return out, nil
}

func (a *App) handleGetPairs(w http.ResponseWriter, r *http.Request) {
	views, err := a.PairViews(r.Context())
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (a *App) handleCreatePair(w http.ResponseWriter, r *http.Request) {
	fields, err := readFields(w, r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	var p store.Pair
	if err := applyPairFields(&p, fields); err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	if err := a.store.CreatePair(r.Context(), &p); err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	a.log.Infof("pairs: %q added", p.Name)
	a.pairEvent(r.Context(), p, "added")
	a.respondPair(w, r, p)
}

func (a *App) handleUpdatePair(w http.ResponseWriter, r *http.Request) {
	fields, err := readFields(w, r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	p, err := a.pairFromFields(r.Context(), fields)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	// Only what the request carried is written back. A form posts every field, so
	// it replaces the lot; a JSON caller can send one key and change one thing.
	if err := applyPairFields(&p, fields); err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	if err := a.store.UpdatePair(r.Context(), &p); err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	a.log.Infof("pairs: %q changed", p.Name)
	a.pairEvent(r.Context(), p, "changed")
	a.respondPair(w, r, p)
}

func (a *App) handleDeletePair(w http.ResponseWriter, r *http.Request) {
	fields, err := readFields(w, r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	p, err := a.pairFromFields(r.Context(), fields)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	// Before the delete: an event's pair_id is set to null rather than cascaded,
	// so the row outlives the pair it describes.
	a.pairEvent(r.Context(), p, "removed")
	if err := a.store.DeletePair(r.Context(), p.ID); err != nil {
		a.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	a.log.Infof("pairs: %q removed; nothing under %s was touched", p.Name, p.LocalPath)
	a.respondPair(w, r, p)
}

// pairFromFields loads the pair an id in the request names.
func (a *App) pairFromFields(ctx context.Context, fields map[string]string) (store.Pair, error) {
	raw, ok := fields["id"]
	if !ok {
		return store.Pair{}, errors.New("no pair id in the request")
	}
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return store.Pair{}, fmt.Errorf("pair id %q is not a number", raw)
	}
	return a.store.PairByID(ctx, id)
}

// applyPairFields writes the fields the request carried onto a pair, leaving the
// rest as they were.
func applyPairFields(p *store.Pair, fields map[string]string) error {
	for key, value := range fields {
		value = strings.TrimSpace(value)
		switch key {
		case "id":
		case "name":
			p.Name = value
		case "type":
			p.Type = value
		case "remoteId":
			if value == "" {
				p.RemoteID = 0
				continue
			}
			id, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return fmt.Errorf("remoteId must be a remote's id, not %q", value)
			}
			p.RemoteID = id
		case "remotePath":
			p.RemotePath = value
		case "localPath":
			p.LocalPath = value
		case "mode":
			p.Mode = value
		case "owner":
			p.Owner = value
		case "fileMode":
			p.FileMode = value
		case "dirMode":
			p.DirMode = value
		case "includes":
			p.Includes = splitPatterns(value)
		case "excludes":
			p.Excludes = splitPatterns(value)
		case "priority", "deleteGuard":
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("%s must be a number, not %q", key, value)
			}
			if key == "priority" {
				p.Priority = n
			} else {
				p.DeleteGuard = n
			}
		case "enabled":
			b, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("enabled must be true or false, not %q", value)
			}
			p.Enabled = b
		default:
			return fmt.Errorf("unknown pair field %q", key)
		}
	}
	return nil
}

// splitPatterns reads a comma-separated list of globs. A glob therefore cannot
// contain a comma, which rules out a directory with one in its name - the same
// trade the CLI's -exclude makes.
func splitPatterns(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (a *App) pairEvent(ctx context.Context, p store.Pair, what string) {
	id := p.ID
	a.eventFor(ctx, &id, store.LevelInfo, store.KindPairChanged,
		fmt.Sprintf("pair %q %s from the dashboard", p.Name, what),
		map[string]any{
			"action": what, "id": p.ID, "name": p.Name, "type": p.Type, "mode": p.Mode,
			"remoteId": p.RemoteID, "remotePath": p.RemotePath, "localPath": p.LocalPath,
			"owner": p.Owner, "fileMode": p.FileMode, "dirMode": p.DirMode, "enabled": p.Enabled,
		})
}

func (a *App) respondPair(w http.ResponseWriter, _ *http.Request, p store.Pair) {
	writeJSON(w, http.StatusOK, p)
}

// readFields pulls a JSON object or a form body into a flat map of what was
// actually sent, so "absent" and "set to empty" stay distinguishable.
//
// The last value of a repeated field wins, which is what makes a checkbox work:
// an unchecked one sends nothing at all, so the form pairs it with a hidden
// false in front of it.
func readFields(w http.ResponseWriter, r *http.Request) (map[string]string, error) {
	out := map[string]string{}

	if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/json") {
		var body map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			return nil, err
		}
		for k, v := range body {
			switch value := v.(type) {
			case string:
				out[k] = value
			case bool:
				out[k] = strconv.FormatBool(value)
			case float64:
				out[k] = strconv.FormatInt(int64(value), 10)
			case []any:
				parts := make([]string, 0, len(value))
				for _, item := range value {
					parts = append(parts, fmt.Sprint(item))
				}
				out[k] = strings.Join(parts, ",")
			default:
				return nil, fmt.Errorf("field %q has a value this endpoint cannot read", k)
			}
		}
		return out, nil
	}

	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	for k, values := range r.PostForm {
		if len(values) > 0 {
			out[k] = values[len(values)-1]
		}
	}
	return out, nil
}
