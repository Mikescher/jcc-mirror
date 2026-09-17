package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"blackforestbytes.com/jcc-mirror/store"
)

// Everything here sends through send() rather than do(): do() carries the
// password on every request, which is the one thing a test of the gate cannot do.

// TestABrowserWithoutThePasswordGetsThePrompt: nothing of the dashboard may
// reach an unauthenticated browser - no view, no bundle, and not the shell it
// would draw them in (DESIGN.md §4).
func TestABrowserWithoutThePasswordGetsThePrompt(t *testing.T) {
	_, h := newApp(t)

	// The two ways a navigation announces itself; a browser sends both, and what
	// does not send Sec-Fetch-Mode still sends the Accept header.
	for name, header := range map[string][2]string{
		"an accept header": {"Accept", "text/html,application/xhtml+xml"},
		"a navigation":     {"Sec-Fetch-Mode", "navigate"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set(header[0], header[1])
		rec := send(t, h, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET / with %s = %d, want 401", name, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `<form id="f" method="post" action="/api/login"`) {
			t.Errorf("GET / with %s did not answer with the prompt: %.200s", name, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "<app-root>") {
			t.Errorf("GET / with %s served the dashboard shell", name)
		}
	}

	// A script asking for the bundle gets the status, because a login page parsed
	// as JavaScript is a worse error message than a 401.
	bundle := send(t, h, httptest.NewRequest(http.MethodGet, "/main.js", nil))
	wantError(t, bundle, http.StatusUnauthorized)
	if strings.Contains(bundle.Body.String(), "<form") {
		t.Error("GET /main.js answered with the login page")
	}

	for _, c := range [][2]string{{http.MethodGet, "/api/status"}, {http.MethodPost, "/api/runs"}} {
		rec := send(t, h, httptest.NewRequest(c[0], c[1], nil))
		wantError(t, rec, http.StatusUnauthorized)
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s %s: a 401 without WWW-Authenticate", c[0], c[1])
		}
	}
}

// TestHealthzNeedsNoPassword: the liveness probe an operator wires into compose
// cannot be made to log in, and it says nothing a caller could not learn by
// watching the container restart.
func TestHealthzNeedsNoPassword(t *testing.T) {
	_, h := newApp(t)

	if rec := send(t, h, httptest.NewRequest(http.MethodGet, "/healthz", nil)); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz with no password = %d, want 200: %s", rec.Code, rec.Body)
	}
}

// TestLoginExchangesThePasswordForACookie: what the browser keeps is proof of
// the password rather than the password, so a cookie read off a machine is not
// something that can be typed into the prompt.
func TestLoginExchangesThePasswordForACookie(t *testing.T) {
	_, h := newApp(t)

	wantError(t, login(t, h, "not-the-password"), http.StatusUnauthorized)

	rec := login(t, h, h.password)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", rec.Code, rec.Body)
	}
	if !authedAnswer(t, rec) {
		t.Errorf("login answered %s", rec.Body)
	}

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookie {
		t.Fatalf("login set %v", cookies)
	}
	c := cookies[0]
	if c.Value == h.password {
		t.Fatal("the cookie is the password itself")
	}
	if c.Value != sessionToken(h.password) {
		t.Errorf("cookie = %q, want the token derived from the password", c.Value)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie is %+v, want HttpOnly and SameSite=Strict", c)
	}

	if rec := getWith(t, h, c, "/api/status"); rec.Code != http.StatusOK {
		t.Fatalf("GET /api/status with the cookie = %d: %s", rec.Code, rec.Body)
	}

	out := send(t, h, httptest.NewRequest(http.MethodPost, "/api/logout", nil))
	if out.Code != http.StatusOK || authedAnswer(t, out) {
		t.Fatalf("logout = %d: %s", out.Code, out.Body)
	}
	cleared := out.Result().Cookies()
	if len(cleared) != 1 || cleared[0].Value != "" || cleared[0].MaxAge >= 0 {
		t.Fatalf("logout set %+v, want the cookie cleared", cleared)
	}
	if rec := getWith(t, h, cleared[0], "/api/status"); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/status after the logout = %d, want 401", rec.Code)
	}
}

// TestSessionSaysWhichSideOfTheGateTheBrowserIsOn is what a dashboard whose
// cookie expired mid-session asks before it sends the operator to the prompt
// (web/src/app/live.ts).
func TestSessionSaysWhichSideOfTheGateTheBrowserIsOn(t *testing.T) {
	_, h := newApp(t)

	if authedAnswer(t, send(t, h, httptest.NewRequest(http.MethodGet, "/api/session", nil))) {
		t.Error("a browser with no cookie is reported as signed in")
	}
	if !authedAnswer(t, getWith(t, h, loginCookie(t, h), "/api/session")) {
		t.Error("a browser with the cookie is reported as signed out")
	}
}

// TestThePasswordItselfOpensTheDashboard is the curl path from the README: a
// script sends the password in a header and derives nothing.
func TestThePasswordItselfOpensTheDashboard(t *testing.T) {
	_, h := newApp(t)

	for name, header := range map[string][2]string{
		"bearer":    {"Authorization", "Bearer " + h.password},
		"x-api-key": {"X-Api-Key", h.password},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		req.Header.Set(header[0], header[1])
		if rec := send(t, h, req); rec.Code != http.StatusOK {
			t.Errorf("%s: %d, want 200: %s", name, rec.Code, rec.Body)
		}
	}

	wrong := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	wrong.Header.Set("Authorization", "Bearer "+h.password+"x")
	wantError(t, send(t, h, wrong), http.StatusUnauthorized)
}

// TestAChangedPasswordLogsEveryBrowserOut: the cookie is derived from the
// password and there is no session table behind it, so changing the password is
// the only thing that revokes one (app/auth.go).
func TestAChangedPasswordLogsEveryBrowserOut(t *testing.T) {
	_, h := newApp(t)
	c := loginCookie(t, h)

	const next = "hunter2hunter2"
	if rec := postForm(t, h, "/api/config", url.Values{store.KeyDashboardPassword: {next}}); rec.Code != http.StatusOK {
		t.Fatalf("change the password: %d %s", rec.Code, rec.Body)
	}

	if rec := getWith(t, h, c, "/api/status"); rec.Code != http.StatusUnauthorized {
		t.Errorf("a cookie from before the change = %d, want 401", rec.Code)
	}
	// h.password is the old one until it is set, and it is no longer a credential
	// either.
	if rec := do(t, h, httptest.NewRequest(http.MethodGet, "/api/status", nil)); rec.Code != http.StatusUnauthorized {
		t.Errorf("the old password = %d, want 401", rec.Code)
	}

	h.password = next
	if rec := do(t, h, httptest.NewRequest(http.MethodGet, "/api/status", nil)); rec.Code != http.StatusOK {
		t.Errorf("the new password = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := loginCookie(t, h).Value; got != sessionToken(next) {
		t.Errorf("the cookie after the change = %q, want the token of the new password", got)
	}
}

func login(t *testing.T, h *dash, password string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"password": {password}}
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return send(t, h, req)
}

func loginCookie(t *testing.T, h *dash) *http.Cookie {
	t.Helper()
	rec := login(t, h, h.password)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", rec.Code, rec.Body)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login set %v", cookies)
	}
	return cookies[0]
}

// getWith sends a request carrying nothing but the cookie, which is all a browser
// past the prompt has.
func getWith(t *testing.T, h *dash, c *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(c)
	return send(t, h, req)
}

func authedAnswer(t *testing.T, rec *httptest.ResponseRecorder) bool {
	t.Helper()
	var body struct{ Authed bool }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return body.Authed
}
