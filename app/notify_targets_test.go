package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"blackforestbytes.com/jcc-mirror/notify"
	"blackforestbytes.com/jcc-mirror/store"
)

func createNotifyTarget(t *testing.T, h http.Handler, fields map[string]any) store.NotifyTarget {
	t.Helper()
	rec := postJSON(t, h, "/api/notify/targets", fields)
	if rec.Code != http.StatusOK {
		t.Fatalf("create notification target: %d %s", rec.Code, rec.Body)
	}
	var target store.NotifyTarget
	if err := json.Unmarshal(rec.Body.Bytes(), &target); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return target
}

func getNotifyTargets(t *testing.T, h http.Handler) (string, []store.NotifyTarget) {
	t.Helper()
	rec := do(t, h, httptest.NewRequest(http.MethodGet, "/api/notify/targets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/notify/targets = %d: %s", rec.Code, rec.Body)
	}
	var out []store.NotifyTarget
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return rec.Body.String(), out
}

func TestNotifyTopicsAreListed(t *testing.T) {
	_, h := newApp(t)

	rec := do(t, h, httptest.NewRequest(http.MethodGet, "/api/notify/topics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/notify/topics = %d: %s", rec.Code, rec.Body)
	}
	var topics []struct {
		Key, Label, Help string
		Default          bool
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &topics); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(topics) != len(store.NotifyTopics) || topics[0].Key != "sync_failed" || !topics[0].Default ||
		topics[1].Key != "sync_ok" || topics[1].Default || topics[0].Label == "" || topics[0].Help == "" {
		t.Errorf("topics = %+v", topics)
	}
}

