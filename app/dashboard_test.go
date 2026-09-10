package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
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
	_, h := newApp(t)

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
// The page is one Angular app with nothing else to draw itself out of, so a view
// that answers anything but JSON leaves a panel blank (DESIGN.md §4).
func TestReadViewsAreOpenAndAnswerJSON(t *testing.T) {
	m := newMirror(t)
	m.write(t, "Filme/a.mkv", 2048)
	m.run(t, RunScan)
	m.run(t, RunSync)

	pair := "?pair=" + itoa(m.pair.ID)
	for _, path := range []string{
		"/api/status", "/api/schedule", "/api/config", "/api/config/audit",
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

// TestTheLogTailIsServed: the container log on a Synology is exactly what an
// operator cannot get at, so the tail is how it is read at all. It goes to every
// reader, because the dashboard is reachable on the LAN or through the WireGuard
// tunnel and nowhere else, and nothing secret is printed into that log.
func TestTheLogTailIsServed(t *testing.T) {
	a, h := newApp(t)
	a.log.Infof("something worth reading")

	rec := do(t, h, httptest.NewRequest(http.MethodGet, "/api/diagnostics", nil))
	var view DiagnosticsView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var found bool
	for _, line := range view.Log {
		found = found || strings.Contains(line.Text, "something worth reading")
	}
	if !found {
		t.Fatalf("the tail of %d lines does not carry the line that was just logged", len(view.Log))
	}
}

// TestTheStreamCarriesTheLog: the stream carries the same tail the diagnostics
// view does, so a dashboard left open keeps up with the log instead of going
// stale until someone reloads the page (DESIGN.md §4).
func TestTheStreamCarriesTheLog(t *testing.T) {
	a, h := newApp(t)
	a.log.Infof("a line an operator should read")

	srv := httptest.NewServer(h)
	defer srv.Close()

	got, err := readStream(srv.URL)
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	if !strings.Contains(got, "event: state") {
		t.Fatalf("the stream carried nothing at all:\n%s", got)
	}
	if !strings.Contains(got, "an operator should read") {
		t.Fatalf("the stream did not carry the log:\n%s", got)
	}
}

// readStream opens the event stream and reads until the log arrives. The deadline
// is what ends it: the connection itself stays open for as long as the daemon is
// up, so there is nothing else to read to.
func readStream(base string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/stream", nil)
	if err != nil {
		return "", err
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
	_, h := newApp(t)

	form := url.Values{store.KeySchedule: {"* * = 5MiB; mon-fri 2-8 = full; sat 0-24 = off"}}
	if rec := postForm(t, h, "/api/config", form); rec.Code != http.StatusOK {
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
	a, h := newApp(t)
	ctx := context.Background()

	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("load the configured zone: %v", err)
	}

	// Yesterday at 23:30 UTC, which in Berlin is the small hours of today - a
	// different weekday and a different hour, which is exactly what a fold done
	// in UTC would get wrong. Yesterday rather than a fixed date because the
	// daemon rolls minute rows older than a week into hours as it starts, and a
	// fixture from last spring would race that.
	yesterday := time.Now().UTC().Add(-24 * time.Hour)
	at := time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(), 23, 30, 0, 0, time.UTC)

	local := at.In(berlin)
	row, hour := mondayFirst(local.Weekday()), local.Hour()
	if row == mondayFirst(at.Weekday()) && hour == at.Hour() {
		t.Fatalf("the fixture at %s reads the same in both zones, so it proves nothing", at)
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
	if got := view.Heatmap[row][hour]; got != 4096 {
		t.Errorf("%s %02d:00 = %d, want 4096; the fold ignored the configured zone",
			local.Weekday(), hour, got)
	}
}

// TestNotificationsOnlyGoOutOnTheEdge is the discipline the whole notification
// design rests on: SCN's daily quota is finite, so a condition that is true for
// hours is announced when it starts and when it clears, and never in between
// (DESIGN.md §4.1).
func TestNotificationsOnlyGoOutOnTheEdge(t *testing.T) {
	a, h := newApp(t)
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
	if rec := postForm(t, h, "/api/config", form); rec.Code != http.StatusOK {
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
	a, h := newApp(t)

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
	if rec := postForm(t, h, "/api/config", form); rec.Code != http.StatusOK {
		t.Fatalf("configure notifications: %d %s", rec.Code, rec.Body)
	}

	a.Notify(context.Background(), store.NotifyTunnelDown, 0, "should not be sent", "")
	time.Sleep(150 * time.Millisecond)
}

// TestNotificationsAreSilentUntilConfigured: no account, no messages, and no
// error either - the push channel is optional.
func TestNotificationsAreSilentUntilConfigured(t *testing.T) {
	a, _ := newApp(t)

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
