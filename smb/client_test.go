package smb

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"testing"

	"github.com/hirochachacha/go-smb2"
)

func TestHostPort(t *testing.T) {
	cases := []struct {
		in      string
		addr    string
		display string
		bad     bool
	}{
		{in: "10.13.13.2", addr: "10.13.13.2:445", display: "10.13.13.2"},
		{in: " nas.local ", addr: "nas.local:445", display: "nas.local"},
		// The default port is dialed either way but never shown: a UNC name has no
		// room for one, and printing it would only invite pasting it somewhere that
		// cannot take it.
		{in: "10.13.13.2:445", addr: "10.13.13.2:445", display: "10.13.13.2"},
		{in: "10.13.13.2:4450", addr: "10.13.13.2:4450", display: "10.13.13.2:4450"},
		{in: "fd00::2", addr: "[fd00::2]:445", display: "fd00::2"},
		{in: "[fd00::2]:4450", addr: "[fd00::2]:4450", display: "[fd00::2]:4450"},
		{in: "", bad: true},
		{in: "nas:", bad: true},
		{in: "nas:1:2", bad: true},
	}
	for _, c := range cases {
		addr, display, err := hostPort(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("hostPort(%q) = %q, want an error", c.in, addr)
			}
			continue
		}
		if err != nil {
			t.Errorf("hostPort(%q): %v", c.in, err)
			continue
		}
		if addr != c.addr || display != c.display {
			t.Errorf("hostPort(%q) = (%q, %q), want (%q, %q)", c.in, addr, display, c.addr, c.display)
		}
	}
}

func TestCleanRel(t *testing.T) {
	ok := map[string]string{
		"":                           "",
		"/":                          "",
		"Filme A-Z":                  "Filme A-Z",
		"/Filme A-Z/":                "Filme A-Z",
		"Filme A-Z\\Gr\u00fc\u00dfe": "Filme A-Z/Gr\u00fc\u00dfe",
		"Filme A-Z//./a.mkv":         "Filme A-Z/a.mkv",
	}
	for in, want := range ok {
		got, err := cleanRel(in)
		if err != nil {
			t.Errorf("cleanRel(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("cleanRel(%q) = %q, want %q", in, got, want)
		}
	}

	// A backslash counts as a separator, so a climbing segment is caught whichever
	// notation it arrives in.
	for _, in := range []string{"..", "../outside", "Filme/../../outside", `Filme\..\..\outside`} {
		if got, err := cleanRel(in); err == nil {
			t.Errorf("cleanRel(%q) = %q, want a refusal", in, got)
		}
	}
}

// The NFC normalization is the same one the manifest and the differ apply: a
// name that arrives decomposed has to come back composed, or every title with an
// umlaut looks new on every scan (DESIGN.md §2.3).
func TestCleanRelNormalizes(t *testing.T) {
	const (
		decomposed = "Gru\u0308\u00dfe" // u plus a combining diaeresis, which is what macOS writes
		composed   = "Gr\u00fc\u00dfe"
	)
	got, err := cleanRel("Filme/" + decomposed)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Filme/"+composed {
		t.Errorf("cleanRel(%+q) = %+q, want %+q", decomposed, got, composed)
	}
}

