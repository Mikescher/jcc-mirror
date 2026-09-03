// Package webdav is a minimal WebDAV client covering exactly what the mirror
// needs: PROPFIND to walk a tree, HEAD to probe for lock files, and ranged GET to
// transfer. It speaks plain HTTP, which is the whole reason WebDAV was chosen -
// it traverses the userspace WireGuard tunnel unchanged (DESIGN.md §2.1).
package webdav

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync/atomic"

	"blackforestbytes.com/jcc-mirror/remote"
)

// Depth header values for Propfind.
const (
	Depth0        = "0"
	Depth1        = "1"
	DepthInfinity = "infinity"
)

// Config configures a Client. Transport is how the tunnel gets in: pass the
// netstack-backed one and every request goes over WireGuard.
type Config struct {
	BaseURL   string
	Username  string
	Password  string
	Transport http.RoundTripper // nil means http.DefaultTransport
	UserAgent string
}

// Client is a WebDAV client rooted at a base URL. It is safe for concurrent use.
type Client struct {
	base   *url.URL
	root   string // decoded path of base, the prefix every href carries
	user   string
	pass   string
	agent  string
	http   *http.Client
	nonNFC atomic.Int64
}

var _ remote.Remote = (*Client)(nil)

// New builds a client for baseURL. The base path is the remote root: every path
// passed to and returned from this client is relative to it.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse webdav url %q: %w", cfg.BaseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("webdav url %q: need an http or https scheme", cfg.BaseURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("webdav url %q: no host", cfg.BaseURL)
	}

	tr := cfg.Transport
	if tr == nil {
		tr = http.DefaultTransport
	}
	agent := cfg.UserAgent
	if agent == "" {
		agent = "jcc-mirror"
	}

	return &Client{
		base:  u,
		root:  u.Path,
		user:  cfg.Username,
		pass:  cfg.Password,
		agent: agent,
		// No Client.Timeout: a single GET of a 40 GB file legitimately runs for
		// hours. Deadlines belong to the caller's context.
		http: &http.Client{Transport: tr, CheckRedirect: checkRedirect},
	}, nil
}

// checkRedirect refuses to follow a redirect of anything but GET and HEAD. Go
// rewrites 301, 302 and 303 to GET, which would silently turn a PROPFIND into a
// file download and a very confusing decode error. A misconfigured base URL is
// the usual cause, and saying so is more useful than following it.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return fmt.Errorf("too many redirects, last to %s", req.URL)
	}
	if m := via[0].Method; m != http.MethodGet && m != http.MethodHead {
		return fmt.Errorf("server redirected %s %s to %s: fix the base URL rather than following it", m, via[0].URL, req.URL)
	}
	return nil
}

// NonNFCNames counts the entry names seen so far that were not already in NFC.
// Anything but zero means the source tree is partly NFD and the diff has to
// normalize, or every affected title looks new on every scan (DESIGN.md §2.3).
func (c *Client) NonNFCNames() int64 { return c.nonNFC.Load() }

// BaseURL returns the configured root, for diagnostics.
func (c *Client) BaseURL() string { return c.base.String() }

// URLFor exposes the absolute URL of a relative path, for diagnostics.
func (c *Client) URLFor(rel string) string { return c.urlFor(rel, false) }

// urlFor builds the absolute URL of a remote-root-relative path. Escaping is left
// to url.URL.String, which encodes each segment correctly - hand-rolled escaping
// gets "+", "#" and "?" in filenames wrong, and media filenames contain all three.
func (c *Client) urlFor(rel string, dir bool) string {
	u := *c.base
	u.RawPath = "" // stale after the copy; let String re-encode from Path
	u.Path = path.Join(c.root, cleanRel(rel))
	if dir && !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	return u.String()
}

func (c *Client) newRequest(ctx context.Context, method, rawurl string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawurl, body)
	if err != nil {
		return nil, fmt.Errorf("build %s %s: %w", method, rawurl, err)
	}
	if c.user != "" || c.pass != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	req.Header.Set("User-Agent", c.agent)
	return req, nil
}

// errStatus turns a non-2xx response into an error carrying a bounded body
// snippet - DSM puts the actual reason in the body more often than in the status.
func errStatus(resp *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("%s %s -> %s: %s",
		resp.Request.Method, resp.Request.URL.Path, resp.Status, strings.TrimSpace(string(snippet)))
}

