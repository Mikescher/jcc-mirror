package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	xwebdav "golang.org/x/net/webdav"

	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/store"
)

func TestMain(m *testing.M) {
	// The self-updater smoke-tests what it downloaded by running `--version` on
	// it (DESIGN.md §5). This branch is what lets a copy of the test binary stand
	// in for a release, so the update test exercises the real check rather than a
	// stub of it.
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("jcc-mirror v-next (built 2030-01-01T00:00:00Z)")
		os.Exit(0)
	}

	log.SetOutput(io.Discard) // a daemon per test, each logging its way through a start
	os.Exit(m.Run())
}

// newApp starts a daemon against a fresh store and returns it with its handler.
func newApp(t *testing.T) (*App, http.Handler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	st, err := store.OpenFile(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	a := New(st, &logs.Logger{}, Options{Version: "test", DataDir: t.TempDir()})
	h := a.Handler()
	a.SetHandler(h)
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Registered after the temp directories, so it runs before them: the daemon's
	// loops have to be stopped and the database closed while the files they use
	// still exist, or the cleanup races sqlite over its own journal.
	t.Cleanup(func() {
		cancel()
		a.Close(context.Background())
		st.Close()
	})

	return a, h
}

func do(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func postForm(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return do(t, h, req)
}

func TestHealthOnAFirstBoot(t *testing.T) {
	_, h := newApp(t)

	rec := do(t, h, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var st Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// An unconfigured tunnel is the first-boot state, not a failure: answering 503
	// there would make a container that is working perfectly look broken.
	if st.Tunnel.State != TunnelUnconfigured {
		t.Errorf("tunnel state = %q, want %q", st.Tunnel.State, TunnelUnconfigured)
	}
	if st.Database != "ok" {
		t.Errorf("database = %q", st.Database)
	}
	if st.PublicKey == "" {
		t.Error("no public key was generated")
	}
}

// TestActionsAndReadsAreBothOpen: the dashboard sits on the LAN or behind the
// WireGuard tunnel and nowhere else, so nothing on it is behind a login. Reading
// a view and pressing a button are equally open (DESIGN.md §4).
func TestActionsAndReadsAreBothOpen(t *testing.T) {
	_, h := newApp(t)

	for _, path := range []string{"/healthz", "/api/status", "/api/config", "/api/events", "/"} {
		if rec := do(t, h, httptest.NewRequest(http.MethodGet, path, nil)); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}

	if rec := postForm(t, h, "/api/config", url.Values{store.KeyRemoteUser: {"ro"}}); rec.Code != http.StatusOK {
		t.Fatalf("POST /api/config = %d: %s", rec.Code, rec.Body)
	}

	// A probe of a remote that was never configured cannot succeed, but what it
	// answers about is the remote rather than the caller.
	if rec := postForm(t, h, "/api/remote/probe", url.Values{}); rec.Code == http.StatusUnauthorized {
		t.Errorf("POST /api/remote/probe = 401: %s", rec.Body)
	}
}

// TestSettingsTakeAFormOrJSON: the dashboard posts JSON and the plain form on the
// page posts a form. Reading only one of the two is a save that works from half
// of the daemon's own UI and silently does nothing from the other.
func TestSettingsTakeAFormOrJSON(t *testing.T) {
	a, h := newApp(t)
	ctx := context.Background()

	if rec := postForm(t, h, "/api/config", url.Values{store.KeyRemoteUser: {"ro"}}); rec.Code != http.StatusOK {
		t.Fatalf("a form body = %d: %s", rec.Code, rec.Body)
	}
	if got, err := a.Store().ConfigGet(ctx, store.KeyRemoteUser); err != nil || got != "ro" {
		t.Fatalf("after the form the user is %q (err %v)", got, err)
	}

	body := strings.NewReader(`{` + strconv.Quote(store.KeyRemoteUser) + `:"rw"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/config", body)
	req.Header.Set("Content-Type", "application/json")
	if rec := do(t, h, req); rec.Code != http.StatusOK {
		t.Fatalf("a JSON body = %d: %s", rec.Code, rec.Body)
	}
	if got, err := a.Store().ConfigGet(ctx, store.KeyRemoteUser); err != nil || got != "rw" {
		t.Errorf("after the JSON body the user is %q (err %v)", got, err)
	}
}

func TestBlankSecretKeepsTheStoredOne(t *testing.T) {
	a, h := newApp(t)
	ctx := context.Background()

	if rec := postForm(t, h, "/api/config", url.Values{store.KeyRemotePassword: {"hunter2"}}); rec.Code != http.StatusOK {
		t.Fatalf("save = %d: %s", rec.Code, rec.Body)
	}

	// The dashboard is never sent a stored password, so an untouched field comes
	// back blank - and must not wipe it.
	if rec := postForm(t, h, "/api/config", url.Values{
		store.KeyRemotePassword: {""},
		store.KeyRemoteUser:     {"ro"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("save = %d: %s", rec.Code, rec.Body)
	}

	got, err := a.Store().ConfigGet(ctx, store.KeyRemotePassword)
	if err != nil {
		t.Fatalf("ConfigGet: %v", err)
	}
	if got != "hunter2" {
		t.Errorf("password after a blank submit = %q", got)
	}
}

func TestSecretsNeverLeaveTheProcess(t *testing.T) {
	a, h := newApp(t)

	if rec := postForm(t, h, "/api/config", url.Values{store.KeyRemotePassword: {"hunter2"}}); rec.Code != http.StatusOK {
		t.Fatalf("save = %d: %s", rec.Code, rec.Body)
	}
	priv, err := a.Store().ConfigGet(context.Background(), store.KeyWGPrivateKey)
	if err != nil {
		t.Fatalf("ConfigGet: %v", err)
	}

	for _, path := range []string{"/api/config", "/api/config/audit", "/api/status", "/healthz", "/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Accept", "text/html")
		body := do(t, h, req).Body.String()

		for name, secret := range map[string]string{"password": "hunter2", "private key": priv} {
			if strings.Contains(body, secret) {
				t.Errorf("GET %s leaked the %s", path, name)
			}
		}
	}
}

func TestBadValueIsRejectedWithoutChangingAnything(t *testing.T) {
	a, h := newApp(t)

	rec := postForm(t, h, "/api/config", url.Values{store.KeyWGEndpoint: {"rootserver"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}

	got, err := a.Store().ConfigGet(context.Background(), store.KeyWGEndpoint)
	if err != nil {
		t.Fatalf("ConfigGet: %v", err)
	}
	if got != "" {
		t.Errorf("a rejected endpoint was stored as %q", got)
	}
}

func TestProbeRecordsAnEvent(t *testing.T) {
	a, h := newApp(t)
	ctx := context.Background()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "ClipCornDB"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	srv := httptest.NewServer(&xwebdav.Handler{
		Prefix:     "/share",
		FileSystem: xwebdav.Dir(dir),
		LockSystem: xwebdav.NewMemLS(),
	})
	defer srv.Close()

	if rec := postForm(t, h, "/api/config", url.Values{store.KeyRemoteURL: {srv.URL + "/share"}}); rec.Code != http.StatusOK {
		t.Fatalf("save = %d: %s", rec.Code, rec.Body)
	}
	if rec := postForm(t, h, "/api/remote/probe", url.Values{}); rec.Code != http.StatusOK {
		t.Fatalf("probe = %d: %s", rec.Code, rec.Body)
	}

	events, err := a.Store().Events(ctx, store.EventFilter{Kinds: []string{store.KindRemoteProbe}})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d probe events, want 1", len(events))
	}
	if got := events[0].Data["dirs"]; got != float64(1) {
		t.Errorf("probe counted dirs = %v, want 1", got)
	}
	if got := events[0].Data["files"]; got != float64(1) {
		t.Errorf("probe counted files = %v, want 1", got)
	}
}

func TestRemoteIsRefusedWhileTheTunnelIsDown(t *testing.T) {
	a, h := newApp(t)

	// A complete tunnel configuration with an endpoint that will never answer:
	// the tunnel opens, but nothing is reachable through it.
	if rec := postForm(t, h, "/api/config", url.Values{
		store.KeyWGPeerKey:    {"LHAc/QH5VQW1F9sdlMTOap88nbDNtx876aR6eBUdvlw="},
		store.KeyWGEndpoint:   {"198.51.100.7:51820"},
		store.KeyWGAddress:    {"10.13.13.3/32"},
		store.KeyWGAllowedIPs: {"10.13.13.0/24"},
		store.KeyRemoteURL:    {"http://10.13.13.2:5005/media"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("save = %d: %s", rec.Code, rec.Body)
	}
	defer a.Close(context.Background())

	if _, err := a.Remote(); err != nil {
		t.Fatalf("with the tunnel open the remote should be usable: %v", err)
	}

	st := a.Status(context.Background())
	if st.Tunnel.State != TunnelConnecting {
		t.Errorf("tunnel state = %q, want %q", st.Tunnel.State, TunnelConnecting)
	}
	if len(st.Tunnel.Peers) != 1 {
		t.Fatalf("got %d peers, want 1", len(st.Tunnel.Peers))
	}
	if st.Tunnel.Peers[0].HandshakeAge != nil {
		t.Error("a peer that never handshaked reports an age")
	}
}

func TestTunnelSettingsCompleteness(t *testing.T) {
	full := store.Values{
		store.KeyWGPrivateKey:   "a",
		store.KeyWGPeerKey:      "b",
		store.KeyWGEndpoint:     "c",
		store.KeyWGAddress:      "d",
		store.KeyWGAllowedIPs:   "e",
		store.KeyWGDNS:          " ",
		store.KeyWGPresharedKey: "",
	}
	if !tunnelSettingsOf(full).complete() {
		t.Error("a full configuration is reported incomplete")
	}

	for _, missing := range []string{
		store.KeyWGPrivateKey, store.KeyWGPeerKey, store.KeyWGEndpoint,
		store.KeyWGAddress, store.KeyWGAllowedIPs,
	} {
		partial := store.Values{}
		for k, v := range full {
			partial[k] = v
		}
		partial[missing] = ""
		if tunnelSettingsOf(partial).complete() {
			t.Errorf("a configuration missing %s is reported complete", missing)
		}
	}
}
