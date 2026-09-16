// Package update is the self-updater of DESIGN.md §5: fetch a newer binary over
// plain HTTP(S), prove it is plausible, put it in place and re-exec.
//
// "Update in place without restarting docker" is not literally possible - the
// kernel holds the running executable's inode - but re-execing gives exactly the
// property that was wanted: same PID, same container, no orchestration involved.
//
// There is no signing, deliberately. The binary comes from a server the operator
// controls, so what the checks here are aimed at is corruption: a truncated
// download, an HTML error page saved as a binary, a binary for the wrong
// architecture. Those are the realistic failures, and they
// are all caught before anything is renamed.
package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// elfMagic is the first four bytes of every ELF file. Checking it costs nothing
// and catches the two failures that actually happen: a proxy's HTML error page,
// and a download that ended early.
var elfMagic = []byte{0x7f, 'E', 'L', 'F'}

// maxBinary bounds a download. The binary is around 25 MB; this is generous
// enough never to be hit by a real one and small enough that a server serving
// something unbounded cannot fill the data volume.
const maxBinary = 512 << 20

// smokeTimeout bounds the `--version` subprocess. A binary that cannot answer
// that in this long is not one to re-exec into.
const smokeTimeout = 20 * time.Second

// Source is where the new binary comes from: an http or https URL, fetched
// directly rather than through the tunnel. Credentials, when the server wants
// any, go in the URL's userinfo and are sent as Basic auth - which is why
// everything shown to a person goes through Location.
type Source struct {
	URL  string
	HTTP *http.Client
}

// Location is the URL with any password masked, the form worth showing someone.
func (s Source) Location() string { return redact(s.URL) }

func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Redacted()
}

// Release is what the server holds. There is no version file and no manifest:
// the question is literally "is the file over there newer than mine", and the
// Last-Modified answers it (DESIGN.md §5).
type Release struct {
	// URL is where the binary came from, redacted.
	URL     string    `json:"url"`
	ModTime time.Time `json:"modTime"`
	Size    int64     `json:"size"`
}

func (s Source) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return http.DefaultClient
}

func (s Source) request(ctx context.Context, method string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("build %s %s: %w", method, s.Location(), err)
	}
	req.Header.Set("User-Agent", "jcc-mirror")
	return req, nil
}

// Head asks the server what it has. A source that answers without a
// modification time is an error rather than a silent "not newer": the whole
// comparison rests on it, and an update that silently never happens is the
// failure mode this is least likely to be noticed by.
func (s Source) Head(ctx context.Context) (Release, error) {
	req, err := s.request(ctx, http.MethodHead)
	if err != nil {
		return Release{}, err
	}

	resp, err := s.client().Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("head: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Release{}, fmt.Errorf("head %s -> %s", s.Location(), resp.Status)
	}

	raw := resp.Header.Get("Last-Modified")
	if raw == "" {
		return Release{}, fmt.Errorf("%s answered without a Last-Modified, so there is nothing to compare against", s.Location())
	}
	mod, err := http.ParseTime(raw)
	if err != nil {
		return Release{}, fmt.Errorf("Last-Modified %q of %s: %w", raw, s.Location(), err)
	}

	return Release{URL: s.Location(), ModTime: mod.UTC(), Size: resp.ContentLength}, nil
}

func (s Source) open(ctx context.Context) (io.ReadCloser, error) {
	req, err := s.request(ctx, http.MethodGet)
	if err != nil {
		return nil, err
	}

	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("get: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("get %s -> %s: %s", s.Location(), resp.Status, strings.TrimSpace(string(snippet)))
	}
	return resp.Body, nil
}