// Propfind runs a PROPFIND on dir and returns every entry the server reported,
// including dir itself (with an empty Path when dir is the root). If raw is
// non-nil the response body is copied into it too, which is how the spike gets to
// look at DSM's actual XML.
func (c *Client) Propfind(ctx context.Context, dir, depth string, raw io.Writer) ([]remote.Entry, error) {
	req, err := c.newRequest(ctx, "PROPFIND", c.urlFor(dir, depth != Depth0), strings.NewReader(propfindBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", depth)
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("propfind %q: %w", dir, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errStatus(resp)
	}

	var body io.Reader = resp.Body
	if raw != nil {
		body = io.TeeReader(body, raw)
	}
	entries, nonNFC, err := decodeMultistatus(body, c.root)
	if err != nil {
		return nil, fmt.Errorf("propfind %q: %w", dir, err)
	}
	c.nonNFC.Add(int64(nonNFC))
	return entries, nil
}

// List implements remote.Remote.
func (c *Client) List(ctx context.Context, dir string) ([]remote.Entry, error) {
	entries, err := c.Propfind(ctx, dir, Depth1, nil)
	if err != nil {
		return nil, err
	}

	self := cleanRel(dir)
	out := entries[:0]
	for _, e := range entries {
		if e.Path == self {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// Stat implements remote.Remote.
func (c *Client) Stat(ctx context.Context, p string) (remote.Entry, error) {
	entries, err := c.Propfind(ctx, p, Depth0, nil)
	if err != nil {
		return remote.Entry{}, err
	}

	want := cleanRel(p)
	for _, e := range entries {
		if e.Path == want {
			return e, nil
		}
	}
	if len(entries) == 1 {
		return entries[0], nil
	}
	return remote.Entry{}, fmt.Errorf("stat %q: no matching response in multistatus (%d entries)", p, len(entries))
}

// ProbeDepthInfinity reports whether the server honours Depth: infinity on dir.
// RFC 4918 lets a server refuse with 403 and a propfind-finite-depth condition,
// and DSM is expected to; the walk then falls back to one request per directory.
// reason carries the server's answer either way.
func (c *Client) ProbeDepthInfinity(ctx context.Context, dir string) (bool, string, error) {
	req, err := c.newRequest(ctx, "PROPFIND", c.urlFor(dir, true), strings.NewReader(propfindBody))
	if err != nil {
		return false, "", err
	}
	req.Header.Set("Depth", DepthInfinity)
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)

	resp, err := c.http.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("propfind %q depth infinity: %w", dir, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, resp.Status, nil
	}

	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return false, fmt.Sprintf("%s: %s", resp.Status, strings.TrimSpace(string(snippet))), nil
}

// Exists reports whether path is present. This is the lock-file probe of §3, and
// an existence check is all it ever is: the body of a .~lock is a PID written by
// another machine, which is uninterpretable here and deliberately never read.
func (c *Client) Exists(ctx context.Context, p string) (bool, error) {
	req, err := c.newRequest(ctx, http.MethodHead, c.urlFor(p, false), nil)
	if err != nil {
		return false, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("head %q: %w", p, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, nil
	default:
		return false, errStatus(resp)
	}
}

// OpenRange streams length bytes of p starting at offset; length <= 0 means "to
// the end". It also returns the file's total size, -1 when the server did not say.
//
// A 200 to a ranged request means the server ignored Range and is sending from
// byte zero. That has to be an error rather than a short read: writing those bytes
// at the resume offset corrupts the file silently, and resume is the property the
// whole transfer design rests on.
func (c *Client) OpenRange(ctx context.Context, p string, offset, length int64) (io.ReadCloser, int64, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("get %q: negative offset %d", p, offset)
	}

	req, err := c.newRequest(ctx, http.MethodGet, c.urlFor(p, false), nil)
	if err != nil {
		return nil, 0, err
	}
	ranged := offset > 0 || length > 0
	if ranged {
		if length > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
		} else {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("get %q: %w", p, err)
	}

	switch {
	case resp.StatusCode == http.StatusPartialContent:
		total, err := totalFromContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			resp.Body.Close()
			return nil, 0, fmt.Errorf("get %q: %w", p, err)
		}
		return resp.Body, total, nil

	case resp.StatusCode == http.StatusOK && !ranged:
		return resp.Body, resp.ContentLength, nil

	case resp.StatusCode == http.StatusOK && offset == 0:
		// The range was ignored, but it started at zero, so the bytes are still the
		// ones asked for - there are just more of them than requested.
		return limitedBody{Reader: io.LimitReader(resp.Body, length), Closer: resp.Body}, resp.ContentLength, nil

	case resp.StatusCode == http.StatusOK:
		resp.Body.Close()
		return nil, 0, fmt.Errorf("get %q: server ignored %q and answered 200 - ranged resume is not available on this server",
			p, req.Header.Get("Range"))

	default:
		defer resp.Body.Close()
		return nil, 0, errStatus(resp)
	}
}

// Open implements remote.Remote.
func (c *Client) Open(ctx context.Context, p string, offset int64) (io.ReadCloser, error) {
	rc, _, err := c.OpenRange(ctx, p, offset, 0)
	return rc, err
}

// limitedBody caps a response body without losing the ability to close it.
type limitedBody struct {
	io.Reader
	io.Closer
}

// totalFromContentRange reads the instance length out of "bytes 100-199/12345".
// A server that answers "*" for the length returns -1, which is legal and only
// costs the caller its progress percentage.
func totalFromContentRange(v string) (int64, error) {
	if v == "" {
		return -1, nil
	}
	i := strings.LastIndex(v, "/")
	if i < 0 {
		return 0, fmt.Errorf("malformed Content-Range %q", v)
	}
	size := strings.TrimSpace(v[i+1:])
	if size == "*" {
		return -1, nil
	}
	n, err := strconv.ParseInt(size, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("malformed Content-Range %q: %w", v, err)
	}
	return n, nil
}
