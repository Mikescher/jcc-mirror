package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"blackforestbytes.com/jcc-mirror/store"
)

func postJSON(t *testing.T, h http.Handler, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	return do(t, h, req)
}

func createRemote(t *testing.T, h http.Handler, fields map[string]any) store.Remote {
	t.Helper()
	rec := postJSON(t, h, "/api/remotes", fields)
	if rec.Code != http.StatusOK {
		t.Fatalf("create remote: %d %s", rec.Code, rec.Body)
	}
	var rem store.Remote
	if err := json.Unmarshal(rec.Body.Bytes(), &rem); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return rem
}

func getRemotes(t *testing.T, h http.Handler) []RemoteView {
	t.Helper()
	rec := do(t, h, httptest.NewRequest(http.MethodGet, "/api/remotes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/remotes = %d: %s", rec.Code, rec.Body)
	}
	var out []RemoteView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

var nas = map[string]any{
	"name": "nas", "host": "10.13.13.2", "share": "media", "user": "ro", "password": "hunter2",
}

// TestRemoteRoundTrip: the password goes in and never comes out, and a form that
// leaves it blank keeps it.
func TestRemoteRoundTrip(t *testing.T) {
	a, h := newApp(t)
	ctx := context.Background()

	var bodies []string
	keep := func(rec *httptest.ResponseRecorder) *httptest.ResponseRecorder {
		bodies = append(bodies, rec.Body.String())
		return rec
	}

	rem := createRemote(t, h, nas)
	if !rem.PasswordSet || rem.ID == 0 {
		t.Fatalf("created remote = %+v", rem)
	}

	rec := keep(do(t, h, httptest.NewRequest(http.MethodGet, "/api/remotes", nil)))
	if !strings.Contains(rec.Body.String(), `"pairs": []`) {
		t.Errorf("a remote no pair reads from lists its pairs as something other than []: %s", rec.Body)
	}
	views := getRemotes(t, h)
	if len(views) != 1 || views[0].Target != `\\10.13.13.2\media` || views[0].Error != "" {
		t.Fatalf("remotes = %+v", views)
	}

	rec = keep(postJSON(t, h, "/api/remotes/update", map[string]any{"id": rem.ID, "path": "Filme", "password": ""}))
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", rec.Code, rec.Body)
	}
	stored, err := a.Store().RemoteByID(ctx, rem.ID)
	if err != nil {
		t.Fatalf("RemoteByID: %v", err)
	}
	if stored.Password != "hunter2" || stored.Path != "Filme" || stored.Host != "10.13.13.2" {
		t.Errorf("after a blank password: %+v (password %q)", stored, stored.Password)
	}

	if rec := postJSON(t, h, "/api/remotes/update", map[string]any{"id": rem.ID, "port": 445}); rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown field = %d, want 400: %s", rec.Code, rec.Body)
	}

	for _, path := range []string{"/api/status", "/api/events", "/api/config", "/api/pairs"} {
		keep(do(t, h, httptest.NewRequest(http.MethodGet, path, nil)))
	}
	for i, body := range bodies {
		if strings.Contains(body, "hunter2") {
			t.Errorf("response %d leaked the password: %s", i, body)
		}
	}

	if rec := postJSON(t, h, "/api/remotes/update", map[string]any{"id": rem.ID, "clearPassword": true}); rec.Code != http.StatusOK {
		t.Fatalf("clear = %d: %s", rec.Code, rec.Body)
	}
	if stored, _ := a.Store().RemoteByID(ctx, rem.ID); stored.Password != "" || stored.PasswordSet {
		t.Errorf("clearPassword left %q", stored.Password)
	}

	if rec := postJSON(t, h, "/api/remotes/delete", map[string]any{"id": rem.ID}); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body)
	}
	if views := getRemotes(t, h); len(views) != 0 {
		t.Errorf("after the delete: %+v", views)
	}
	if _, err := a.Remote(rem.ID); err == nil {
		t.Error("the removed remote still has a client")
	}

	events, err := a.Store().Events(ctx, store.EventFilter{Kinds: []string{store.KindRemoteChanged}})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 4 {
		t.Errorf("got %d remote events, want 4", len(events))
	}
}

