package app

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"blackforestbytes.com/jcc-mirror/store"
)

// sessionCookie carries proof of the password rather than the password itself, so
// what sits in the browser cannot be read back out and typed into the prompt.
// SameSite=Strict is the CSRF story: there is no CORS and no cross-site request
// that could carry it. No Secure flag - the dashboard answers on plain HTTP, and
// a Secure cookie would simply never be stored (DESIGN.md §4).
const sessionCookie = "jccmirror_session"

// sessionMaxAge is how long a browser keeps the cookie. There is no server-side
// session to expire, so this is the whole of it.
const sessionMaxAge = 30 * 24 * 60 * 60

// openPaths are the only routes that answer without the password: the liveness
// probe, which an operator may well have wired into compose and which says
// nothing a caller could not learn by watching the container restart, and the
// three the login form itself needs.
var openPaths = map[string]bool{
	"/healthz":     true,
	"/api/login":   true,
	"/api/logout":  true,
	"/api/session": true,
}

// requirePassword is the gate in front of the whole dashboard - the API, the
// stream and the Angular bundle alike. An unauthenticated browser is served the
// login page and never the app, so nothing of the dashboard reaches it: no view,
// no bundle, no state.
//
// It wraps the inner mux only. The remote API is behind its own key on a
// separate branch of the root mux and must not be gated twice (app/http.go).
func (a *App) requirePassword(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if openPaths[r.URL.Path] || a.authenticated(r) {
			next.ServeHTTP(w, r)
			return
		}

		// A browser asking for a page gets the prompt; anything else gets the
		// status, because a login page parsed as JSON is a worse error message than
		// a 401.
		if r.Method == http.MethodGet && wantsHTML(r) {
			writeLoginPage(w)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="jcc-mirror"`)
		a.fail(w, r, http.StatusUnauthorized, errors.New("the dashboard needs its password"))
	})
}

// authenticated says whether the request carries the password. The password is
// read on every request, so one changed in the Config view applies at once - and
// logs every browser out, the cookie being derived from it.
func (a *App) authenticated(r *http.Request) bool {
	want, err := a.store.ConfigGet(r.Context(), store.KeyDashboardPassword)
	if err != nil || want == "" {
		// Fail closed. A password that cannot be read is not a password that has
		// been met.
		return false
	}

	// The cookie holds the derived token, a header holds the password itself: the
	// browser never has to keep the typed value, and curl never has to derive
	// anything (README, "Talking to it directly").
	if c, err := r.Cookie(sessionCookie); err == nil && keyMatches(c.Value, sessionToken(want)) {
		return true
	}
	return keyMatches(requestKey(r), want)
}

// sessionToken derives the cookie value from the password. It is deterministic
// on purpose: a restart - a self-update in particular - must not log everyone
// out, and there is no session table to survive one. The cost is that nothing but
// changing the password can revoke it.
func sessionToken(password string) string {
	sum := sha256.Sum256([]byte("jccmirror-session-v1\x00" + password))
	return hex.EncodeToString(sum[:])
}

// NewPassword makes the one generated at first start. rand.Text is base32, so it
// is long enough to be a password nobody guesses and still survives being read
// off a container log and typed into a prompt: no case ambiguity, no character
// that a font renders two ways.
func NewPassword() (string, error) {
	return rand.Text(), nil
}

// handleSession says whether this browser is past the gate. The login page polls
// nothing and the app is only ever served authenticated, so this exists for one
// job: telling a dashboard whose cookie expired mid-session to go and get the
// prompt (web/src/app/live.ts).
func (a *App) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"authed": a.authenticated(r)})
}

// handleLogin exchanges the password for the cookie.
func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	// readFields rather than FormValue: the login page posts a form and a script
	// posts JSON, and both have to work.
	fields, err := readFields(w, r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	want, err := a.store.ConfigGet(r.Context(), store.KeyDashboardPassword)
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	if want == "" {
		a.fail(w, r, http.StatusInternalServerError, errors.New("no dashboard password is set"))
		return
	}

	if !keyMatches(strings.TrimSpace(fields["password"]), want) {
		a.log.Warnf("dashboard: rejected a login from %s", actorOf(r))
		a.fail(w, r, http.StatusUnauthorized, errors.New("wrong password"))
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sessionToken(want),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   sessionMaxAge,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"authed": true})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"authed": false})
}

// wantsHTML distinguishes a browser navigating from a script calling. Sec-Fetch-Mode
// is the reliable half of it; the Accept sniff covers what does not send one.
func wantsHTML(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Mode") == "navigate" {
		return true
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// writeLoginPage serves the prompt. It is one self-contained file with no asset
// of its own, which is the point: an unauthenticated browser fetches this and
// nothing else, so there is no bundle to gate and no second request to get wrong.
func writeLoginPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(loginPage))
}

const loginPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex,nofollow">
<title>jcc-mirror</title>
<style>
  :root { color-scheme: dark; --bg:#14161a; --card:#1c1f26; --line:#2b3040; --fg:#e6e8ee; --dim:#8b91a3; --accent:#4d7cfe; --bad:#ff6b6b; }
  * { box-sizing: border-box; }
  body { margin:0; min-height:100vh; display:grid; place-items:center; padding:16px;
         background:var(--bg); color:var(--fg);
         font:15px/1.5 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,sans-serif; }
  form { width:100%; max-width:22rem; background:var(--card); border:1px solid var(--line);
         border-radius:10px; padding:1.75rem; }
  h1 { margin:0 0 .25rem; font-size:1.1rem; letter-spacing:.01em; }
  p { margin:0 0 1.25rem; color:var(--dim); font-size:.875rem; }
  label { display:block; margin-bottom:.4rem; font-size:.8125rem; color:var(--dim); }
  input { width:100%; padding:.6rem .7rem; border-radius:7px; border:1px solid var(--line);
          background:#0f1116; color:var(--fg); font-size:1rem; font-family:inherit; }
  input:focus { outline:2px solid var(--accent); outline-offset:1px; border-color:transparent; }
  button { width:100%; margin-top:1rem; padding:.6rem; border:0; border-radius:7px;
           background:var(--accent); color:#fff; font-size:.9375rem; font-weight:600;
           font-family:inherit; cursor:pointer; }
  button:hover { filter:brightness(1.08); }
  .err { margin:1rem 0 0; color:var(--bad); font-size:.875rem; }
  .err[hidden] { display:none; }
</style>
</head>
<body>
<form id="f" method="post" action="/api/login" autocomplete="on">
  <h1>jcc-mirror</h1>
  <p>This dashboard is password protected.</p>
  <label for="password">Password</label>
  <input id="password" name="password" type="password" autocomplete="current-password" autofocus required>
  <button type="submit">Sign in</button>
  <p class="err" id="e" hidden></p>
</form>
<script>
  const f = document.getElementById('f'), e = document.getElementById('e');
  f.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    e.hidden = true;
    try {
      const res = await fetch('/api/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'same-origin',
        body: JSON.stringify({ password: document.getElementById('password').value }),
      });
      if (res.ok) { location.replace(location.pathname + location.search); return; }
      const body = await res.json().catch(() => ({}));
      e.textContent = body.error || 'Wrong password.';
    } catch {
      e.textContent = 'The daemon did not answer.';
    }
    e.hidden = false;
    document.getElementById('password').select();
  });
</script>
</body>
</html>
`
