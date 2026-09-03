package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"blackforestbytes.com/jcc-mirror/notify"
	"blackforestbytes.com/jcc-mirror/store"
)

// TestDashboardIsServedFromTheBinary: the deployment is one file and one volume,
// so the Angular build has to come out of the binary - and a deep link the
// browser asks for directly has to answer with the shell rather than a 404, or
// every route but the first is broken on a reload (DESIGN.md §4).
func TestDashboardIsServedFromTheBinary(t *testing.T) {
	_, h, _ := newApp(t)

	root := do(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
	if root.Code != http.StatusOK {
		t.Fatalf("GET / = %d", root.Code)
	}
	if !strings.Contains(root.Body.String(), "<app-root>") {
		t.Fatalf("GET / did not answer with the dashboard shell: %.200s", root.Body)
	}
	if got := root.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("the shell is served %q; it names the bundles and must never be the stale half", got)
	}

	deep := do(t, h, httptest.NewRequest(http.MethodGet, "/diagnostics", nil))
	if deep.Code != http.StatusOK || deep.Body.String() != root.Body.String() {
		t.Errorf("a deep link answered %d with something other than the shell", deep.Code)
	}

	bundle := do(t, h, httptest.NewRequest(http.MethodGet, "/main.js", nil))
	if bundle.Code != http.StatusOK {
		t.Fatalf("GET /main.js = %d; was the dashboard built?", bundle.Code)
	}
	if bundle.Header().Get("ETag") == "" {
		t.Error("a bundle is served without an ETag, so every load re-sends it whole")
	}

	// The API must not be shadowed by the catch-all that answers everything else.
	if api := do(t, h, httptest.NewRequest(http.MethodGet, "/api/status", nil)); api.Code != http.StatusOK ||
		!strings.HasPrefix(api.Header().Get("Content-Type"), "application/json") {
		t.Errorf("GET /api/status = %d %q", api.Code, api.Header().Get("Content-Type"))
	}
}

// TestReadViewsAreOpenAndAnswerJSON walks the views the dashboard is drawn from.
// None of them needs the token: reading is open and only changing anything is not
// (DESIGN.md §4, S3).
func TestReadViewsAreOpenAndAnswerJSON(t *testing.T) {
	m := newMirror(t)
	m.write(t, "Filme/a.mkv", 2048)
	m.run(t, RunScan)
	m.run(t, RunSync)

	pair := "?pair=" + itoa(m.pair.ID)
	for _, path := range []string{
		"/api/session", "/api/status", "/api/schedule", "/api/config", "/api/config/audit",
		"/api/events", "/api/changes", "/api/pairs", "/api/runs", "/api/jobs",
		"/api/trash", "/api/diagnostics", "/api/bandwidth?span=minute", "/api/scans" + pair,
	} {
		rec := do(t, m.h, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d: %s", path, rec.Code, rec.Body)
			continue
		}
		var any any
		if err := json.Unmarshal(rec.Body.Bytes(), &any); err != nil {
			t.Errorf("GET %s did not answer JSON: %v", path, err)
		}
	}

	// The sync moved bytes over a real HTTP connection, so the meter the
	// bandwidth series is sampled from must have seen them.
	if got := m.app.meter.in.Load(); got < 2048 {
		t.Errorf("the transport meter counted %d bytes in; the transfer alone was 2048", got)
	}
}

