package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"blackforestbytes.com/jcc-mirror/notify"
	"blackforestbytes.com/jcc-mirror/store"
	"blackforestbytes.com/jcc-mirror/wg"
)

// The two keys the WireGuard fixtures are written with: base64 of 32 identical
// bytes, which is a valid curve25519 key and nothing that could be a real one.
const (
	wgTestPrivate = "ERERERERERERERERERERERERERERERERERERERERERE="
	wgTestPeer    = "IiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiI="
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
}

// TestTheMeterCountsWhatCrossesTheWire covers the counter the Bandwidth view is
// sampled from. It sits on the dialer rather than on any request, because the
// remote is not HTTP and there is no round trip to hook - so what it counts is
// checked on a connection rather than on a run.
func TestTheMeterCountsWhatCrossesTheWire(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// An echo, so one exchange moves the same bytes in both directions and the
	// two counters can be told apart.
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(conn, conn)
	}()

	a, _ := newApp(t)

	a.mu.Lock()
	dial := a.meteredDialLocked()
	a.mu.Unlock()

	conn, err := dial(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sent := []byte("forty-two bytes of nothing in particular")
	if _, err := conn.Write(sent); err != nil {
		t.Fatalf("write: %v", err)
	}
	back := make([]byte, len(sent))
	if _, err := io.ReadFull(conn, back); err != nil {
		t.Fatalf("read: %v", err)
	}

	in, out := a.meter.take()
	if in != int64(len(sent)) || out != int64(len(sent)) {
		t.Errorf("meter counted %d in and %d out, want %d each", in, out, len(sent))
	}
	// take() is what a per-minute sample does, so the counters must be back to
	// zero or every sample would carry the whole run's total again.
	if in, out := a.meter.take(); in != 0 || out != 0 {
		t.Errorf("a second sample counted %d in and %d out, want zero", in, out)
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

	createNotifyTarget(t, h, map[string]any{"name": "phone", "userId": "7", "userKey": "secret"})

	a.NotifyEdge(ctx, store.NotifyTunnelDown, 0, true, "the tunnel is down", "the tunnel is back")
	first := waitForMessage(t, sent)
	if first["title"] != "the tunnel is down" {
		t.Errorf("first message = %v", first["title"])
	}
	if first["user_id"] != "7" {
		t.Errorf("user_id came through as %#v, want the string 7", first["user_id"])
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

// TestNotificationsRespectTheirTopics: a kind a target has not chosen, or a
// target that is switched off, must not reach the network at all.
func TestNotificationsRespectTheirTopics(t *testing.T) {
	a, h := newApp(t)

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a notification was sent for a kind that is switched off")
	}))
	defer srv.Close()
	a.notifier = &notify.Client{Endpoint: srv.URL}

	createNotifyTarget(t, h, map[string]any{
		"name": "quiet", "userId": "7", "userKey": "secret", "events": []string{"sync_failed", "update"},
	})
	createNotifyTarget(t, h, map[string]any{
		"name": "off", "userId": "8", "userKey": "secret", "enabled": false, "events": []string{"tunnel_down"},
	})

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

// TestImportingTheServersWireguardConfig is the whole point of the importer: the
// WireGuard server hands out a finished client config, and pasting it has to fill
// the Tunnel group in one go - including the private key it assigned, which is
// the one field the old flow had nowhere to put.
func TestImportingTheServersWireguardConfig(t *testing.T) {
	a, h := newApp(t)
	ctx := context.Background()

	before, err := a.store.ConfigGet(ctx, store.KeyWGPrivateKey)
	if err != nil {
		t.Fatalf("ConfigGet: %v", err)
	}
	if before == "" {
		t.Fatal("no private key was seeded at first start, so the tunnel has no identity at all")
	}

	const file = `[Interface]
PrivateKey = ` + wgTestPrivate + `
Address = 10.13.13.3/32, fd00::3/128
DNS = 10.13.13.1
MTU = 1380
ListenPort = 51820
PostUp = iptables -A FORWARD -j ACCEPT

[Peer]
PublicKey = ` + wgTestPeer + `
AllowedIPs = 0.0.0.0/0, ::/0
Endpoint = 203.0.113.7:51820
PersistentKeepalive = 21
`

	var res struct {
		Changed []string `json:"changed"`
		Ignored []string `json:"ignored"`
		Status  Status   `json:"status"`
	}
	rec := postImport(t, h, file)
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d: %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, key := range []string{
		store.KeyWGPrivateKey, store.KeyWGPeerKey, store.KeyWGEndpoint,
		store.KeyWGAddress, store.KeyWGAllowedIPs, store.KeyWGDNS,
		store.KeyWGMTU, store.KeyWGKeepalive,
	} {
		if !slices.Contains(res.Changed, key) {
			t.Errorf("%s is not in changed = %v", key, res.Changed)
		}
	}
	if strings.Join(res.Ignored, ",") != "ListenPort,PostUp" {
		t.Errorf("ignored = %v, want the two host directives", res.Ignored)
	}

	for key, want := range map[string]string{
		store.KeyWGPrivateKey: wgTestPrivate,
		store.KeyWGPeerKey:    wgTestPeer,
		store.KeyWGEndpoint:   "203.0.113.7:51820",
		store.KeyWGAddress:    "10.13.13.3/32, fd00::3/128",
		store.KeyWGAllowedIPs: "0.0.0.0/0, ::/0",
		store.KeyWGDNS:        "10.13.13.1",
		store.KeyWGMTU:        "1380",
		store.KeyWGKeepalive:  "21",
	} {
		got, err := a.store.ConfigGet(ctx, key)
		if err != nil {
			t.Fatalf("ConfigGet %s: %v", key, err)
		}
		if got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	// The import is a configuration change like any other, so it went through the
	// audit trail and the derived public key follows the imported private half.
	entries, err := a.store.ConfigAudit(ctx, 50)
	if err != nil {
		t.Fatalf("ConfigAudit: %v", err)
	}
	for _, e := range entries {
		if e.Key == store.KeyWGPrivateKey && e.New != store.SecretMask {
			t.Errorf("the audit trail recorded the private key as %q", e.New)
		}
	}
	pub, err := a.PublicKey(ctx)
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	if res.Status.PublicKey != pub {
		t.Errorf("the answer reports %q as our public key, but it is now %q", res.Status.PublicKey, pub)
	}
}

// A config that will not parse is answered with the message itself, because the
// dashboard shows it to whoever pasted the file.
func TestImportingABrokenWireguardConfig(t *testing.T) {
	_, h := newApp(t)

	rec := postImport(t, h, "[Interface]\nAddress = 10.13.13.3/32\n")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("import = %d: %s", rec.Code, rec.Body)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(body["error"], "PrivateKey") {
		t.Errorf("error = %q; it has to name the field that is missing", body["error"])
	}
}

func postImport(t *testing.T, h http.Handler, config string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"config": config})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/config/wireguard/import", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return do(t, h, req)
}

