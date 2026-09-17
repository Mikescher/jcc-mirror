package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/store"
)

const remoteTestKey = "0123456789abcdef0123456789abcdef"

func setRemoteKey(t *testing.T, h http.Handler, key string) {
	t.Helper()
	if rec := postForm(t, h, "/api/config", url.Values{store.KeyRemoteAPIKey: {key}}); rec.Code != http.StatusOK {
		t.Fatalf("set the remote API key: %d %s", rec.Code, rec.Body)
	}
}

func remoteGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+remoteTestKey)
	return do(t, h, req)
}

func remoteDecode[T any](t *testing.T, h http.Handler, path string) T {
	t.Helper()
	rec := remoteGet(t, h, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body)
	}
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET %s: decode: %v", path, err)
	}
	return out
}

func wantError(t *testing.T, rec *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status = %d, want %d: %s", rec.Code, code, rec.Body)
	}
	var body struct{ Error string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error == "" {
		t.Errorf("error body is not {\"error\": ...}: %s", rec.Body)
	}
}

// TestRemoteAPIIsOffWithoutAKey: an install that never set a key must not grow
// an API nobody asked for, and "off" is how the Config view takes one back.
func TestRemoteAPIIsOffWithoutAKey(t *testing.T) {
	_, h := newApp(t)

	wantError(t, remoteGet(t, h, "/api/remote/v1/status"), http.StatusNotFound)

	setRemoteKey(t, h, remoteTestKey)
	if rec := remoteGet(t, h, "/api/remote/v1/status"); rec.Code != http.StatusOK {
		t.Fatalf("with a key set: %d %s", rec.Code, rec.Body)
	}

	setRemoteKey(t, h, store.RemoteAPIOff)
	wantError(t, remoteGet(t, h, "/api/remote/v1/status"), http.StatusNotFound)

	if rec := do(t, h, httptest.NewRequest(http.MethodGet, "/api/status", nil)); rec.Code != http.StatusOK {
		t.Errorf("the LAN API answered %d once a remote key existed", rec.Code)
	}
}

func TestRemoteAPIKeyValidation(t *testing.T) {
	_, h := newApp(t)
	for _, bad := range []string{"short", "has a space in it somewhere", "tab\tseparated-key-value"} {
		if rec := postForm(t, h, "/api/config", url.Values{store.KeyRemoteAPIKey: {bad}}); rec.Code != http.StatusBadRequest {
			t.Errorf("key %q was accepted: %d", bad, rec.Code)
		}
	}
}

func TestRemoteAPIGuard(t *testing.T) {
	_, h := newApp(t)
	setRemoteKey(t, h, remoteTestKey)

	const path = "/api/remote/v1/status"

	missing := do(t, h, httptest.NewRequest(http.MethodGet, path, nil))
	wantError(t, missing, http.StatusUnauthorized)
	if missing.Header().Get("WWW-Authenticate") == "" {
		t.Error("a 401 without WWW-Authenticate")
	}

	for name, header := range map[string][2]string{
		"wrong bearer":  {"Authorization", "Bearer " + remoteTestKey + "x"},
		"wrong x-key":   {"X-Api-Key", "not-the-key-at-all-really"},
		"basic scheme":  {"Authorization", "Basic " + remoteTestKey},
		"bare key":      {"Authorization", remoteTestKey},
		"empty bearer":  {"Authorization", "Bearer "},
		"prefix of key": {"X-Api-Key", remoteTestKey[:16]},
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set(header[0], header[1])
		if rec := do(t, h, req); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, rec.Code)
		}
	}

	for name, header := range map[string][2]string{
		"bearer":           {"Authorization", "Bearer " + remoteTestKey},
		"lowercase bearer": {"Authorization", "bearer " + remoteTestKey},
		"x-api-key":        {"X-Api-Key", remoteTestKey},
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set(header[0], header[1])
		rec := do(t, h, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: %d, want 200: %s", name, rec.Code, rec.Body)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q", name, got)
		}
	}

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("X-Api-Key", remoteTestKey)
		rec := do(t, h, req)
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
			t.Errorf("%s: %d Allow=%q, want 405 Allow=GET", method, rec.Code, rec.Header().Get("Allow"))
		}
	}

	wantError(t, remoteGet(t, h, "/api/remote/v1/nothing"), http.StatusNotFound)
	if rec := do(t, h, httptest.NewRequest(http.MethodGet, "/api/remote/v1/nothing", nil)); rec.Code != http.StatusUnauthorized {
		t.Errorf("an unknown path without a key answered %d, want 401", rec.Code)
	}

	// A new key applies to the next request, with no restart in between.
	setRemoteKey(t, h, "fedcba9876543210fedcba9876543210")
	wantError(t, remoteGet(t, h, path), http.StatusUnauthorized)
}

