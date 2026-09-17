package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"blackforestbytes.com/jcc-mirror/store"
	"blackforestbytes.com/jcc-mirror/wg"
)

// Handler builds the dashboard: the JSON API, the live stream and the built
// Angular app. Both listeners - the LAN one and the one inside the tunnel - serve
// this same handler.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", a.handleHealth)
	mux.HandleFunc("GET /api/status", a.handleStatus)
	mux.HandleFunc("GET /api/schedule", a.handleGetSchedule)
	mux.HandleFunc("GET /api/config", a.handleGetConfig)
	mux.HandleFunc("GET /api/config/audit", a.handleGetAudit)
	mux.HandleFunc("GET /api/events", a.handleGetEvents)
	mux.HandleFunc("GET /api/changes", a.handleGetChanges)
	mux.HandleFunc("GET /api/bandwidth", a.handleGetBandwidth)
	mux.HandleFunc("GET /api/pairs", a.handleGetPairs)
	mux.HandleFunc("GET /api/remotes", a.handleGetRemotes)
	mux.HandleFunc("GET /api/notify/topics", a.handleGetNotifyTopics)
	mux.HandleFunc("GET /api/notify/targets", a.handleGetNotifyTargets)
	mux.HandleFunc("GET /api/jobs", a.handleGetJobs)
	mux.HandleFunc("GET /api/scans", a.handleGetScans)
	mux.HandleFunc("GET /api/trash", a.handleGetTrash)
	mux.HandleFunc("GET /api/runs", a.handleGetRuns)
	mux.HandleFunc("GET /api/runs/dryrun", a.handleGetDryRun)
	mux.HandleFunc("GET /api/diagnostics", a.handleGetDiagnostics)
	mux.HandleFunc("GET /api/update", a.handleGetUpdate)
	mux.HandleFunc("GET /api/remote/list", a.handleRemoteList)
	mux.HandleFunc("GET /api/stream", a.handleStream)

	mux.HandleFunc("POST /api/config", a.handleSetConfig)
	mux.HandleFunc("POST /api/config/wireguard/import", a.handleImportWireguard)
	mux.HandleFunc("POST /api/remote/probe", a.handleProbe)
	mux.HandleFunc("POST /api/diagnostics/ping", a.handlePing)
	mux.HandleFunc("POST /api/diagnostics/throughput", a.handleThroughput)
	mux.HandleFunc("POST /api/notify/test", a.handleTestNotification)

	mux.HandleFunc("POST /api/remotes", a.handleCreateRemote)
	mux.HandleFunc("POST /api/remotes/update", a.handleUpdateRemote)
	mux.HandleFunc("POST /api/remotes/delete", a.handleDeleteRemote)
	mux.HandleFunc("POST /api/notify/targets", a.handleCreateNotifyTarget)
	mux.HandleFunc("POST /api/notify/targets/update", a.handleUpdateNotifyTarget)
	mux.HandleFunc("POST /api/notify/targets/delete", a.handleDeleteNotifyTarget)
	mux.HandleFunc("POST /api/pairs", a.handleCreatePair)
	mux.HandleFunc("POST /api/pairs/update", a.handleUpdatePair)
	mux.HandleFunc("POST /api/pairs/delete", a.handleDeletePair)
	mux.HandleFunc("POST /api/pairs/deletions", a.handleDecideDeletion)
	mux.HandleFunc("POST /api/pairs/database/rollback", a.handleRollbackDatabase)
	mux.HandleFunc("POST /api/trash/restore", a.handleRestoreTrash)
	mux.HandleFunc("POST /api/runs", a.handleStartRun)
	mux.HandleFunc("POST /api/runs/cancel", a.handleCancelRun)
	mux.HandleFunc("POST /api/update/check", a.handleCheckUpdate)
	mux.HandleFunc("POST /api/update/apply", a.handleApplyUpdate)
	mux.HandleFunc("POST /api/update/rollback", a.handleRollbackUpdate)

	// Everything else is the single-page app, including the deep links it routes
	// itself. It is registered last so no API path can be shadowed by it.
	mux.Handle("GET /", a.uiHandler())

	// The remote API sits in a mux of its own: it has to see every method to
	// answer 405 itself, and a method-less pattern conflicts with "GET /".
	root := http.NewServeMux()
	root.Handle(remoteAPIPrefix, a.remoteAPI())
	root.Handle("/", mux)

	return noStore(root)
}

// noStore keeps the API out of every cache: it is all live state, and a stale
// tunnel status is worse than a slow one. The static handler overwrites the
// header for the files it serves, which do not change without their bundle name
// changing with them.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// handleHealth is the container's liveness probe. It answers 503 only for the
// failures a restart could plausibly fix; see Status.Healthy.
func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	st := a.Status(r.Context())
	code := http.StatusOK
	if !st.Healthy() {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, st)
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.Status(r.Context()))
}