// The tunnel is re-opened only when the settings differ from the running ones, so
// a key missing from that comparison is a setting that silently does nothing
// until something unrelated is saved.
func TestTheMTUAndKeepaliveReachTheTunnel(t *testing.T) {
	base := store.Values{store.KeyWGMTU: "1420", store.KeyWGKeepalive: "25"}

	if cfg := tunnelSettingsOf(base).wgConfig(nil); cfg.MTU != 1420 || cfg.Keepalive != 25 {
		t.Errorf("wgConfig = mtu %d, keepalive %d", cfg.MTU, cfg.Keepalive)
	}
	if tunnelSettingsOf(base) == tunnelSettingsOf(store.Values{store.KeyWGMTU: "1380", store.KeyWGKeepalive: "25"}) {
		t.Error("a changed MTU compares equal, so the tunnel would keep the old one")
	}
	if tunnelSettingsOf(base) == tunnelSettingsOf(store.Values{store.KeyWGMTU: "1420", store.KeyWGKeepalive: "0"}) {
		t.Error("a changed keepalive compares equal, so the tunnel would keep the old one")
	}
}

// The registry deliberately imports nothing, so the two tunnel defaults are
// spelled out in both places and only this keeps them from drifting apart.
func TestTunnelDefaultsMatchTheWgPackage(t *testing.T) {
	for _, c := range []struct {
		key  string
		want int
	}{
		{store.KeyWGMTU, wg.DefaultMTU},
		{store.KeyWGKeepalive, wg.DefaultKeepalive},
	} {
		d, ok := store.Key(c.key)
		if !ok {
			t.Fatalf("%s is not in the registry", c.key)
		}
		if d.Default != strconv.Itoa(c.want) {
			t.Errorf("%s defaults to %q, but wg uses %d", c.key, d.Default, c.want)
		}
	}
}
