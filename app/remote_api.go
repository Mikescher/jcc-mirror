package app

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/store"
)

// remoteAPIPrefix is the read-only API for clients outside the LAN. Unlike the
// dashboard's own routes it is behind a key, and it only ever answers GET.
const remoteAPIPrefix = "/api/remote/v1/"

const (
	remoteEventsLimit = 100
	remoteEventsMax   = 1000
	remoteJobsLimit   = 100
	remoteJobsMax     = 1000
	remoteLogsLimit   = 200
	remoteLogsMax     = 2000
)

// remoteAPI is the sub-router under remoteAPIPrefix, already behind its guard.
func (a *App) remoteAPI() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(remoteAPIPrefix+"status", a.handleRemoteStatus)
	mux.HandleFunc(remoteAPIPrefix+"pairs", a.handleGetPairs)
	mux.HandleFunc(remoteAPIPrefix+"events", a.handleRemoteEvents)
	mux.HandleFunc(remoteAPIPrefix+"jobs", a.handleRemoteJobs)
	mux.HandleFunc(remoteAPIPrefix+"logs", a.handleRemoteLogs)
	mux.HandleFunc(remoteAPIPrefix+"stream", a.handleStream)
	mux.HandleFunc(remoteAPIPrefix, func(w http.ResponseWriter, r *http.Request) {
		a.fail(w, r, http.StatusNotFound, errors.New("no such endpoint"))
	})
	return a.remoteGuard(mux)
}

// remoteGuard lets a request through only with the configured key. The key is
// read on every request, so a key changed in the Config view applies at once.
func (a *App) remoteGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")

		key, err := a.store.ConfigGet(r.Context(), store.KeyRemoteAPIKey)
		if err != nil {
			a.fail(w, r, http.StatusInternalServerError, err)
			return
		}
		if key == "" || key == store.RemoteAPIOff {
			a.fail(w, r, http.StatusNotFound, errors.New("remote API disabled"))
			return
		}

		if !keyMatches(requestKey(r), key) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="jcc-mirror"`)
			a.fail(w, r, http.StatusUnauthorized, errors.New("missing or wrong API key"))
			return
		}

		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			a.fail(w, r, http.StatusMethodNotAllowed, errors.New("the remote API is read-only; only GET is allowed"))
			return
		}

		next.ServeHTTP(w, r)
	})
}

func requestKey(r *http.Request) string {
	if v := r.Header.Get("X-Api-Key"); v != "" {
		return strings.TrimSpace(v)
	}
	scheme, token, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if ok && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(token)
	}
	return ""
}

// keyMatches compares digests rather than the keys themselves, because
// ConstantTimeCompare returns early when the lengths differ.
func keyMatches(got, want string) bool {
	if got == "" {
		return false
	}
	g, w := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1
}

func (a *App) handleRemoteStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.streamState(r.Context()))
}

func (a *App) handleRemoteEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.EventFilter{Kinds: q["kind"], Levels: q["level"]}

	var err error
	if f.Limit, err = limitParam(r, remoteEventsLimit, remoteEventsMax); err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	for _, l := range f.Levels {
		if !slices.Contains([]string{store.LevelDebug, store.LevelInfo, store.LevelWarn, store.LevelError}, l) {
			a.fail(w, r, http.StatusBadRequest, fmt.Errorf("unknown level %q", l))
			return
		}
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

	events, err := a.store.Events(r.Context(), f)
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (a *App) handleRemoteJobs(w http.ResponseWriter, r *http.Request) {
	f := store.JobFilter{States: r.URL.Query()["state"]}

	var err error
	if f.Limit, err = limitParam(r, remoteJobsLimit, remoteJobsMax); err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	for _, s := range f.States {
		if !slices.Contains([]string{store.JobPending, store.JobRunning, store.JobVerifying, store.JobDone, store.JobFailed}, s) {
			a.fail(w, r, http.StatusBadRequest, fmt.Errorf("unknown job state %q", s))
			return
		}
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
	if jobs == nil {
		jobs = []store.Job{}
	}
	writeJSON(w, http.StatusOK, jobs)
}

// remoteLogs is one page of the log tail. Reset says the lines do not continue
// from the cursor that was sent: it came from another process, or lines between
// it and the oldest one still held have been dropped.
type remoteLogs struct {
	Lines  []logs.Line `json:"lines"`
	Cursor string      `json:"cursor"`
	Reset  bool        `json:"reset,omitempty"`
}

// handleRemoteLogs pages through the in-memory log ring. The cursor is
// "<boot>.<seq>": boot is the process start in Unix milliseconds, seq the last
// line handed out. Sequences restart at 1 with every process, so the boot part
// is what tells a stale cursor from a current one.
func (a *App) handleRemoteLogs(w http.ResponseWriter, r *http.Request) {
	limit, err := limitParam(r, remoteLogsLimit, remoteLogsMax)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	boot := a.started.UnixMilli()
	all, newest := a.log.Tail(0)
	out := remoteLogs{Lines: []logs.Line{}}

	raw := strings.TrimSpace(r.URL.Query().Get("after"))
	if raw == "" {
		if len(all) > limit {
			all = all[len(all)-limit:]
		}
		out.Lines = append(out.Lines, all...)
		out.Cursor = logCursor(boot, newest)
		writeJSON(w, http.StatusOK, out)
		return
	}

	cursorBoot, after, err := parseLogCursor(raw)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	if cursorBoot != boot || after > newest {
		out.Reset = true
		after = 0
	} else if len(all) > 0 && after < all[0].Seq-1 {
		out.Reset = true
	}

	for _, l := range all {
		if l.Seq <= after {
			continue
		}
		if len(out.Lines) == limit {
			break
		}
		out.Lines = append(out.Lines, l)
	}
	if n := len(out.Lines); n > 0 {
		after = out.Lines[n-1].Seq
	}
	out.Cursor = logCursor(boot, after)
	writeJSON(w, http.StatusOK, out)
}

func logCursor(boot, seq int64) string {
	return strconv.FormatInt(boot, 10) + "." + strconv.FormatInt(seq, 10)
}

func parseLogCursor(s string) (boot, seq int64, err error) {
	b, q, ok := strings.Cut(s, ".")
	if ok {
		boot, err = strconv.ParseInt(b, 10, 64)
	}
	if ok && err == nil {
		seq, err = strconv.ParseInt(q, 10, 64)
	}
	if !ok || err != nil || seq < 0 {
		return 0, 0, errors.New("after must be a cursor from a previous logs response")
	}
	return boot, seq, nil
}

// limitParam is intParam for the remote API: a limit that is not a positive
// number is refused rather than defaulted, and one above max is capped.
func limitParam(r *http.Request, def, ceiling int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, errors.New("limit must be a positive number")
	}
	return min(n, ceiling), nil
}