// TestNotifyTargetRoundTrip: the user key goes in and never comes out, a blank
// one keeps what is stored, and only the fields an update carries change.
func TestNotifyTargetRoundTrip(t *testing.T) {
	a, h := newApp(t)
	ctx := context.Background()

	body, targets := getNotifyTargets(t, h)
	if strings.TrimSpace(body) != "[]" || len(targets) != 0 {
		t.Errorf("no targets listed as %s", body)
	}

	if rec := postJSON(t, h, "/api/notify/targets", map[string]any{"name": "x", "userId": "7"}); rec.Code != http.StatusBadRequest {
		t.Errorf("no user key = %d, want 400", rec.Code)
	}
	if rec := postJSON(t, h, "/api/notify/targets", map[string]any{"name": "x", "userId": "7", "userKey": "k", "events": []string{"nope"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown topic = %d, want 400", rec.Code)
	}

	created := createNotifyTarget(t, h, map[string]any{
		"name": "phone", "userId": 7, "userKey": "hunter2", "channel": "mirror", "sender": "",
		"enabled": true, "events": []string{"update", "sync_ok"},
	})
	if created.ID == 0 || !created.UserKeySet || created.Sender != store.DefaultNotifySender ||
		!slices.Equal(created.Events, []string{"sync_ok", "update"}) {
		t.Fatalf("created = %+v", created)
	}

	if rec := postJSON(t, h, "/api/notify/targets", map[string]any{"name": "phone", "userId": "8", "userKey": "k"}); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "already exists") {
		t.Errorf("a duplicate name = %d %s", rec.Code, rec.Body)
	}

	rec := postJSON(t, h, "/api/notify/targets/update", map[string]any{
		"id": created.ID, "userKey": "", "enabled": false, "events": []string{},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body)
	}
	stored, err := a.Store().NotifyTargetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("NotifyTargetByID: %v", err)
	}
	if stored.UserKey != "hunter2" || stored.Enabled || len(stored.Events) != 0 ||
		stored.Channel != "mirror" || stored.Name != "phone" {
		t.Errorf("after update = %+v", stored)
	}

	if rec := postJSON(t, h, "/api/notify/targets/update", map[string]any{"id": created.ID, "userKey": "new"}); rec.Code != http.StatusOK {
		t.Fatalf("update key = %d %s", rec.Code, rec.Body)
	}
	if stored, _ = a.Store().NotifyTargetByID(ctx, created.ID); stored.UserKey != "new" {
		t.Errorf("user key after replacing it = %q", stored.UserKey)
	}

	if rec := postJSON(t, h, "/api/notify/targets/update", map[string]any{"id": 999, "name": "x"}); rec.Code != http.StatusNotFound {
		t.Errorf("update of an unknown id = %d, want 404", rec.Code)
	}
	if rec := postJSON(t, h, "/api/notify/targets/update", map[string]any{"name": "x"}); rec.Code != http.StatusBadRequest {
		t.Errorf("update without an id = %d, want 400", rec.Code)
	}

	body, _ = getNotifyTargets(t, h)
	for _, secret := range []string{"hunter2", `"new"`} {
		if strings.Contains(body, secret) {
			t.Errorf("the user key was listed: %s", body)
		}
	}

	rec = postJSON(t, h, "/api/notify/targets/delete", map[string]any{"id": created.ID})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"phone"`) {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	if rec := postJSON(t, h, "/api/notify/targets/delete", map[string]any{"id": created.ID}); rec.Code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", rec.Code)
	}

	events, err := a.Store().Events(ctx, store.EventFilter{Kinds: []string{store.KindNotifyChanged}})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 4 {
		t.Errorf("got %d notify.changed events, want 4 (add, 2 changes, remove)", len(events))
	}
	for _, e := range events {
		if strings.Contains(e.Message, "hunter2") || strings.Contains(string(mustJSON(e.Data)), "hunter2") {
			t.Errorf("an event carries the user key: %+v", e)
		}
	}
}

// scnRecorder is an SCN endpoint that remembers which account got which title.
type scnRecorder struct {
	mu   sync.Mutex
	got  map[string][]string
	srv  *httptest.Server
	fail bool
}

func newSCNRecorder(t *testing.T) *scnRecorder {
	t.Helper()
	rec := &scnRecorder{got: map[string][]string{}}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec.mu.Lock()
		defer rec.mu.Unlock()
		uid, _ := body["user_id"].(string)
		title, _ := body["title"].(string)
		rec.got[uid] = append(rec.got[uid], title)
		if rec.fail {
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *scnRecorder) titles(uid string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := slices.Clone(r.got[uid])
	slices.Sort(out)
	return out
}

// TestEachTargetGetsOnlyItsTopics: two accounts with different topics each
// receive their own kinds and nothing else.
func TestEachTargetGetsOnlyItsTopics(t *testing.T) {
	a, h := newApp(t)
	ctx := context.Background()
	scn := newSCNRecorder(t)
	a.notifier = &notify.Client{Endpoint: scn.srv.URL}

	createNotifyTarget(t, h, map[string]any{
		"name": "ops", "userId": "1", "userKey": "k1", "events": []string{"sync_failed", "tunnel_down"},
	})
	createNotifyTarget(t, h, map[string]any{
		"name": "fan", "userId": "2", "userKey": "k2", "events": []string{"sync_ok", "update", "tunnel_down"},
	})

	a.Notify(ctx, store.NotifySyncFailed, 1, "failed", "")
	a.Notify(ctx, store.NotifySyncOK, 1, "ok", "")
	a.Notify(ctx, store.NotifyUpdateRolledBack, 0, "rolled back", "")
	a.Notify(ctx, store.NotifyTunnelDown, 0, "tunnel", "")
	a.Notify(ctx, store.NotifyDBReplaced, 1, "db", "")
	a.bg.Wait()

	if got, want := scn.titles("1"), []string{"failed", "tunnel"}; !slices.Equal(got, want) {
		t.Errorf("ops received %v, want %v", got, want)
	}
	if got, want := scn.titles("2"), []string{"ok", "rolled back", "tunnel"}; !slices.Equal(got, want) {
		t.Errorf("fan received %v, want %v", got, want)
	}
}

func TestFailedNotificationNamesTheTarget(t *testing.T) {
	a, h := newApp(t)
	ctx := context.Background()
	scn := newSCNRecorder(t)
	scn.fail = true
	a.notifier = &notify.Client{Endpoint: scn.srv.URL}

	createNotifyTarget(t, h, map[string]any{"name": "flaky", "userId": "1", "userKey": "k"})
	a.Notify(ctx, store.NotifySyncFailed, 0, "failed", "")
	a.bg.Wait()

	events, err := a.Store().Events(ctx, store.EventFilter{Kinds: []string{store.KindError}})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if !slices.ContainsFunc(events, func(e store.Event) bool { return strings.Contains(e.Message, `"flaky"`) }) {
		t.Errorf("no failure event names the target: %+v", events)
	}
}

// TestTestNotificationGoesToOneTarget: the test button reaches exactly the target
// it names, even one that is switched off and has no topics.
func TestTestNotificationGoesToOneTarget(t *testing.T) {
	a, h := newApp(t)
	scn := newSCNRecorder(t)
	a.notifier = &notify.Client{Endpoint: scn.srv.URL}

	createNotifyTarget(t, h, map[string]any{"name": "other", "userId": "1", "userKey": "k"})
	off := createNotifyTarget(t, h, map[string]any{
		"name": "off", "userId": "2", "userKey": "k", "enabled": false, "events": "",
	})

	rec := postJSON(t, h, "/api/notify/test", map[string]any{"id": off.ID})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sent": true`) {
		t.Fatalf("test = %d %s", rec.Code, rec.Body)
	}
	if got := scn.titles("2"); len(got) != 1 {
		t.Errorf("the named target received %v", got)
	}
	if got := scn.titles("1"); len(got) != 0 {
		t.Errorf("another target received %v", got)
	}

	if rec := postJSON(t, h, "/api/notify/test", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Errorf("test without an id = %d, want 400", rec.Code)
	}
	if rec := postJSON(t, h, "/api/notify/test", map[string]any{"id": 999}); rec.Code != http.StatusNotFound {
		t.Errorf("test of an unknown id = %d, want 404", rec.Code)
	}

	scn.mu.Lock()
	scn.fail = true
	scn.mu.Unlock()
	if rec := postJSON(t, h, "/api/notify/test", map[string]any{"id": off.ID}); rec.Code != http.StatusBadGateway {
		t.Errorf("test against a failing SCN = %d, want 502", rec.Code)
	}
}
