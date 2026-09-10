// Package smb is the client the mirror reads the publisher's share with,
// reduced to what the engine needs: list a directory, stat a path, probe for a
// lock file, and read a bounded range of a file. There is no write side.
//
// SMB is what the publisher's DSM actually serves. The library dials over a
// net.Conn the caller hands it, which is the whole reason it fits here: the
// connection comes out of the userspace WireGuard tunnel's netstack, so nothing
// on this side needs a kernel mount or a privileged container (DESIGN.md §2.1).
package smb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/hirochachacha/go-smb2"
	"golang.org/x/text/unicode/norm"

	"blackforestbytes.com/jcc-mirror/remote"
)

// DefaultPort is where SMB2 listens. A host written down without one gets it,
// which is how the setting is written in practice.
const DefaultPort = "445"

// The NTSTATUS codes that mean "there is nothing at that path". go-smb2 turns
// the first two into fs.ErrNotExist itself; the rest arrive as a
// *smb2.ResponseError, and Samba - which is what DSM runs - does use them.
//
// This is an explicit list rather than a catch-all because the lock gate of
// DESIGN.md §3 must never read "cannot tell" as "nobody is using it": a refused
// request or a tunnel that went down has to stay an error.
const (
	statusNoSuchFile         = 0xC000000F
	statusObjectNameNotFound = 0xC0000034
	statusObjectPathNotFound = 0xC000003A
	statusNotADirectory      = 0xC0000103
)

// DialFunc opens the TCP connection the session runs over. Passing the tunnel's
// DialContext is what puts the whole session inside WireGuard.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Config configures a Client.
type Config struct {
	Host     string // host or host:port; DefaultPort when no port is given
	Share    string // the share name alone, without separators or a server prefix
	Path     string // optional directory inside the share; the remote root of the mirror
	User     string
	Password string
	Domain   string // optional NTLM domain or workgroup
	Dial     DialFunc
}

// Client is one SMB session against one share, rooted at the configured base
// path: every path passed to and returned from it is relative to that. It is
// safe for concurrent use.
type Client struct {
	addr    string // what Dial is given
	display string // the host as an operator wrote it, for Target
	share   string
	base    string // remote root inside the share, slash-separated, possibly empty
	user    string
	pass    string
	domain  string
	dial    DialFunc

	nonNFC atomic.Int64

	// mu guards the session and everything that comes with it. Unlike an HTTP
	// request an SMB session is stateful and long-lived, so it is established
	// once and kept; the dial happens with the lock held, which is what stops two
	// callers from opening two sessions where one is wanted.
	mu     sync.Mutex
	conn   net.Conn
	sess   *smb2.Session
	mount  *smb2.Share
	gen    uint64 // bumped per session, so a stale caller cannot tear down a fresh one
	closed bool
}

var (
	_ remote.Remote      = (*Client)(nil)
	_ remote.Prober      = (*Client)(nil)
	_ remote.RangeReader = (*Client)(nil)
)

var errClosed = errors.New("the smb client is closed")

// New builds a client. Nothing is dialed here: the session comes up on the first
// operation, so a daemon whose tunnel is not up yet still starts.
func New(cfg Config) (*Client, error) {
	addr, display, err := hostPort(cfg.Host)
	if err != nil {
		return nil, fmt.Errorf("smb host: %w", err)
	}

	share := strings.Trim(strings.TrimSpace(cfg.Share), `\/`)
	if share == "" {
		return nil, errors.New("smb: no share name")
	}
	if strings.ContainsAny(share, `\/`) {
		return nil, fmt.Errorf("smb share %q: a share name is one segment, without a path", cfg.Share)
	}

	base, err := cleanRel(cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("smb path: %w", err)
	}

	dial := cfg.Dial
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}

	return &Client{
		addr: addr, display: display, share: share, base: base,
		user: cfg.User, pass: cfg.Password, domain: cfg.Domain, dial: dial,
	}, nil
}

