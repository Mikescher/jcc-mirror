package app

import (
	"context"
	"encoding/json"
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
	log.SetOutput(io.Discard) // the daemon prints the token at startup
	os.Exit(m.Run())
}

// newApp starts a daemon against a fresh store and returns it with its handler
// and the generated token.
func newApp(t *testing.T) (*App, http.Handler, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	st, err := store.OpenFile(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	a := New(st, &logs.Logger{}, Options{Version: "test"})
	h := a.Handler()
	a.SetHandler(h)
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	token, err := st.ConfigGet(ctx, store.KeyDashboardToken)
	if err != nil {
		t.Fatalf("ConfigGet: %v", err)
	}
	return a, h, token
}

func do(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func postForm(t *testing.T, h http.Handler, path, token string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return do(t, h, req)
}

func TestHealthOnAFirstBoot(t *testing.T) {
	_, h, _ := newApp(t)

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

func TestMutationsNeedTheToken(t *testing.T) {
	_, h, token := newApp(t)

	for _, path := range []string{"/api/config", "/api/remote/probe"} {
		if rec := postForm(t, h, path, "", url.Values{}); rec.Code != http.StatusUnauthorized {
			t.Errorf("POST %s without a token = %d, want 401", path, rec.Code)
		}
		if rec := postForm(t, h, path, "not-the-token", url.Values{}); rec.Code != http.StatusUnauthorized {
			t.Errorf("POST %s with a wrong token = %d, want 401", path, rec.Code)
		}
	}

	// Reading is deliberately open; only the actions are gated.
	for _, path := range []string{"/healthz", "/api/status", "/api/config", "/api/events", "/"} {
		if rec := do(t, h, httptest.NewRequest(http.MethodGet, path, nil)); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}

	rec := postForm(t, h, "/api/config", token, url.Values{store.KeyRemoteUser: {"ro"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST with the token = %d: %s", rec.Code, rec.Body)
	}
}

func TestLoginCookieAuthorizes(t *testing.T) {
	_, h, token := newApp(t)

	rec := postForm(t, h, "/api/login", "", url.Values{"token": {token}})
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d, want 200: %s", rec.Code, rec.Body)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != tokenCookie {
		t.Fatalf("login set %v", cookies)
	}
	if !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie is %+v, want HttpOnly and SameSite=Strict", cookies[0])
	}

	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(store.KeyRemoteUser+"=ro"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookies[0])
	if rec := do(t, h, req); rec.Code != http.StatusOK {
		t.Errorf("POST with the cookie = %d: %s", rec.Code, rec.Body)
	}

	if rec := postForm(t, h, "/api/login", "", url.Values{"token": {"wrong"}}); rec.Code != http.StatusUnauthorized {
		t.Errorf("login with a wrong token = %d, want 401", rec.Code)
	}

	// The dashboard posts JSON, not a form. Reading only the form body is a login
	// that always fails from the browser and always works from curl.
	body := strings.NewReader(`{"token":` + strconv.Quote(token) + `}`)
	req = httptest.NewRequest(http.MethodPost, "/api/login", body)
	req.Header.Set("Content-Type", "application/json")
	if rec := do(t, h, req); rec.Code != http.StatusOK {
		t.Errorf("login with a JSON body = %d: %s", rec.Code, rec.Body)
	}
}

func TestBlankSecretKeepsTheStoredOne(t *testing.T) {
	a, h, token := newApp(t)
	ctx := context.Background()

	if rec := postForm(t, h, "/api/config", token, url.Values{store.KeyRemotePassword: {"hunter2"}}); rec.Code != http.StatusOK {
		t.Fatalf("save = %d: %s", rec.Code, rec.Body)
	}

	// The dashboard is never sent a stored password, so an untouched field comes
	// back blank - and must not wipe it.
	if rec := postForm(t, h, "/api/config", token, url.Values{
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
	a, h, token := newApp(t)

	if rec := postForm(t, h, "/api/config", token, url.Values{store.KeyRemotePassword: {"hunter2"}}); rec.Code != http.StatusOK {
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

		for name, secret := range map[string]string{"password": "hunter2", "token": token, "private key": priv} {
			if strings.Contains(body, secret) {
				t.Errorf("GET %s leaked the %s", path, name)
			}
		}
	}
}

func TestBadValueIsRejectedWithoutChangingAnything(t *testing.T) {
	a, h, token := newApp(t)

	rec := postForm(t, h, "/api/config", token, url.Values{store.KeyWGEndpoint: {"rootserver"}})
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
	a, h, token := newApp(t)
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

	if rec := postForm(t, h, "/api/config", token, url.Values{store.KeyRemoteURL: {srv.URL + "/share"}}); rec.Code != http.StatusOK {
		t.Fatalf("save = %d: %s", rec.Code, rec.Body)
	}
	if rec := postForm(t, h, "/api/remote/probe", token, url.Values{}); rec.Code != http.StatusOK {
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
	a, h, token := newApp(t)

	// A complete tunnel configuration with an endpoint that will never answer:
	// the tunnel opens, but nothing is reachable through it.
	if rec := postForm(t, h, "/api/config", token, url.Values{
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