func TestSharePathAndTarget(t *testing.T) {
	rooted, err := New(Config{Host: "10.13.13.2", Share: "media", Path: "/Collection/", User: "ro"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := rooted.sharePath("Filme A-Z/Grüße.mkv"); got != `Collection\Filme A-Z\Grüße.mkv` {
		t.Errorf("sharePath = %q", got)
	}
	// The remote root itself: no leading separator, which go-smb2 refuses outright.
	if got := rooted.sharePath(""); got != "Collection" {
		t.Errorf("sharePath(root) = %q", got)
	}
	if got := rooted.Target(); got != `\\10.13.13.2\media\Collection` {
		t.Errorf("Target = %q", got)
	}
	if got := rooted.TargetFor("Filme A-Z"); got != `\\10.13.13.2\media\Collection\Filme A-Z` {
		t.Errorf("TargetFor = %q", got)
	}

	bare, err := New(Config{Host: "nas:4450", Share: "media", User: "ro"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := bare.sharePath(""); got != "" {
		t.Errorf("sharePath(root) = %q, want the share itself", got)
	}
	if got := bare.Target(); got != `\\nas:4450\media` {
		t.Errorf("Target = %q", got)
	}
}

func TestNewRefusesWhatCannotBeMounted(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no host":       {Share: "media"},
		"no share":      {Host: "10.13.13.2"},
		"share is path": {Host: "10.13.13.2", Share: `media\Filme`},
		"path climbs":   {Host: "10.13.13.2", Share: "media", Path: "../elsewhere"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New succeeded, want an error", name)
		}
	}
}

// notFound decides whether the lock gate of DESIGN.md §3 reads a path as absent.
// Reading "cannot tell" as "nobody is using it" is the one mistake it must never
// make, so the mapping is exact rather than a catch-all.
func TestNotFound(t *testing.T) {
	absent := []error{
		&os.PathError{Op: "stat", Path: `db\x.~lock`, Err: fs.ErrNotExist},
		&os.PathError{Op: "stat", Path: `db\x.~lock`, Err: &smb2.ResponseError{Code: statusNoSuchFile}},
		&smb2.ResponseError{Code: statusObjectPathNotFound},
		&smb2.ResponseError{Code: statusNotADirectory},
	}
	for _, err := range absent {
		if !notFound(err) {
			t.Errorf("notFound(%v) = false, want true", err)
		}
	}

	present := []error{
		nil,
		&os.PathError{Op: "stat", Path: "x", Err: fs.ErrPermission},
		// Access denied, which is emphatically not "there is nothing there".
		&smb2.ResponseError{Code: 0xC0000022},
		&smb2.TransportError{Err: io.ErrUnexpectedEOF},
		errors.New("the tunnel is down"),
	}
	for _, err := range present {
		if notFound(err) {
			t.Errorf("notFound(%v) = true, want false", err)
		}
	}
}

// connBroken decides what is worth a re-dial. An NTSTATUS is the server
// answering and must not be retried; the connection going away is exactly what a
// sync running for days has to survive.
func TestConnBroken(t *testing.T) {
	broken := []error{
		&smb2.TransportError{Err: io.EOF},
		&os.PathError{Op: "read", Path: "x", Err: &smb2.TransportError{Err: net.ErrClosed}},
		io.ErrUnexpectedEOF,
		net.ErrClosed,
	}
	for _, err := range broken {
		if !connBroken(err) {
			t.Errorf("connBroken(%v) = false, want true", err)
		}
	}

	intact := []error{
		&smb2.ResponseError{Code: statusObjectNameNotFound},
		&os.PathError{Op: "stat", Path: "x", Err: fs.ErrPermission},
		errors.New("mount refused"),
	}
	for _, err := range intact {
		if connBroken(err) {
			t.Errorf("connBroken(%v) = true, want false", err)
		}
	}
}

// A closed client answers rather than dialing again. The daemon replaces its
// client on every reload, and one that quietly re-opened a session after being
// closed would be a connection to the publisher nobody owns any more.
func TestClosedClientRefuses(t *testing.T) {
	c, err := New(Config{Host: "10.13.13.2", Share: "media", User: "ro"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.Stat(t.Context(), "anything"); !errors.Is(err, errClosed) {
		t.Errorf("Stat after Close = %v, want errClosed", err)
	}
	// Twice is harmless: the daemon closes on reload and again on shutdown.
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// Everything that can be settled without the publisher is settled without him: a
// path that climbs and a negative offset are caller mistakes, and refusing them
// before the dial means a typo does not cost a connection.
func TestBadArgumentsNeverDial(t *testing.T) {
	c, err := New(Config{
		Host: "10.13.13.2", Share: "media", User: "ro",
		Dial: func(context.Context, string, string) (net.Conn, error) {
			t.Error("a refusable argument still opened a connection")
			return nil, errors.New("should not have been called")
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	ctx := t.Context()
	if _, err := c.Stat(ctx, "../outside"); err == nil {
		t.Error("Stat of a climbing path succeeded")
	}
	if _, err := c.List(ctx, "Filme/../.."); err == nil {
		t.Error("List of a climbing path succeeded")
	}
	if _, err := c.Exists(ctx, "../outside"); err == nil {
		t.Error("Exists of a climbing path succeeded")
	}
	if _, _, err := c.OpenRange(ctx, "Filme/a.mkv", -1, 0); err == nil {
		t.Error("OpenRange with a negative offset succeeded")
	}
}