// Close ends the session and lets the connection go. The client is not reusable
// afterwards: it is called where a reload replaces the client, and a session
// left behind there is a TCP connection to the publisher leaked per config save.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return c.teardownLocked()
}

// NonNFCNames counts the entry names seen so far that were not already in NFC.
// Anything but zero means the source tree is partly NFD and the diff has to
// normalize, or every affected title looks new on every scan (DESIGN.md §2.3).
func (c *Client) NonNFCNames() int64 { return c.nonNFC.Load() }

// Target names the remote root the way an operator writes it down, for the
// probe, the events and the diagnostics.
func (c *Client) Target() string { return c.TargetFor("") }

// TargetFor is Target with a remote-root-relative path appended.
func (c *Client) TargetFor(rel string) string {
	out := `\\` + c.display + `\` + c.share
	if p, err := cleanRel(rel); err == nil {
		if full := c.sharePath(p); full != "" {
			out += `\` + full
		}
	}
	return out
}

// List implements remote.Remote.
func (c *Client) List(ctx context.Context, dir string) ([]remote.Entry, error) {
	rel, err := cleanRel(dir)
	if err != nil {
		return nil, err
	}

	var infos []os.FileInfo
	if err := c.do(ctx, func(s *smb2.Share) error {
		var err error
		infos, err = s.ReadDir(c.sharePath(rel))
		return err
	}); err != nil {
		return nil, fmt.Errorf("list %q: %w", dir, err)
	}

	out := make([]remote.Entry, 0, len(infos))
	for _, fi := range infos {
		name := fi.Name()
		if !norm.NFC.IsNormalString(name) {
			c.nonNFC.Add(1)
			name = norm.NFC.String(name)
		}
		out = append(out, entryOf(path.Join(rel, name), fi))
	}

	// The server's order is its own business, and the fake sorts by the
	// normalized path; sorting here is what keeps the two comparable.
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Stat implements remote.Remote.
func (c *Client) Stat(ctx context.Context, p string) (remote.Entry, error) {
	rel, err := cleanRel(p)
	if err != nil {
		return remote.Entry{}, err
	}

	var fi os.FileInfo
	if err := c.do(ctx, func(s *smb2.Share) error {
		var err error
		fi, err = s.Stat(c.sharePath(rel))
		return err
	}); err != nil {
		return remote.Entry{}, fmt.Errorf("stat %q: %w", p, err)
	}
	return entryOf(rel, fi), nil
}

// Exists implements remote.Prober. This is the lock-file probe of §3, and an
// existence check is all it ever is: the body of a .~lock is a PID written by
// another machine, which is uninterpretable here and deliberately never read.
func (c *Client) Exists(ctx context.Context, p string) (bool, error) {
	rel, err := cleanRel(p)
	if err != nil {
		return false, err
	}

	var found bool
	if err := c.do(ctx, func(s *smb2.Share) error {
		_, err := s.Stat(c.sharePath(rel))
		switch {
		case err == nil:
			found = true
			return nil
		case notFound(err):
			found = false
			return nil
		default:
			return err
		}
	}); err != nil {
		return false, fmt.Errorf("stat %q: %w", p, err)
	}
	return found, nil
}

// Open implements remote.Remote.
func (c *Client) Open(ctx context.Context, p string, offset int64) (io.ReadCloser, error) {
	rc, _, err := c.OpenRange(ctx, p, offset, 0)
	return rc, err
}

// OpenRange streams length bytes of p starting at offset; length <= 0 means "to
// the end". It also returns the file's total size.
//
// The reader stays bound to the session it was opened on. A session that dies
// mid-stream fails the read rather than resuming silently somewhere else, which
// is what the transfer engine's per-chunk retry expects.
func (c *Client) OpenRange(ctx context.Context, p string, offset, length int64) (io.ReadCloser, int64, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("open %q: negative offset %d", p, offset)
	}
	rel, err := cleanRel(p)
	if err != nil {
		return nil, 0, err
	}

	var (
		body io.ReadCloser
		size int64
	)
	if err := c.do(ctx, func(s *smb2.Share) error {
		var err error
		body, size, err = openRange(s, c.sharePath(rel), offset, length)
		return err
	}); err != nil {
		return nil, 0, fmt.Errorf("open %q: %w", p, err)
	}
	return body, size, nil
}

// openRange does the work of OpenRange on one session, so the retry can simply
// run it again. It owns the file handle until it hands it back: every error path
// closes it, or a failed attempt would leave one open on the server.
func openRange(s *smb2.Share, name string, offset, length int64) (io.ReadCloser, int64, error) {
	f, err := s.Open(name)
	if err != nil {
		return nil, 0, err
	}

	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	// A read past the end has to be an error rather than an empty body: an empty
	// body at a resume offset looks exactly like a finished file.
	if offset > st.Size() {
		f.Close()
		return nil, 0, fmt.Errorf("offset %d is past the end (%d bytes)", offset, st.Size())
	}

	pos, err := f.Seek(offset, io.SeekStart)
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	// Every SMB read carries its own absolute offset, so the server cannot
	// quietly answer from byte zero the way an HTTP server can ignore a Range
	// header. The position is still checked rather than assumed, because the
	// property the whole transfer design rests on is that the first byte read is
	// the byte asked for: anything else writes the start of a file into its
	// middle, and does it silently.
	if pos != offset {
		f.Close()
		return nil, 0, fmt.Errorf("seek to %d landed at %d", offset, pos)
	}

	if length <= 0 {
		return f, st.Size(), nil
	}
	return boundedFile{Reader: io.LimitReader(f, length), Closer: f}, st.Size(), nil
}

// boundedFile caps a read without losing the ability to close the file.
type boundedFile struct {
	io.Reader
	io.Closer
}

// do runs one operation against the session, bringing it up first if there is
// none. A connection-level failure is retried exactly once on a fresh session: a
// sync runs for days and a tunnel blip must not fail the whole run, while a
// server that refuses every attempt still fails in bounded time.
//
// Only the connection going away is retried. An NTSTATUS is the server
// answering, and asking it twice would only get the same answer.
func (c *Client) do(ctx context.Context, fn func(*smb2.Share) error) error {
	share, gen, err := c.session(ctx)
	if err != nil {
		return err
	}

	err = fn(share.WithContext(ctx))
	if err == nil || ctx.Err() != nil || !connBroken(err) {
		return err
	}

	c.reset(gen)
	share, _, redial := c.session(ctx)
	if redial != nil {
		return fmt.Errorf("%w (re-dialing failed too: %v)", err, redial)
	}
	return fn(share.WithContext(ctx))
}

// session returns the mounted share, establishing one if there is none. The dial
// happens with the lock held, so a second caller arriving mid-dial waits and then
// finds the session already there - go-smb2 does not multiplex two sessions over
// one connection, so two concurrent dials would mean two connections to the
// publisher, one of them leaked.
func (c *Client) session(ctx context.Context) (*smb2.Share, uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, 0, errClosed
	}
	if c.mount != nil {
		return c.mount, c.gen, nil
	}

	conn, err := c.dial(ctx, "tcp", c.addr)
	if err != nil {
		return nil, 0, fmt.Errorf("dial %s: %w", c.addr, err)
	}

	d := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{
		User: c.user, Password: c.pass, Domain: c.domain,
	}}
	sess, err := d.DialContext(ctx, conn)
	if err != nil {
		conn.Close()
		return nil, 0, fmt.Errorf("log in to %s as %q: %w", c.addr, c.user, err)
	}

	mount, err := sess.WithContext(ctx).Mount(c.share)
	if err != nil {
		// Logoff closes the connection only when its round trip succeeds, so the
		// connection is closed here either way.
		_ = sess.Logoff()
		conn.Close()
		return nil, 0, fmt.Errorf("mount %s: %w", c.Target(), err)
	}

	c.conn, c.sess, c.mount = conn, sess, mount
	c.gen++
	return c.mount, c.gen, nil
}

// reset drops the session, but only when it is still the one the caller was
// using. Two callers failing on the same dead connection would otherwise have
// the second tear down the session the first has already replaced.
func (c *Client) reset(gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen == gen {
		_ = c.teardownLocked()
	}
}

// teardownLocked ends the session politely and then closes the connection. The
// polite half is best effort - a session whose connection has already gone fails
// it - but the connection has to be let go of regardless, since go-smb2 closes
// it only on a logoff that got an answer.
func (c *Client) teardownLocked() error {
	if c.mount == nil {
		return nil
	}
	_ = c.mount.Umount()
	_ = c.sess.Logoff()
	err := c.conn.Close()
	c.mount, c.sess, c.conn = nil, nil, nil
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// connBroken reports whether err is the connection going away rather than the
// server saying no.
func connBroken(err error) bool {
	var transport *smb2.TransportError
	if errors.As(err, &transport) {
		return true
	}
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE)
}

// notFound reports whether err is the server saying the path is not there.
func notFound(err error) bool {
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	var resp *smb2.ResponseError
	if errors.As(err, &resp) {
		switch resp.Code {
		case statusNoSuchFile, statusObjectNameNotFound, statusObjectPathNotFound, statusNotADirectory:
			return true
		}
	}
	return false
}

// entryOf fills a remote.Entry from what the server reported about rel.
func entryOf(rel string, fi os.FileInfo) remote.Entry {
	e := remote.Entry{
		Path:  rel,
		Name:  path.Base("/" + rel),
		IsDir: fi.IsDir(),
		// SMB carries a Windows FILETIME, so this is good to 100 ns - finer than
		// the manifest, which stores milliseconds. The mtime tolerance of the diff
		// is what absorbs that, and is why it must never be zero.
		MTime: fi.ModTime().UTC(),
	}
	if rel == "" {
		e.Name = ""
	}
	if !e.IsDir {
		e.Size = fi.Size()
	}
	return e
}

// sharePath maps a remote-root-relative path onto the share. go-smb2 separates
// with "\" and converts "/" itself, but the conversion is done here so exactly
// one place knows about it - and so a leading separator, which the library
// refuses outright, can never be produced.
func (c *Client) sharePath(rel string) string {
	joined := rel
	if c.base != "" {
		joined = path.Join(c.base, rel)
	}
	return strings.ReplaceAll(joined, "/", `\`)
}

// cleanRel puts a caller-supplied path into the form every Entry.Path carries:
// relative to the remote root, slash-separated, no leading slash, NFC.
//
// A "\" counts as a separator too, so a path in either notation is checked by
// the same rule. A segment that climbs is refused rather than resolved away: the
// path is handed to the server as text, and what a share makes of ".." is the
// server's business rather than ours.
func cleanRel(rel string) (string, error) {
	clean := strings.Trim(strings.ReplaceAll(rel, `\`, "/"), "/")
	if clean == "" {
		return "", nil
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == ".." {
			return "", fmt.Errorf("path %q climbs out of the remote root", rel)
		}
	}
	return norm.NFC.String(strings.TrimPrefix(path.Clean("/"+clean), "/")), nil
}

// hostPort splits the configured host into what to dial and what to show. The
// two differ because a UNC name has no room for a port, so one is only worth
// printing when it is not the default.
func hostPort(raw string) (addr, display string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("no host")
	}

	if host, port, splitErr := net.SplitHostPort(raw); splitErr == nil {
		if host == "" || port == "" {
			return "", "", fmt.Errorf("%q is not host or host:port", raw)
		}
		if port == DefaultPort {
			return net.JoinHostPort(host, port), host, nil
		}
		return net.JoinHostPort(host, port), raw, nil
	}

	// An IPv6 literal written without a port has colons of its own; JoinHostPort
	// is what puts the brackets on.
	if _, addrErr := netip.ParseAddr(raw); addrErr != nil && strings.ContainsAny(raw, ":[]") {
		return "", "", fmt.Errorf("%q is not host or host:port", raw)
	}
	return net.JoinHostPort(raw, DefaultPort), raw, nil
}
