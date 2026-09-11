package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/store"
	"blackforestbytes.com/jcc-mirror/wg"
)

// The diagnostic commands and the daemon have to agree about the tunnel, and an
// int flag carries its default whether or not it was given - so without fs.Visit
// an imported MTU reaches the daemon and silently not these commands.
func TestStoredMTUAndKeepaliveReachTheFlags(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.ConfigSet(ctx, map[string]string{
		store.KeyWGMTU:       "1380",
		store.KeyWGKeepalive: "0",
	}, "test"); err != nil {
		t.Fatalf("ConfigSet: %v", err)
	}
	st.Close()

	fs, cfg := newFlagSet("test")
	if err := fs.Parse([]string{"-data", dir}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cfg.fromStore(ctx); err != nil {
		t.Fatalf("fromStore: %v", err)
	}
	if cfg.wgMTU != 1380 {
		t.Errorf("mtu = %d, want the stored 1380", cfg.wgMTU)
	}
	if cfg.wgKeepalive != 0 {
		t.Errorf("keepalive = %d, want the stored 0", cfg.wgKeepalive)
	}

	// A flag that was given still wins, which is what these commands are for.
	fs2, cfg2 := newFlagSet("test")
	if err := fs2.Parse([]string{"-data", dir, "-wg-mtu", "1200"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cfg2.fromStore(ctx); err != nil {
		t.Fatalf("fromStore: %v", err)
	}
	if cfg2.wgMTU != 1200 {
		t.Errorf("mtu = %d, want the flag's 1200", cfg2.wgMTU)
	}

	// And with nothing stored the wg defaults survive.
	fs3, cfg3 := newFlagSet("test")
	if err := fs3.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cfg3.fromStore(ctx); err != nil {
		t.Fatalf("fromStore: %v", err)
	}
	if cfg3.wgMTU != wg.DefaultMTU || cfg3.wgKeepalive != wg.DefaultKeepalive {
		t.Errorf("mtu %d keepalive %d, want the wg defaults", cfg3.wgMTU, cfg3.wgKeepalive)
	}
}

// cmdPubkey has to read the store like every other command: the key it derives
// from is normally the one the daemon seeded or the import wrote, and there is
// no reason to make someone paste it back in on the command line.
func TestPubkeyReadsTheStoredPrivateKey(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	priv, err := wg.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	want, err := wg.PublicKey(priv)
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}

	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.ConfigSet(ctx, map[string]string{store.KeyWGPrivateKey: priv}, "test"); err != nil {
		t.Fatalf("ConfigSet: %v", err)
	}
	st.Close()

	out := captureStdout(t, func() {
		if err := cmdPubkey(ctx, []string{"-data", dir}); err != nil {
			t.Fatalf("pubkey: %v", err)
		}
	})
	if strings.TrimSpace(out) != want {
		t.Errorf("pubkey printed %q, want %q", strings.TrimSpace(out), want)
	}
}

// seedRemotes stores two remotes. The first is what everything that names no
// remote falls back to.
func seedRemotes(t *testing.T, dir string) (first, second store.Remote) {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	first = store.Remote{Name: "media", Host: "10.13.13.2", Share: "media", Path: "Filme", User: "ro", Password: "pw"}
	second = store.Remote{Name: "archive", Host: "10.13.13.3:4450", Share: "archive", User: "backup", Domain: "WG"}
	for _, r := range []*store.Remote{&first, &second} {
		if err := st.CreateRemote(ctx, r); err != nil {
			t.Fatalf("CreateRemote %q: %v", r.Name, err)
		}
	}
	return first, second
}

func parseConfig(t *testing.T, args ...string) *config {
	t.Helper()
	fs, cfg := newFlagSet("test")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return cfg
}

func TestDiagnosticsTakeTheShareFromAStoredRemote(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	first, second := seedRemotes(t, dir)

	cfg := parseConfig(t, "-data", dir)
	if err := cfg.remoteFromStore(ctx); err != nil {
		t.Fatalf("remoteFromStore: %v", err)
	}
	if cfg.smbHost != first.Host || cfg.smbShare != first.Share || cfg.smbPath != first.Path ||
		cfg.smbUser != first.User || cfg.smbPass != first.Password {
		t.Errorf("without -from got %s/%s/%s as %s, want the first remote", cfg.smbHost, cfg.smbShare, cfg.smbPath, cfg.smbUser)
	}

	for _, ref := range []string{second.Name, strconv.FormatInt(second.ID, 10)} {
		cfg := parseConfig(t, "-data", dir, "-from", ref)
		if err := cfg.remoteFromStore(ctx); err != nil {
			t.Fatalf("remoteFromStore -from %s: %v", ref, err)
		}
		// Nothing of the first remote may leak into the gaps the second leaves.
		if cfg.smbHost != second.Host || cfg.smbDomain != second.Domain || cfg.smbPath != "" || cfg.smbPass != "" {
			t.Errorf("-from %s got host %q path %q domain %q pass %q, want the second remote's",
				ref, cfg.smbHost, cfg.smbPath, cfg.smbDomain, cfg.smbPass)
		}
	}

	cfg = parseConfig(t, "-data", dir, "-from", second.Name, "-host", "192.0.2.1", "-share-path", "x")
	if err := cfg.remoteFromStore(ctx); err != nil {
		t.Fatalf("remoteFromStore: %v", err)
	}
	if cfg.smbHost != "192.0.2.1" || cfg.smbPath != "x" || cfg.smbShare != second.Share {
		t.Errorf("got %s/%s/%s, want the flags over the second remote's share", cfg.smbHost, cfg.smbShare, cfg.smbPath)
	}

	cfg = parseConfig(t, "-data", dir, "-from", "nope")
	if err := cfg.remoteFromStore(ctx); !errors.Is(err, store.ErrNoRemote) {
		t.Errorf("-from nope: err = %v, want ErrNoRemote", err)
	}

	cfg = parseConfig(t, "-from", second.Name)
	if err := cfg.remoteFromStore(ctx); err == nil {
		t.Errorf("-from without -data: want an error")
	}

	// With nothing stored the flags are all there is, and that is not an error.
	cfg = parseConfig(t, "-data", t.TempDir(), "-host", "192.0.2.1")
	if err := cfg.remoteFromStore(ctx); err != nil || cfg.smbHost != "192.0.2.1" {
		t.Errorf("empty store: host %q err %v, want the flag and no error", cfg.smbHost, err)
	}
}

func TestMirrorCommandsReadThroughThePairsRemote(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	first, second := seedRemotes(t, dir)

	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	onSecond := store.Pair{Name: "old", LocalPath: "/mnt/old", RemoteID: second.ID}
	orphan := store.Pair{Name: "orphan", LocalPath: "/mnt/orphan"}
	for _, p := range []*store.Pair{&onSecond, &orphan} {
		if err := st.CreatePair(ctx, p); err != nil {
			t.Fatalf("CreatePair %q: %v", p.Name, err)
		}
	}

	pick := func(p store.Pair, args ...string) (*config, error) {
		cfg := parseConfig(t, append([]string{"-data", dir}, args...)...)
		if err := cfg.pickRemote(ctx, st, p); err != nil {
			return cfg, err
		}
		return cfg, cfg.remoteFromStore(ctx)
	}

	cfg, err := pick(onSecond)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if cfg.smbHost != second.Host || cfg.smbPath != "" {
		t.Errorf("got %s path %q, want the pair's own remote rather than the first", cfg.smbHost, cfg.smbPath)
	}

	cfg, err = pick(onSecond, "-from", first.Name)
	if err != nil {
		t.Fatalf("pick -from: %v", err)
	}
	if cfg.smbHost != first.Host {
		t.Errorf("-from %s got %s, want it over the pair's remote", first.Name, cfg.smbHost)
	}

	if _, err := pick(orphan); err == nil || !strings.Contains(err.Error(), "pairs set orphan -from") {
		t.Errorf("pair with no remote: err = %v, want the hint to set one", err)
	}

	cfg, err = pick(orphan, "-host", "192.0.2.1")
	if err != nil {
		t.Fatalf("pick -host: %v", err)
	}
	if cfg.smbHost != "192.0.2.1" || cfg.smbShare != "" {
		t.Errorf("-host alone got %s/%s, want the flags and nothing of the first remote", cfg.smbHost, cfg.smbShare)
	}

	// -remote-dir needs no remote at all, all the way through openEngine.
	src := t.TempDir()
	cfg = parseConfig(t, "-data", dir, "-remote-dir", src)
	eng, _, pair, closeFn, err := cfg.openEngine(ctx, &logs.Logger{}, orphan.Name)
	if err != nil {
		t.Fatalf("openEngine -remote-dir: %v", err)
	}
	defer closeFn()
	if eng == nil || pair.ID != orphan.ID {
		t.Errorf("openEngine returned pair %d, want %d", pair.ID, orphan.ID)
	}
}

func TestPairsAddPicksTheOnlyRemote(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	pairRemoteOf := func(name string) int64 {
		t.Helper()
		st, err := store.Open(ctx, dir)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer st.Close()
		p, err := st.FindPair(ctx, name)
		if err != nil {
			t.Fatalf("FindPair %q: %v", name, err)
		}
		return p.RemoteID
	}
	run := func(args ...string) (string, error) {
		t.Helper()
		var err error
		out := captureStdout(t, func() { err = cmdPairs(ctx, args) })
		return out, err
	}

	out, err := run("add", "-data", dir, "-name", "none", "-local", "/mnt/none")
	if err != nil {
		t.Fatalf("add with no remote stored: %v", err)
	}
	if id := pairRemoteOf("none"); id != 0 || !strings.Contains(out, "remotes add") {
		t.Errorf("no remote stored: remote %d, output %q; want none and a hint to add one", id, out)
	}

	only := addRemote(t, dir, store.Remote{Name: "media", Host: "10.13.13.2", Share: "media", User: "ro"})
	if _, err := run("add", "-data", dir, "-name", "one", "-local", "/mnt/one"); err != nil {
		t.Fatalf("add with one remote stored: %v", err)
	}
	if id := pairRemoteOf("one"); id != only.ID {
		t.Errorf("one remote stored: pair reads from %d, want %d", id, only.ID)
	}

	second := addRemote(t, dir, store.Remote{Name: "archive", Host: "10.13.13.3", Share: "archive", User: "ro"})
	if _, err := run("add", "-data", dir, "-name", "two", "-local", "/mnt/two"); err == nil || !strings.Contains(err.Error(), "-from") {
		t.Errorf("two remotes stored, no -from: err = %v, want one asking for -from", err)
	}
	if _, err := run("add", "-data", dir, "-name", "two", "-local", "/mnt/two", "-from", "archive"); err != nil {
		t.Fatalf("add -from archive: %v", err)
	}
	if id := pairRemoteOf("two"); id != second.ID {
		t.Errorf("-from archive: pair reads from %d, want %d", id, second.ID)
	}

	if _, err := run("set", "two", "-data", dir, "-from", ""); err != nil {
		t.Fatalf("set -from \"\": %v", err)
	}
	if id := pairRemoteOf("two"); id != 0 {
		t.Errorf("set -from \"\": pair reads from %d, want none", id)
	}
}

func addRemote(t *testing.T, dir string, r store.Remote) store.Remote {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := st.CreateRemote(ctx, &r); err != nil {
		t.Fatalf("CreateRemote %q: %v", r.Name, err)
	}
	return r
}

func TestRemotesSetChangesOnlyWhatWasGiven(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	first, _ := seedRemotes(t, dir)

	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	p := store.Pair{Name: "films", LocalPath: "/mnt/films", RemoteID: first.ID}
	if err := st.CreatePair(ctx, &p); err != nil {
		t.Fatalf("CreatePair: %v", err)
	}
	st.Close()

	captureStdout(t, func() {
		if err := cmdRemotes(ctx, []string{"set", first.Name, "-data", dir, "-host", "10.13.13.9"}); err != nil {
			t.Fatalf("set -host: %v", err)
		}
	})
	st, err = store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got, err := st.RemoteByID(ctx, first.ID)
	st.Close()
	if err != nil {
		t.Fatalf("RemoteByID: %v", err)
	}
	if got.Host != "10.13.13.9" || got.Share != first.Share || got.Path != first.Path || got.Password != first.Password {
		t.Errorf("after -host: %+v, want only the host changed", got)
	}

	captureStdout(t, func() {
		if err := cmdRemotes(ctx, []string{"set", first.Name, "-data", dir, "-pass", ""}); err != nil {
			t.Fatalf("set -pass \"\": %v", err)
		}
	})
	st, err = store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got, err = st.RemoteByID(ctx, first.ID)
	st.Close()
	if err != nil {
		t.Fatalf("RemoteByID: %v", err)
	}
	if got.PasswordSet {
		t.Errorf("an explicit -pass \"\" left the password stored")
	}

	if err := cmdRemotes(ctx, []string{"rm", first.Name, "-data", dir}); !errors.Is(err, store.ErrRemoteInUse) {
		t.Errorf("rm of a remote a pair reads from: err = %v, want ErrRemoteInUse", err)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = saved }()

	fn()
	w.Close()

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read: %v", err)
	}
	return buf.String()
}
