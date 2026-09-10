package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/remote/localfs"
)

// TestWalker exercises the breadth-first walk over a generated tree, served by
// the local fake the design asks for: the walk is the most intricate part of the
// spike and needs no NAS to test.
func TestWalker(t *testing.T) {
	dir := t.TempDir()

	const (
		levels     = 3
		perLevel   = 4
		filesPerNS = 5
	)
	wantDirs, wantFiles := buildTree(t, dir, "", levels, perLevel, filesPerNS)

	client, err := localfs.New(dir)
	if err != nil {
		t.Fatalf("localfs.New: %v", err)
	}

	for _, workers := range []int{1, 4} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			w := &walker{client: client, log: &logs.Logger{}, workers: workers}
			w.walk(context.Background(), "", 0)

			if w.errors != 0 {
				t.Errorf("errors = %d, want 0", w.errors)
			}
			if w.dirs != int64(wantDirs) {
				t.Errorf("dirs = %d, want %d", w.dirs, wantDirs)
			}
			if w.files != int64(wantFiles) {
				t.Errorf("files = %d, want %d", w.files, wantFiles)
			}
			if w.bytes != int64(wantFiles) {
				t.Errorf("bytes = %d, want %d (every file is one byte)", w.bytes, wantFiles)
			}
		})
	}
}

// TestWalkerMaxDepth checks that -max-depth stops the walk where it says it does,
// which is what makes the command usable against the real 30 TB tree.
func TestWalkerMaxDepth(t *testing.T) {
	dir := t.TempDir()
	buildTree(t, dir, "", 3, 2, 1)

	client, err := localfs.New(dir)
	if err != nil {
		t.Fatalf("localfs.New: %v", err)
	}

	w := &walker{client: client, log: &logs.Logger{}, workers: 2}
	w.walk(context.Background(), "", 1)

	// One level means the root only: one listing, and its files.
	if w.requests != 1 {
		t.Errorf("requests = %d, want 1", w.requests)
	}
	if w.files != 1 {
		t.Errorf("files = %d, want 1", w.files)
	}
}

// buildTree writes a balanced directory tree and returns the number of
// directories the walk will list and the number of files it will see. The root
// counts as a listed directory.
func buildTree(t *testing.T, base, rel string, levels, perLevel, files int) (dirs, fileCount int) {
	t.Helper()

	full := filepath.Join(base, filepath.FromSlash(rel))
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", full, err)
	}
	dirs = 1

	for i := 0; i < files; i++ {
		name := filepath.Join(full, fmt.Sprintf("file-%d.mkv", i))
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
		fileCount++
	}
	if levels <= 1 {
		return dirs, fileCount
	}

	for i := 0; i < perLevel; i++ {
		d, f := buildTree(t, base, filepath.ToSlash(filepath.Join(rel, fmt.Sprintf("dir-%d", i))), levels-1, perLevel, files)
		dirs += d
		fileCount += f
	}
	return dirs, fileCount
}