func TestRemoteAPIStatus(t *testing.T) {
	m := newMirror(t)
	setRemoteKey(t, m.h, remoteTestKey)
	m.write(t, "Filme/a.mkv", 1024)
	m.run(t, RunScan)

	st := remoteDecode[StreamState](t, m.h, "/api/remote/v1/status")
	if st.Status.Database != "ok" || st.Status.Version != "test" {
		t.Errorf("status = %+v", st.Status)
	}
	if len(st.Runs.History) != 1 || st.Runs.History[0].Kind != RunScan {
		t.Errorf("run history = %+v", st.Runs.History)
	}
}

func TestRemoteAPIPairs(t *testing.T) {
	m := newMirror(t)
	setRemoteKey(t, m.h, remoteTestKey)
	m.write(t, "Filme/a.mkv", 1024)
	m.run(t, RunScan)

	pairs := remoteDecode[[]PairView](t, m.h, "/api/remote/v1/pairs")
	if len(pairs) != 1 || pairs[0].ID != m.pair.ID || pairs[0].RemoteFiles != 1 || pairs[0].LastScan == nil {
		t.Errorf("pairs = %+v", pairs)
	}
}

func TestRemoteAPIEvents(t *testing.T) {
	a, h := newApp(t)
	setRemoteKey(t, h, remoteTestKey)
	ctx := context.Background()
	a.Event(ctx, store.LevelError, store.KindError, "first failure", nil)
	a.Event(ctx, store.LevelError, store.KindError, "second failure", nil)

	errs := remoteDecode[[]store.Event](t, h, "/api/remote/v1/events?level=error&limit=1")
	if len(errs) != 1 || errs[0].Message != "second failure" {
		t.Errorf("newest error = %+v", errs)
	}

	future := url.QueryEscape(time.Now().Add(time.Hour).Format(time.RFC3339))
	if got := remoteDecode[[]store.Event](t, h, "/api/remote/v1/events?since="+future); len(got) != 0 {
		t.Errorf("events after an hour from now: %+v", got)
	}

	for _, q := range []string{"since=yesterday", "limit=0", "limit=x", "level=fatal", "pair=first"} {
		wantError(t, remoteGet(t, h, "/api/remote/v1/events?"+q), http.StatusBadRequest)
	}
}

func TestRemoteAPIJobs(t *testing.T) {
	m := newMirror(t)
	setRemoteKey(t, m.h, remoteTestKey)
	m.write(t, "Filme/a.mkv", 1024)
	m.write(t, "Filme/b.mkv", 1024)
	m.run(t, RunScan)
	m.run(t, RunSync)

	jobs := remoteDecode[[]store.Job](t, m.h, "/api/remote/v1/jobs?state=done&pair="+itoa(m.pair.ID))
	if len(jobs) != 2 || jobs[0].State != store.JobDone || jobs[0].BytesTotal != 1024 {
		t.Errorf("done jobs = %+v", jobs)
	}
	if got := remoteDecode[[]store.Job](t, m.h, "/api/remote/v1/jobs?state=failed"); got == nil || len(got) != 0 {
		t.Errorf("failed jobs = %#v, want an empty list", got)
	}

	for _, q := range []string{"state=stuck", "limit=-1", "pair=x"} {
		wantError(t, remoteGet(t, m.h, "/api/remote/v1/jobs?"+q), http.StatusBadRequest)
	}
}

