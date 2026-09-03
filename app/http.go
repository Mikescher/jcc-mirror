package app

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"blackforestbytes.com/jcc-mirror/store"
)

// tokenCookie carries the bearer token for the browser, so the setup view works
// as plain HTML forms with no script. SameSite=Strict is the CSRF story: there is
// no CORS and no cross-site request that could carry it (DESIGN.md §4).
const tokenCookie = "jccmirror_token"

// Handler builds the dashboard. Both listeners - the LAN one and the one inside
// the tunnel - serve this same handler.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", a.handleHealth)
	mux.HandleFunc("GET /api/status", a.handleStatus)
	mux.HandleFunc("GET /api/config", a.handleGetConfig)
	mux.HandleFunc("GET /api/config/audit", a.handleGetAudit)
	mux.HandleFunc("GET /api/events", a.handleGetEvents)
	mux.HandleFunc("GET /api/pairs", a.handleGetPairs)
	mux.HandleFunc("GET /api/runs", a.handleGetRuns)
	mux.HandleFunc("GET /{$}", a.handleSetupPage)

	mux.HandleFunc("POST /api/login", a.handleLogin)
	mux.HandleFunc("POST /api/logout", a.handleLogout)
	mux.HandleFunc("POST /api/config", a.requireToken(a.handleSetConfig))
	mux.HandleFunc("POST /api/remote/probe", a.requireToken(a.handleProbe))

	mux.HandleFunc("POST /api/pairs", a.requireToken(a.handleCreatePair))
	mux.HandleFunc("POST /api/pairs/update", a.requireToken(a.handleUpdatePair))
	mux.HandleFunc("POST /api/pairs/delete", a.requireToken(a.handleDeletePair))
	mux.HandleFunc("POST /api/runs", a.requireToken(a.handleStartRun))
	mux.HandleFunc("POST /api/runs/cancel", a.requireToken(a.handleCancelRun))

	return noStore(mux)
}

// requireToken guards everything that changes something. An unauthenticated
// "force replace" or "approve deletion" reachable from anything that can see the
// LAN or the WG address is not an acceptable default (DESIGN.md §4, S3).
func (a *App) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authenticated(r) {
			a.fail(w, r, http.StatusUnauthorized, errors.New("this action needs the dashboard token; it is printed to the container log at startup"))
			return
		}
		next(w, r)
	}
}

func (a *App) authenticated(r *http.Request) bool {
	want, err := a.store.ConfigGet(r.Context(), store.KeyDashboardToken)
	if err != nil || want == "" {
		return false
	}

	got := ""
	if h := r.Header.Get("Authorization"); h != "" {
		if v, ok := strings.CutPrefix(h, "Bearer "); ok {
			got = strings.TrimSpace(v)
		}
	}
	if got == "" {
		if c, err := r.Cookie(tokenCookie); err == nil {
			got = c.Value
		}
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// noStore keeps the dashboard out of every cache. The whole page is live state,
// and a stale tunnel status is worse than a slow one.
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
	Key       string `json:"key"`
	Group     string `json:"group"`
	Label     string `json:"label"`
	Help      string `json:"help,omitempty"`
	Value     string `json:"value,omitempty"`
	Secret    bool   `json:"secret,omitempty"`
	Generated bool   `json:"generated,omitempty"`
	Required  bool   `json:"required,omitempty"`
	Set       bool   `json:"set"`
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
			Secret: d.Secret, Generated: d.Generated, Required: d.Required,
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
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			a.fail(w, r, http.StatusBadRequest, errors.New("since must be RFC3339"))
			return
		}
		f.Since = t
	}

	events, err := a.store.Events(r.Context(), f)
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

// handleLogin exchanges the token for a cookie, so the setup view can be plain
// HTML forms. It is not a session: the cookie holds the token itself, and there
// is nothing to invalidate but the cookie.
func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.FormValue("token"))
	want, err := a.store.ConfigGet(r.Context(), store.KeyDashboardToken)
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(want)) != 1 {
		a.fail(w, r, http.StatusUnauthorized, errors.New("that is not the token from the container log"))
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     tokenCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int((30 * 24 * time.Hour).Seconds()),
	})
	a.redirectHome(w, r)
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: tokenCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	a.redirectHome(w, r)
}