// configEntry is one setting as the API reports it. A secret's value is never
// sent out, not even masked into something a client might send back: Set says
// whether one is stored, which is all a UI needs to render it.
type configEntry struct {
	Key      string `json:"key"`
	Group    string `json:"group"`
	Label    string `json:"label"`
	Help     string `json:"help,omitempty"`
	Value    string `json:"value,omitempty"`
	Secret   bool   `json:"secret,omitempty"`
	Required bool   `json:"required,omitempty"`
	Set      bool   `json:"set"`
}

func (a *App) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	values, err := a.store.Config(r.Context())
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	out := make([]configEntry, 0, len(store.Keys()))
	for _, d := range store.Keys() {
		e := configEntry{
			Key: d.Name, Group: d.Group, Label: d.Label, Help: d.Help,
			Secret: d.Secret, Required: d.Required,
			Set: strings.TrimSpace(values.Get(d.Name)) != "",
		}
		if !d.Secret {
			e.Value = values.Get(d.Name)
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleGetAudit(w http.ResponseWriter, r *http.Request) {
	entries, err := a.store.ConfigAudit(r.Context(), intParam(r, "limit", 50))
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (a *App) handleGetEvents(w http.ResponseWriter, r *http.Request) {
	f := store.EventFilter{Limit: intParam(r, "limit", 100)}
	if v := r.URL.Query()["kind"]; len(v) > 0 {
		f.Kinds = v
	}
	if v := r.URL.Query()["level"]; len(v) > 0 {
		f.Levels = v
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

// handleSetConfig writes settings and re-applies them. Accepts a JSON object or
// a form body, so the same endpoint serves the dashboard and curl.
func (a *App) handleSetConfig(w http.ResponseWriter, r *http.Request) {
	values, err := readSettings(w, r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	if len(values) == 0 {
		a.fail(w, r, http.StatusBadRequest, errors.New("no settings in the request"))
		return
	}

	changed, ok := a.applyConfig(w, r, values)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changed": changed, "status": a.Status(r.Context())})
}

// handleImportWireguard takes the config file the WireGuard server generated and
// spreads it over the tunnel settings. Everything the file can say is written,
// blanks included: the file is the whole truth about the tunnel, so a preshared
// key it does not mention has to clear the one that is stored.
func (a *App) handleImportWireguard(w http.ResponseWriter, r *http.Request) {
	fields, err := readFields(w, r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	parsed, err := wg.ParseQuick(fields["config"])
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	changed, ok := a.applyConfig(w, r, map[string]string{
		store.KeyWGPrivateKey:   parsed.Config.PrivateKey,
		store.KeyWGPeerKey:      parsed.Config.PeerPublicKey,
		store.KeyWGPresharedKey: parsed.Config.PresharedKey,
		store.KeyWGEndpoint:     parsed.Config.Endpoint,
		store.KeyWGAddress:      parsed.Config.Addresses,
		store.KeyWGAllowedIPs:   parsed.Config.AllowedIPs,
		store.KeyWGDNS:          parsed.Config.DNS,
		store.KeyWGMTU:          strconv.Itoa(parsed.Config.MTU),
		store.KeyWGKeepalive:    strconv.Itoa(parsed.Config.Keepalive),
	})
	if !ok {
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"changed": changed,
		"ignored": parsed.Ignored,
		"status":  a.Status(r.Context()),
	})
}

// applyConfig is the tail every save shares: write the settings, record what
// actually moved, and re-apply it. A file pasted into the importer therefore
// lands in the audit trail and re-opens the tunnel exactly as a typed field
// does. It answers the request itself on failure and reports whether it did.
func (a *App) applyConfig(w http.ResponseWriter, r *http.Request, values map[string]string) ([]string, bool) {
	changed, err := a.store.ConfigSet(r.Context(), values, actorOf(r))
	if err != nil {
		var invalid *store.ValidationError
		code := http.StatusInternalServerError
		if errors.As(err, &invalid) || errors.Is(err, store.ErrUnknownKey) {
			code = http.StatusBadRequest
		}
		a.fail(w, r, code, err)
		return nil, false
	}

	if len(changed) > 0 {
		a.log.Infof("config: %s changed", strings.Join(changed, ", "))
		a.Event(r.Context(), store.LevelInfo, store.KindConfigChanged,
			"configuration changed: "+strings.Join(changed, ", "),
			map[string]any{"keys": changed})
		a.Reload(r.Context())
	}
	return changed, true
}

// handleProbe lists the root of one remote - the one the request names, or the
// first - and records the answer as an event, so the result lands in the same
// log as everything else rather than in a flash message that is gone on the
// next page load.
func (a *App) handleProbe(w http.ResponseWriter, r *http.Request) {
	fields, err := readFields(w, r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result, err := a.probeRemote(ctx, fields["remote"])
	code := http.StatusOK
	if err != nil {
		code = remoteErrorCode(err)
	}
	writeJSON(w, code, result)
}

func (a *App) probeRemote(ctx context.Context, ref string) (map[string]any, error) {
	rec, client, err := a.remoteFromRequest(ctx, ref)
	if err != nil {
		data := map[string]any{}
		if rec.Name != "" {
			data["remote"], data["name"] = rec.ID, rec.Name
		}
		a.Event(ctx, store.LevelError, store.KindRemoteProbe, "remote probe failed: "+err.Error(), data)
		data["error"] = err.Error()
		return data, err
	}

	start := time.Now()
	entries, err := client.List(ctx, "")
	took := time.Since(start)
	if err != nil {
		data := map[string]any{"remote": rec.ID, "name": rec.Name, "target": client.Target()}
		a.Event(ctx, store.LevelError, store.KindRemoteProbe, "remote probe failed: "+err.Error(), data)
		data["error"] = err.Error()
		return data, err
	}

	var dirs, files int
	for _, e := range entries {
		if e.IsDir {
			dirs++
		} else {
			files++
		}
	}
	data := map[string]any{
		"remote": rec.ID, "name": rec.Name, "target": client.Target(),
		"dirs": dirs, "files": files, "millis": took.Milliseconds(),
	}
	a.Event(ctx, store.LevelInfo, store.KindRemoteProbe,
		fmt.Sprintf("%d directories and %d files in the root of %q", dirs, files, rec.Name), data)
	return data, nil
}

func (a *App) handleGetRuns(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.Runs(r.Context()))
}

// handleStartRun is the button that replaces a shell in the container. It answers
// as soon as the operation has started: a scan runs for minutes and a sync for
// days, so nothing here waits for one.
func (a *App) handleStartRun(w http.ResponseWriter, r *http.Request) {
	fields, err := readFields(w, r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	id, err := strconv.ParseInt(strings.TrimSpace(fields["pair"]), 10, 64)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, errors.New("which pair? send its id as `pair`"))
		return
	}

	// The override is only ever the lock gate's, and only ever a person's: a
	// scheduled run never sets it (DESIGN.md §3).
	force, _ := strconv.ParseBool(strings.TrimSpace(fields["force"]))

	run, err := a.StartRun(r.Context(), strings.TrimSpace(fields["kind"]), id, force)
	if err != nil {
		// 409 is the interesting one - something else is already running. A pair
		// that is not there is an ordinary mistake and should not read like one.
		code := http.StatusConflict
		if errors.Is(err, store.ErrNoPair) {
			code = http.StatusNotFound
		}
		a.fail(w, r, code, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (a *App) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	if err := a.CancelRun("stopped from the dashboard"); err != nil {
		a.fail(w, r, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopping"})
}

// readSettings pulls the settings out of a JSON or form body. A blank secret
// means "leave it alone": the dashboard is never sent a stored password, so an
// untouched field comes back empty and must not wipe one.
func readSettings(w http.ResponseWriter, r *http.Request) (map[string]string, error) {
	out := map[string]string{}

	if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/json") {
		var body map[string]string
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			return nil, err
		}
		for k, v := range body {
			out[k] = v
		}
	} else {
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		for k, v := range r.PostForm {
			if _, ok := store.Key(k); !ok {
				continue // the form also carries its own fields
			}
			out[k] = v[0]
		}
	}

	for k, v := range out {
		def, ok := store.Key(k)
		if ok && def.Secret && strings.TrimSpace(v) == "" {
			delete(out, k)
		}
	}
	return out, nil
}

// actorOf labels an audit row. There are no user accounts, so the address is the
// only thing that distinguishes two operators.
func actorOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return "dashboard@" + host
}

// fail answers an error the way everything here answers: as JSON with a message
// meant to be read, since the dashboard shows it verbatim.
func (a *App) fail(w http.ResponseWriter, r *http.Request, code int, err error) {
	if code >= 500 {
		a.log.Errorf("dashboard: %s %s: %v", r.Method, r.URL.Path, err)
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// intParam reads a positive query parameter, falling back to def for anything
// missing or unreadable. Zero is not a row limit anyone means, so it falls back
// too.
func intParam(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// intParam64 is intParam for a cursor rather than a limit, so zero is a legal
// value: it means "everything the ring still holds".
func intParam64(r *http.Request, name string, def int64) int64 {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return def
	}
	return n
}
