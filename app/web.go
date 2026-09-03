package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"time"

	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/web"
)

// indexFile is both the entry point and the fallback: the dashboard is a
// single-page app, so a deep link the browser asks for directly is answered with
// the shell and routed on the client.
const indexFile = "index.html"

// dashboard serves the built Angular app out of the binary. The files are held in
// memory rather than read from the embedded FS per request - the whole build is a
// few hundred kilobytes, and it buys an ETag computed once.
type dashboard struct {
	files map[string]webFile
}

type webFile struct {
	body        []byte
	contentType string
	etag        string
}

func newDashboard(root fs.FS) (*dashboard, error) {
	d := &dashboard{files: map[string]webFile{}}

	err := fs.WalkDir(root, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		f, err := root.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()

		body, err := io.ReadAll(f)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		d.files[name] = webFile{
			body:        body,
			contentType: contentTypeOf(name),
			etag:        `"` + hex.EncodeToString(sum[:8]) + `"`,
		}
		return nil
	})
	return d, err
}

// contentTypes covers what an Angular build actually emits. It is spelled out
// rather than left to mime.TypeByExtension because that reads /etc/mime.types,
// which a distroless image does not have - and with nosniff set, a favicon
// served as application/octet-stream is a favicon the browser refuses.
var contentTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".html":  "text/html; charset=utf-8",
	".ico":   "image/x-icon",
	".js":    "text/javascript; charset=utf-8",
	".json":  "application/json",
	".map":   "application/json",
	".mjs":   "text/javascript; charset=utf-8",
	".png":   "image/png",
	".svg":   "image/svg+xml",
	".txt":   "text/plain; charset=utf-8",
	".webp":  "image/webp",
	".woff":  "font/woff",
	".woff2": "font/woff2",
}

func contentTypeOf(name string) string {
	if ct, ok := contentTypes[strings.ToLower(path.Ext(name))]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

func (d *dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")

	file, ok := d.files[name]
	if !ok {
		// Not a file we built, so it is a route: hand back the shell and let the
		// router work it out. A wrong URL is a 404 the app draws, not one the
		// server does.
		file, ok = d.files[indexFile]
		if !ok {
			http.Error(w, "the dashboard was not built into this binary; run `make web`", http.StatusNotFound)
			return
		}
		// The shell names the bundles, so it must never be the stale half of a
		// pair. The bundles themselves revalidate cheaply.
		w.Header().Set("Cache-Control", "no-store")
	} else {
		// Revalidate rather than cache outright: the lazy chunks carry a content
		// hash but the entry points do not, and one rule for both is simpler than
		// two. A 304 is a header exchange, which over the tunnel is nothing.
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", file.etag)
	}

	w.Header().Set("Content-Type", file.contentType)
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(file.body))
}

// devUI proxies everything the API does not answer to a running `ng serve`, so
// the dashboard can be developed against the real daemon with no CORS and no
// second origin: one port answers both halves (DESIGN.md §4).
func devUI(target string, log *logs.Logger) (http.Handler, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}

	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		// The usual cause is not a crash but an address: `ng serve` binds to
		// localhost, which resolves to ::1, so a target of 127.0.0.1 is refused
		// by a dev server that is running perfectly well.
		log.Errorf("dashboard: %s did not answer: %v", target, err)
		http.Error(w, "the dev server at "+target+" is not answering: "+err.Error(),
			http.StatusBadGateway)
	}
	return proxy, nil
}

// uiHandler is what serves everything that is not an API route.
func (a *App) uiHandler() http.Handler {
	if a.opts.DevUI != "" {
		proxy, err := devUI(a.opts.DevUI, a.log)
		if err != nil {
			a.log.Errorf("dashboard: -dev-ui %q: %v", a.opts.DevUI, err)
		} else {
			a.log.Infof("dashboard: serving the UI from %s", a.opts.DevUI)
			return proxy
		}
	}

	d, err := newDashboard(web.FS())
	if err != nil {
		a.log.Errorf("dashboard: %v", err)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "the dashboard could not be loaded", http.StatusInternalServerError)
		})
	}
	return d
}