// handleSetConfig writes settings and re-applies them. Accepts a JSON object or
// a form body, so the same endpoint serves the setup view and curl.
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

	changed, err := a.store.ConfigSet(r.Context(), values, actorOf(r))
	if err != nil {
		var invalid *store.ValidationError
		code := http.StatusInternalServerError
		if errors.As(err, &invalid) || errors.Is(err, store.ErrUnknownKey) {
			code = http.StatusBadRequest
		}
		a.fail(w, r, code, err)
		return
	}

	if len(changed) > 0 {
		a.log.Infof("config: %s changed", strings.Join(changed, ", "))
		a.Event(r.Context(), store.LevelInfo, store.KindConfigChanged,
			"configuration changed: "+strings.Join(changed, ", "),
			map[string]any{"keys": changed})
		a.Reload(r.Context())
	}

	if wantsHTML(r) {
		a.redirectHome(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changed": changed, "status": a.Status(r.Context())})
}

// handleProbe lists the remote root once and records the answer as an event, so
// the result lands in the same log as everything else rather than in a flash
// message that is gone on the next page load.
func (a *App) handleProbe(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result := a.probeRemote(ctx)
	if wantsHTML(r) {
		a.redirectHome(w, r)
		return
	}
	code := http.StatusOK
	if result["error"] != nil {
		code = http.StatusBadGateway
	}
	writeJSON(w, code, result)
}

func (a *App) probeRemote(ctx context.Context) map[string]any {
	client, err := a.Remote()
	if err != nil {
		a.Event(ctx, store.LevelError, store.KindRemoteProbe, "remote probe failed: "+err.Error(), nil)
		return map[string]any{"error": err.Error()}
	}

	start := time.Now()
	entries, err := client.List(ctx, "")
	took := time.Since(start)
	if err != nil {
		a.Event(ctx, store.LevelError, store.KindRemoteProbe, "remote probe failed: "+err.Error(),
			map[string]any{"url": client.BaseURL()})
		return map[string]any{"error": err.Error(), "url": client.BaseURL()}
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
		"url": client.BaseURL(), "dirs": dirs, "files": files,
		"millis": took.Milliseconds(),
	}
	a.Event(ctx, store.LevelInfo, store.KindRemoteProbe,
		strconv.Itoa(dirs)+" directories and "+strconv.Itoa(files)+" files in the remote root", data)
	return data
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

	run, err := a.StartRun(r.Context(), strings.TrimSpace(fields["kind"]), id)
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
	if wantsHTML(r) {
		a.redirectHome(w, r)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (a *App) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	if err := a.CancelRun("stopped from the dashboard"); err != nil {
		a.fail(w, r, http.StatusConflict, err)
		return
	}
	if wantsHTML(r) {
		a.redirectHome(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopping"})
}

// readSettings pulls the settings out of a JSON or form body. A blank secret
// means "leave it alone": the setup view cannot render a stored password, so an
// untouched field must not wipe one.
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

// actorOf labels an audit row. There are no user accounts - one token, one
// operator - so the address is the only thing that distinguishes two of them.
func actorOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return "dashboard@" + host
}

// redirectHome answers a form post. Everything the setup view can do is on the
// one page, so there is nowhere else to go back to.
func (a *App) redirectHome(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// fail answers in the caller's language: a page for a browser, JSON for anything
// else.
func (a *App) fail(w http.ResponseWriter, r *http.Request, code int, err error) {
	if code >= 500 {
		a.log.Errorf("dashboard: %s %s: %v", r.Method, r.URL.Path, err)
	}
	if wantsHTML(r) {
		a.renderSetupPage(w, r, code, err.Error())
		return
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func wantsHTML(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") && !strings.Contains(r.Header.Get("Accept"), "text/html") {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

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