func TestRemoteInUseIsNotRemoved(t *testing.T) {
	a, h := newApp(t)

	rem := createRemote(t, h, nas)
	rec := postJSON(t, h, "/api/pairs", map[string]any{
		"name": "media", "localPath": t.TempDir(), "remoteId": rem.ID,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("add pair: %d %s", rec.Code, rec.Body)
	}
	var pair store.Pair
	if err := json.Unmarshal(rec.Body.Bytes(), &pair); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pair.RemoteID != rem.ID {
		t.Errorf("pair reads from remote %d, want %d", pair.RemoteID, rem.ID)
	}

	views, err := a.PairViews(context.Background())
	if err != nil || len(views) != 1 || views[0].RemoteName != "nas" {
		t.Errorf("pair views = %+v (err %v)", views, err)
	}
	if got := getRemotes(t, h); len(got) != 1 || len(got[0].Pairs) != 1 || got[0].Pairs[0] != "media" {
		t.Errorf("remotes = %+v", got)
	}

	if rec := postJSON(t, h, "/api/remotes/delete", map[string]any{"id": rem.ID}); rec.Code != http.StatusConflict {
		t.Errorf("delete of a remote in use = %d, want 409: %s", rec.Code, rec.Body)
	}
	if rec := postJSON(t, h, "/api/remotes/delete", map[string]any{"id": rem.ID + 100}); rec.Code != http.StatusNotFound {
		t.Errorf("delete of an unknown remote = %d, want 404: %s", rec.Code, rec.Body)
	}

	rec = postJSON(t, h, "/api/pairs", map[string]any{
		"name": "other", "localPath": t.TempDir(), "remoteId": rem.ID + 100,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a pair on a remote that does not exist = %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestPairWithNoRemoteDoesNotRun(t *testing.T) {
	a, h := newApp(t)

	rec := postJSON(t, h, "/api/pairs", map[string]any{"name": "media", "localPath": t.TempDir(), "enabled": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("add pair: %d %s", rec.Code, rec.Body)
	}
	var pair store.Pair
	if err := json.Unmarshal(rec.Body.Bytes(), &pair); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if _, err := a.RemoteFor(pair); err == nil || !strings.Contains(err.Error(), "reads from no remote") {
		t.Errorf("RemoteFor a pair with no remote = %v", err)
	}
	if _, err := a.StartRun(context.Background(), RunScan, pair.ID, false); err == nil {
		t.Error("a scan of a pair with no remote was started")
	}
}

// TestEditingOneRemoteKeepsTheOthersSession: a running sync holds its remote's
// client, and a save on the Remotes tab for a different remote must not close it.
func TestEditingOneRemoteKeepsTheOthersSession(t *testing.T) {
	a, h := newApp(t)

	first := createRemote(t, h, nas)
	second := createRemote(t, h, map[string]any{"name": "backup", "host": "10.13.13.4", "share": "archive", "user": "ro"})

	before1, err := a.Remote(first.ID)
	if err != nil {
		t.Fatalf("Remote(first): %v", err)
	}
	before2, err := a.Remote(second.ID)
	if err != nil {
		t.Fatalf("Remote(second): %v", err)
	}

	if rec := postJSON(t, h, "/api/remotes/update", map[string]any{"id": second.ID, "share": "archiv"}); rec.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", rec.Code, rec.Body)
	}
	if rec := postJSON(t, h, "/api/remotes/update", map[string]any{"id": first.ID, "name": "publisher"}); rec.Code != http.StatusOK {
		t.Fatalf("rename = %d: %s", rec.Code, rec.Body)
	}

	after1, _ := a.Remote(first.ID)
	after2, _ := a.Remote(second.ID)
	if after1 != before1 {
		t.Error("editing one remote, or renaming this one, replaced its client")
	}
	if after2 == before2 {
		t.Error("the edited remote kept a client built from its old settings")
	}
	if got := after2.Target(); got != `\\10.13.13.4\archiv` {
		t.Errorf("the edited remote reads %s", got)
	}
}

func TestStatusListsEveryRemote(t *testing.T) {
	dir := t.TempDir()
	a, h := newAppWithRemote(t, dir)
	ctx := context.Background()

	st := a.Status(ctx)
	if len(st.Remotes) != 1 || st.Remotes[0].ID != 0 || st.Remotes[0].Name != localRemoteName || st.Remotes[0].Target != dir {
		t.Fatalf("with only the local directory: %+v", st.Remotes)
	}

	rem := createRemote(t, h, nas)
	st = a.Status(ctx)
	if len(st.Remotes) != 1 || st.Remotes[0].ID != rem.ID || st.Remotes[0].Target != dir {
		t.Errorf("with a remote stored: %+v", st.Remotes)
	}
}
