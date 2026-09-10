package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

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