// TestTheLogTailNeedsTheToken is the hole the log tail would otherwise open: the
// container log is where the dashboard token is printed, so serving it back to an
// unauthenticated reader hands over everything the token guards (DESIGN.md §4, S3).
func TestTheLogTailNeedsTheToken(t *testing.T) {
	a, h, token := newApp(t)
	a.log.Infof("something worth reading")

	open := do(t, h, httptest.NewRequest(http.MethodGet, "/api/diagnostics", nil))
	var view DiagnosticsView
	if err := json.Unmarshal(open.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(view.Log) != 0 || !view.LogLocked {
		t.Fatalf("an unauthenticated read got %d log lines (locked=%v)", len(view.Log), view.LogLocked)
	}

	// A fresh value: logLocked is omitted when false, so decoding over the first
	// answer would leave it set and the assertion would pass for the wrong reason.
	var unlocked DiagnosticsView
	req := httptest.NewRequest(http.MethodGet, "/api/diagnostics", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if err := json.Unmarshal(do(t, h, req).Body.Bytes(), &unlocked); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(unlocked.Log) == 0 || unlocked.LogLocked {
		t.Fatalf("an authenticated read got %d log lines (locked=%v)", len(unlocked.Log), unlocked.LogLocked)
	}

	// And the token is never in the ring in the first place, so a future reader of
	// the tail cannot find it there either.
	lines, _ := a.log.Tail(0)
	for _, line := range lines {
		if strings.Contains(line.Text, token) {
			t.Fatalf("the log ring holds the dashboard token: %q", line.Text)
		}
	}
}

// TestTheStreamAppliesTheSameLogGate: the live stream carries the same log the
// diagnostics view gates, so gating one and not the other would leave the door
// open next to the lock (DESIGN.md §4, S3).
//
// Both connections are open at once on purpose. The feed publishes one frame to
// every subscriber, so this is the gate itself under test rather than two
// separately-timed reads that could each pass for the wrong reason.
func TestTheStreamAppliesTheSameLogGate(t *testing.T) {
	a, h, token := newApp(t)
	a.log.Infof("a line only an operator should read")

	srv := httptest.NewServer(h)
	defer srv.Close()

	var wg sync.WaitGroup
	var open, authed string
	var openErr, authedErr error

	wg.Add(2)
	go func() { defer wg.Done(); open, openErr = readStream(srv.URL, "") }()
	go func() { defer wg.Done(); authed, authedErr = readStream(srv.URL, token) }()
	wg.Wait()

	if openErr != nil || authedErr != nil {
		t.Fatalf("reading the streams: %v, %v", openErr, authedErr)
	}
	if !strings.Contains(open, "event: state") {
		t.Fatalf("an unauthenticated stream carried nothing at all:\n%s", open)
	}
	if strings.Contains(open, "an operator should read") {
		t.Fatalf("an unauthenticated stream carried the log:\n%s", open)
	}
	if !strings.Contains(authed, "an operator should read") {
		t.Fatalf("an authenticated stream did not carry the log:\n%s", authed)
	}
}

// readStream opens the event stream and reads until the log arrives or the
// deadline runs out. A stream that will never carry the log reads for the whole
// window, which is the point: it has to be given every chance to leak it.
func readStream(base, token string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/stream", nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	var got strings.Builder
	buf := make([]byte, 4096)
	for {
		n, _ := res.Body.Read(buf)
		got.Write(buf[:n])
		if n == 0 || strings.Contains(got.String(), "event: log") {
			return got.String(), nil
		}
	}
}

// TestChangesRecordWhatLanded: the Changes view is per-file history, and it is
// the only place a single file's fate is recorded.
func TestChangesRecordWhatLanded(t *testing.T) {
	m := newMirror(t)
	m.write(t, "Filme/a.mkv", 2048)
	m.run(t, RunScan)
	m.run(t, RunSync)

	rec := do(t, m.h, httptest.NewRequest(http.MethodGet, "/api/changes", nil))
	var changes []store.Change
	if err := json.Unmarshal(rec.Body.Bytes(), &changes); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "Filme/a.mkv" || changes[0].Op != store.ChangeAdd {
		t.Fatalf("changes = %+v, want one add of Filme/a.mkv", changes)
	}

	filtered := do(t, m.h, httptest.NewRequest(http.MethodGet, "/api/changes?op=delete", nil))
	if err := json.Unmarshal(filtered.Body.Bytes(), &changes); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("filtering to deletions returned %d rows", len(changes))
	}
}

// TestScheduleViewDrawsTheWholeWeek: the grid is what the schedule is edited as,
// so the API has to hand back all 168 cells and the rules they came from.
func TestScheduleViewDrawsTheWholeWeek(t *testing.T) {
	_, h, token := newApp(t)

	form := url.Values{store.KeySchedule: {"* * = 5MiB; mon-fri 2-8 = full; sat 0-24 = off"}}
	if rec := postForm(t, h, "/api/config", token, form); rec.Code != http.StatusOK {
		t.Fatalf("save the schedule: %d %s", rec.Code, rec.Body)
	}

	rec := do(t, h, httptest.NewRequest(http.MethodGet, "/api/schedule", nil))
	var view ScheduleView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(view.Transfer.Cells) != 7 || len(view.Transfer.Cells[0]) != 24 {
		t.Fatalf("the grid is %d days of %d hours", len(view.Transfer.Cells), len(view.Transfer.Cells[0]))
	}
	if view.Transfer.Days[0] != "Monday" || view.Transfer.Days[6] != "Sunday" {
		t.Errorf("the week runs %v; it has to start on Monday", view.Transfer.Days)
	}
	if cell := view.Transfer.Cells[0][3]; !cell.Open || cell.Limit != 0 {
		t.Errorf("Monday 03:00 = %+v, want open with no cap", cell)
	}
	if cell := view.Transfer.Cells[0][12]; !cell.Open || cell.Limit != 5<<20 {
		t.Errorf("Monday 12:00 = %+v, want open capped at 5 MiB/s", cell)
	}
	if cell := view.Transfer.Cells[5][12]; cell.Open {
		t.Errorf("Saturday 12:00 = %+v, want closed", cell)
	}
	if !strings.Contains(view.Transfer.Rules, "5MiB") {
		t.Errorf("the rules came back as %q", view.Transfer.Rules)
	}
}

// TestBandwidthFoldsIntoTheHeatmap: the series is bucketed in UTC and the heatmap
// is drawn in the configured timezone, which is a setting of its own rather than
// the container's TZ (DESIGN.md §6).
func TestBandwidthFoldsIntoTheHeatmap(t *testing.T) {
	a, h, _ := newApp(t)
	ctx := context.Background()

	// 22:30 UTC on a Sunday is 23:30 on the Sunday in Berlin - the same day, but
	// the hour a naive reading would get wrong.
	at := time.Date(2026, 3, 1, 22, 30, 0, 0, time.UTC)
	if at.Weekday() != time.Sunday {
		t.Fatalf("the fixture is not a Sunday: %v", at.Weekday())
	}
	if err := a.store.AddBandwidth(ctx, at, 4096, 128); err != nil {
		t.Fatalf("AddBandwidth: %v", err)
	}

	rec := do(t, h, httptest.NewRequest(http.MethodGet, "/api/bandwidth?span=minute&hours=999999", nil))
	var view BandwidthView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if view.TotalIn != 4096 || view.TotalOut != 128 {
		t.Errorf("totals = %d/%d, want 4096/128", view.TotalIn, view.TotalOut)
	}
	if view.Timezone != "Europe/Berlin" {
		t.Fatalf("timezone = %q", view.Timezone)
	}
	// Sunday is the last row when the week starts on Monday.
	if got := view.Heatmap[6][23]; got != 4096 {
		t.Errorf("Sunday 23:00 = %d, want 4096; the fold ignored the configured zone", got)
	}
}

// TestNotificationsOnlyGoOutOnTheEdge is the discipline the whole notification
// design rests on: SCN's daily quota is finite, so a condition that is true for
// hours is announced when it starts and when it clears, and never in between
// (DESIGN.md §4.1).
func TestNotificationsOnlyGoOutOnTheEdge(t *testing.T) {
	a, h, token := newApp(t)
	ctx := context.Background()

	sent := make(chan map[string]any, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	a.notifier = &notify.Client{Endpoint: srv.URL}

	form := url.Values{
		store.KeyNotifyUserID:  {"7"},
		store.KeyNotifyUserKey: {"secret"},
	}
	if rec := postForm(t, h, "/api/config", token, form); rec.Code != http.StatusOK {
		t.Fatalf("configure notifications: %d %s", rec.Code, rec.Body)
	}

	a.NotifyEdge(ctx, store.NotifyTunnelDown, 0, true, "the tunnel is down", "the tunnel is back")
	first := waitForMessage(t, sent)
	if first["title"] != "the tunnel is down" {
		t.Errorf("first message = %v", first["title"])
	}
	if first["user_id"] != float64(7) {
		t.Errorf("user_id came through as %#v; it has to be a number", first["user_id"])
	}
	if first["msg_id"] == nil || first["msg_id"] == "" {
		t.Error("no idempotency key was sent, so a retry would be a second message")
	}

	for range 3 {
		a.NotifyEdge(ctx, store.NotifyTunnelDown, 0, true, "the tunnel is down", "the tunnel is back")
	}
	select {
	case msg := <-sent:
		t.Fatalf("a condition that stayed true sent %v", msg["title"])
	case <-time.After(150 * time.Millisecond):
	}

	a.NotifyEdge(ctx, store.NotifyTunnelDown, 0, false, "the tunnel is down", "the tunnel is back")
	if cleared := waitForMessage(t, sent); cleared["title"] != "the tunnel is back" {
		t.Errorf("the clearing message = %v", cleared["title"])
	}
}

// TestNotificationsRespectTheirToggle: a kind switched off must not reach the
// network at all, not merely be filtered later.
func TestNotificationsRespectTheirToggle(t *testing.T) {
	a, h, token := newApp(t)

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a notification was sent for a kind that is switched off")
	}))
	defer srv.Close()
	a.notifier = &notify.Client{Endpoint: srv.URL}

	form := url.Values{
		store.KeyNotifyUserID:        {"7"},
		store.KeyNotifyUserKey:       {"secret"},
		store.KeyNotifyTunnelDown:    {"false"},
		store.KeyNotifyDBReplaced:    {"false"},
		store.KeyNotifySyncFailed:    {"false"},
		store.KeyNotifySpaceLow:      {"false"},
		store.KeyNotifyLockStale:     {"false"},
		store.KeyNotifyDeleteBlocked: {"false"},
	}
	if rec := postForm(t, h, "/api/config", token, form); rec.Code != http.StatusOK {
		t.Fatalf("configure notifications: %d %s", rec.Code, rec.Body)
	}

	a.Notify(context.Background(), store.NotifyTunnelDown, 0, "should not be sent", "")
	time.Sleep(150 * time.Millisecond)
}

// TestNotificationsAreSilentUntilConfigured: no account, no messages, and no
// error either - the push channel is optional.
func TestNotificationsAreSilentUntilConfigured(t *testing.T) {
	a, _, _ := newApp(t)

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a notification was sent with no SCN account configured")
	}))
	defer srv.Close()
	a.notifier = &notify.Client{Endpoint: srv.URL}

	a.Notify(context.Background(), store.NotifyTunnelDown, 0, "should not be sent", "")
	time.Sleep(150 * time.Millisecond)
}

func waitForMessage(t *testing.T, ch <-chan map[string]any) map[string]any {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(3 * time.Second):
		t.Fatal("no notification was sent")
		return nil
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