// Download writes the binary to dst and returns how many bytes arrived. The
// caller verifies; this only fetches, so a partial file is left where the
// verification can see how big it actually is.
func (s Source) Download(ctx context.Context, dst string) (int64, error) {
	body, err := s.open(ctx)
	if err != nil {
		return 0, err
	}
	defer body.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, fmt.Errorf("create %q: %w", filepath.Dir(dst), err)
	}
	// 0o755 from the start: a binary written 0o644 and chmod'ed afterwards is one
	// more step that can fail between the write and the exec.
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return 0, fmt.Errorf("create %q: %w", dst, err)
	}

	n, err := io.Copy(f, io.LimitReader(body, maxBinary+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return n, fmt.Errorf("download %s: %w", s.Location(), err)
	}
	if n > maxBinary {
		return n, fmt.Errorf("download %s: more than %d bytes, which is not a jcc-mirror binary", s.Location(), maxBinary)
	}
	// The mode is set on open, but a pre-existing file keeps its own - and a
	// binary that is not executable fails at the exec, which is the worst place
	// to find out.
	if err := os.Chmod(dst, 0o755); err != nil {
		return n, fmt.Errorf("make %q executable: %w", dst, err)
	}
	return n, nil
}

// Verify is the sanity check of DESIGN.md §5 step 2: the size the server
// promised, and the ELF magic. want <= 0 means the server did not say how large
// it is, which is legal and only costs the truncation check.
func Verify(path string, want int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if want > 0 && info.Size() != want {
		return fmt.Errorf("%s is %d bytes, the server promised %d - the download was cut short",
			filepath.Base(path), info.Size(), want)
	}
	if info.Size() < int64(len(elfMagic)) {
		return fmt.Errorf("%s is %d bytes, which is not a binary", filepath.Base(path), info.Size())
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	head := make([]byte, len(elfMagic))
	if _, err := io.ReadFull(f, head); err != nil {
		return fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if string(head) != string(elfMagic) {
		return fmt.Errorf("%s does not start with the ELF magic - an error page saved as a binary looks exactly like this",
			filepath.Base(path))
	}
	return nil
}

// SmokeTest runs the candidate's own `--version` and reads what it says. It is
// the check that catches the failure the ELF magic cannot: a binary built for
// the other architecture, which is a well-formed ELF that will not run here
// (DESIGN.md §5 step 3).
func SmokeTest(ctx context.Context, path string) (version, build string, err error) {
	ctx, cancel := context.WithTimeout(ctx, smokeTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	line := strings.TrimSpace(string(out))
	if err != nil {
		if line != "" {
			return "", "", fmt.Errorf("%s --version: %w: %s", filepath.Base(path), err, line)
		}
		return "", "", fmt.Errorf("%s --version: %w", filepath.Base(path), err)
	}

	version, build, ok := parseVersion(line)
	if !ok {
		return "", "", fmt.Errorf("%s --version answered %q, which is not a jcc-mirror", filepath.Base(path), line)
	}
	return version, build, nil
}

// parseVersion reads back what cmdVersion prints: "jcc-mirror <version> (built
// <stamp>)". Requiring the prefix is most of the point - it is what distinguishes
// our binary from any other ELF that happens to sit at that URL.
func parseVersion(line string) (version, build string, ok bool) {
	rest, ok := strings.CutPrefix(line, "jcc-mirror ")
	if !ok {
		return "", "", false
	}
	version, build, hasBuild := strings.Cut(rest, " (built ")
	if version = strings.TrimSpace(version); version == "" {
		return "", "", false
	}
	if hasBuild {
		build = strings.TrimSuffix(strings.TrimSpace(build), ")")
	}
	return version, build, true
}

// ParseBuildStamp reads the -ldflags timestamp. A build that carries no usable
// one - a `go build` with no Makefile behind it - returns the zero time, which
// every caller reads as "cannot tell whether the server's is newer" rather than as
// "the epoch, so everything is newer".
func ParseBuildStamp(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" || s == "unknown" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// ErrNotConfigured is what every entry point answers when no binary URL is set.
// It is a sentinel because "nobody asked for updates" is not a fault to report.
var ErrNotConfigured = errors.New("no update URL is configured")

// Locate checks the configured binary URL: http or https with a host, nothing
// else.
func Locate(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrNotConfigured
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("update URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("update URL %s: must be http:// or https://", u.Redacted())
	}
	if u.Host == "" {
		return "", fmt.Errorf("update URL %s: has no host", u.Redacted())
	}
	return raw, nil
}
