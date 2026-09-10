package main

import (
	"context"
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