// TestRemoteAPILogsCursor runs on a logger of its own: a started daemon logs
// from its loops whenever it likes, which no exact count would survive.
func TestRemoteAPILogsCursor(t *testing.T) {
	a := &App{log: &logs.Logger{}, started: time.Now()}
	get := func(query string) remoteLogs {
		t.Helper()
		rec := httptest.NewRecorder()
		a.handleRemoteLogs(rec, httptest.NewRequest(http.MethodGet, "/api/remote/v1/logs?"+query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("logs?%s = %d: %s", query, rec.Code, rec.Body)
		}
		var out remoteLogs
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	empty := get("")
	if empty.Lines == nil || len(empty.Lines) != 0 {
		t.Errorf("an empty ring answered %#v, want an empty list", empty.Lines)
	}

	for i := range 5 {
		a.log.Infof("line %d", i)
	}

	first := get("limit=2")
	if len(first.Lines) != 2 || first.Lines[0].Text != "line 3" || first.Lines[1].Text != "line 4" {
		t.Fatalf("the last two lines = %+v", first.Lines)
	}

	again := get("after=" + first.Cursor)
	if len(again.Lines) != 0 || again.Cursor != first.Cursor || again.Reset {
		t.Errorf("nothing new, but got %+v", again)
	}

	for i := 5; i < 8; i++ {
		a.log.Infof("line %d", i)
	}
	page := get("limit=2&after=" + first.Cursor)
	if len(page.Lines) != 2 || page.Lines[0].Text != "line 5" || page.Lines[1].Text != "line 6" || page.Reset {
		t.Fatalf("the next page = %+v", page)
	}
	rest := get("after=" + page.Cursor)
	if len(rest.Lines) != 1 || rest.Lines[0].Text != "line 7" {
		t.Errorf("the rest = %+v", rest.Lines)
	}

	// Sequences start over with every process, so a cursor from another one
	// makes the whole ring new to the client.
	stale := get("after=1.3")
	if !stale.Reset || len(stale.Lines) != 8 || stale.Lines[0].Seq != 1 {
		t.Errorf("a cursor from another process: %+v", stale)
	}

	// Lines that left the ring since the cursor are a gap the client is told about.
	for i := range 600 {
		a.log.Infof("filler %d", i)
	}
	gap := get("limit=1&after=" + rest.Cursor)
	if !gap.Reset || len(gap.Lines) != 1 || gap.Lines[0].Seq != 109 {
		t.Errorf("after a gap: %+v", gap)
	}
	if tail := get("limit=5000"); len(tail.Lines) != 500 {
		t.Errorf("the whole ring is %d lines, want 500", len(tail.Lines))
	}

	for _, q := range []string{"after=7", "after=a.b", "after=1.-1", "limit=nope"} {
		rec := httptest.NewRecorder()
		a.handleRemoteLogs(rec, httptest.NewRequest(http.MethodGet, "/api/remote/v1/logs?"+q, nil))
		wantError(t, rec, http.StatusBadRequest)
	}
}

func TestRemoteAPILogsAreServed(t *testing.T) {
	_, h := newApp(t)
	setRemoteKey(t, h, remoteTestKey)
	got := remoteDecode[remoteLogs](t, h, "/api/remote/v1/logs")
	if len(got.Lines) == 0 || got.Cursor == "" {
		t.Errorf("a started daemon has logged nothing? %+v", got)
	}
}

func TestRemoteAPIStream(t *testing.T) {
	_, h := newApp(t)
	setRemoteKey(t, h, remoteTestKey)
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for key, want := range map[string]int{"": http.StatusUnauthorized, remoteTestKey: http.StatusOK} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/remote/v1/stream", nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Header.Set("X-Api-Key", key)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("open the stream: %v", err)
		}
		if res.StatusCode != want {
			t.Errorf("key %q: %d, want %d", key, res.StatusCode, want)
		}
		if want == http.StatusOK {
			if frame := readFrame(t, res.Body); !strings.HasPrefix(frame, "event: state") {
				t.Errorf("the first frame is %q, want the state", frame)
			}
		}
		res.Body.Close()
	}
}
