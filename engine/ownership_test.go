//go:build linux || darwin

package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Handing a file to its own uid and gid is the one chown a test can make
// without root, and it still goes through the whole path.
func TestSyncAppliesThePairsOwnershipToWhatItCreates(t *testing.T) {
	h := newHarness(t)
	h.pair.Owner = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	h.pair.FileMode, h.pair.DirMode = "0600", "0700"
	if err := h.store.UpdatePair(context.Background(), &h.pair); err != nil {
		t.Fatalf("update pair: %v", err)
	}
	if err := os.Chmod(h.dst, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	body := h.write("Serien/S01/e01.mkv", 1000)
	h.scan()
	h.sync()
	h.wantFile("Serien/S01/e01.mkv", body)

	want := map[string]os.FileMode{
		h.dst:                                      0o755, // existed already, so it is left alone
		filepath.Join(h.dst, "Serien"):             0o700,
		filepath.Join(h.dst, "Serien/S01"):         0o700,
		filepath.Join(h.dst, PartDir):              0o700,
		filepath.Join(h.dst, "Serien/S01/e01.mkv"): 0o600,
	}
	for path, perm := range want {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Mode().Perm() != perm {
			t.Errorf("%s: mode %04o, want %04o", path, info.Mode().Perm(), perm)
		}
		st := info.Sys().(*syscall.Stat_t)
		if int(st.Uid) != os.Getuid() || int(st.Gid) != os.Getgid() {
			t.Errorf("%s: owned by %d:%d", path, st.Uid, st.Gid)
		}
	}
}

func TestSyncRefusesAnOwnerItCannotSet(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can hand a file to anyone")
	}
	h := newHarness(t, func(o *Options) { o.MaxAttempts = 1 })
	h.pair.Owner = "0:0"
	if err := h.store.UpdatePair(context.Background(), &h.pair); err != nil {
		t.Fatalf("update pair: %v", err)
	}

	h.write("a.bin", 10)
	h.scan()
	if res := h.sync(); res.Files != 0 || res.Failed != 1 {
		t.Errorf("sync moved %d and failed %d files with an owner it cannot set, want 0 and 1", res.Files, res.Failed)
	}
	if _, err := os.Stat(filepath.Join(h.dst, PartDir)); !os.IsNotExist(err) {
		t.Errorf("the staging directory it could not hand over was kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.dst, "a.bin")); !os.IsNotExist(err) {
		t.Errorf("the file appeared anyway: %v", err)
	}
}
