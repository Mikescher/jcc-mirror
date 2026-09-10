package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/remote"
)

// cmdWalk is M0 step 4, and the measurement the schedule is built on: how long
// does a full metadata walk of the real tree actually take? No remote manifest
// exists, so this is what every scan will cost, forever (DESIGN.md §2.3, §10.4).
func cmdWalk(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("walk")
	root := fs.String("path", "", "remote directory to walk, relative to the remote root")
	workers := fs.Int("workers", 8, "directory listings in flight - the walk is latency-bound, so a little parallelism helps a lot and more helps nothing")
	out := fs.String("out", "", "write the manifest as NDJSON to this file")
	progress := fs.Duration("progress", 10*time.Second, "progress interval, 0 to disable")
	maxDepth := fs.Int("max-depth", 0, "stop after this many levels, 0 for the whole tree")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *workers < 1 {
		return fmt.Errorf("-workers must be at least 1")
	}

	logger := &logs.Logger{Verbose: cfg.verbose}
	rem, closeFn, err := cfg.openEngineRemote(ctx, logger)
	if err != nil {
		return err
	}
	defer closeFn()

	w := &walker{client: rem, log: logger, workers: *workers}

	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return fmt.Errorf("create manifest %q: %w", *out, err)
		}
		defer f.Close()

		buf := bufio.NewWriterSize(f, 1<<20)
		defer buf.Flush()
		w.manifest = json.NewEncoder(buf)
	}

	if *progress > 0 {
		stop := w.reportProgress(ctx, *progress)
		defer stop()
	}

	start := time.Now()
	w.walk(ctx, *root, *maxDepth)
	elapsed := time.Since(start)

	w.summarize(*root, elapsed, *out)
	if ctx.Err() != nil {
		return fmt.Errorf("walk interrupted after %s", format.Duration(elapsed))
	}
	if w.errors > 0 {
		return fmt.Errorf("%d directory listing(s) failed", w.errors)
	}
	return nil
}

// walker accumulates the walk under one mutex. The walk is latency-bound, so the
// lock is never the bottleneck and one lock is simpler than five atomics.
type walker struct {
	client   remote.Remote
	log      *logs.Logger
	workers  int
	manifest *json.Encoder

	mu          sync.Mutex
	dirs        int64
	files       int64
	bytes       int64
	requests    int64
	errors      int64
	depth       int
	widestDir   string
	widestCount int
	slowestDir  string
	slowest     time.Duration
}

// walk lists the tree level by level. Breadth-first bounds memory to one level's
// worth of directory names, where a depth-first recursion with a goroutine per
// directory would hold a hundred thousand stacks at once.
func (w *walker) walk(ctx context.Context, root string, maxDepth int) {
	level := []string{root}

	for depth := 1; len(level) > 0 && ctx.Err() == nil; depth++ {
		w.mu.Lock()
		w.depth = depth
		w.mu.Unlock()

		next := w.listLevel(ctx, level)
		if maxDepth > 0 && depth >= maxDepth {
			w.log.Infof("walk: stopping at -max-depth %d with %s directories unvisited", maxDepth, format.Comma(int64(len(next))))
			return
		}
		level = next
	}
}

// listLevel lists every directory of one level and returns the directories of the
// next.
func (w *walker) listLevel(ctx context.Context, dirs []string) []string {
	var (
		wg   sync.WaitGroup
		next []string
		mu   sync.Mutex
	)

	in := make(chan string)
	for i := 0; i < w.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for dir := range in {
				children := w.listDir(ctx, dir)
				mu.Lock()
				next = append(next, children...)
				mu.Unlock()
			}
		}()
	}

	for _, dir := range dirs {
		select {
		case in <- dir:
		case <-ctx.Done():
			close(in)
			wg.Wait()
			return nil
		}
	}
	close(in)
	wg.Wait()

	return next
}

// listDir lists one directory and folds it into the statistics. A failure is
// counted and logged rather than returned: one unreadable directory should not
// end a walk that has been running for ten minutes.
func (w *walker) listDir(ctx context.Context, dir string) []string {
	start := time.Now()
	entries, err := w.client.List(ctx, dir)
	took := time.Since(start)

	w.mu.Lock()
	defer w.mu.Unlock()

	w.requests++
	w.dirs++
	if took > w.slowest {
		w.slowest, w.slowestDir = took, dir
	}
	if len(entries) > w.widestCount {
		w.widestCount, w.widestDir = len(entries), dir
	}

	if err != nil {
		w.errors++
		if ctx.Err() == nil {
			w.log.Errorf("walk: %v", err)
		}
		return nil
	}

	var children []string
	for _, e := range entries {
		if e.IsDir {
			children = append(children, e.Path)
		} else {
			w.files++
			w.bytes += e.Size
		}
		if err := w.writeManifest(e); err != nil {
			w.log.Errorf("walk: %v", err)
		}
	}
	return children
}

func (w *walker) writeManifest(e remote.Entry) error {
	if w.manifest == nil {
		return nil
	}
	if err := w.manifest.Encode(e); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// reportProgress prints a line every interval until the returned function is
// called. A walk of the real collection runs for minutes; silence for that long
// is indistinguishable from a hang.
func (w *walker) reportProgress(ctx context.Context, interval time.Duration) func() {
	done := make(chan struct{})
	var once sync.Once

	go func() {
		tick := time.NewTicker(interval)
		defer tick.Stop()
		start := time.Now()

		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				w.mu.Lock()
				dirs, files, bytes, reqs, depth := w.dirs, w.files, w.bytes, w.requests, w.depth
				w.mu.Unlock()

				elapsed := time.Since(start)
				w.log.Infof("walk: %s dirs, %s files, %s, level %d, %.1f req/s",
					format.Comma(dirs), format.Comma(files), format.Bytes(bytes), depth, float64(reqs)/elapsed.Seconds())
			}
		}
	}()

	return func() { once.Do(func() { close(done) }) }
}

func (w *walker) summarize(root string, elapsed time.Duration, manifestPath string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	fmt.Printf("\nwalk of %q finished in %s\n", root, format.Duration(elapsed))
	fmt.Printf("  directories    %s\n", format.Comma(w.dirs))
	fmt.Printf("  files          %s\n", format.Comma(w.files))
	fmt.Printf("  bytes          %s\n", format.Bytes(w.bytes))
	fmt.Printf("  listings       %s (%.1f req/s across %d workers)\n", format.Comma(w.requests), float64(w.requests)/elapsed.Seconds(), w.workers)
	fmt.Printf("  deepest level  %d\n", w.depth)
	fmt.Printf("  widest dir     %q with %s entries\n", w.widestDir, format.Comma(int64(w.widestCount)))
	fmt.Printf("  slowest dir    %q at %s\n", w.slowestDir, format.Duration(w.slowest))
	fmt.Printf("  errors         %s\n", format.Comma(w.errors))
	// Only the real client counts these; the local stand-in reads a tree that was
	// never anybody's source.
	if counter, ok := w.client.(interface{ NonNFCNames() int64 }); ok {
		fmt.Printf("  non-NFC names  %s\n", format.Comma(counter.NonNFCNames()))
	}
	if manifestPath != "" {
		fmt.Printf("  manifest       %s\n", manifestPath)
	}
	fmt.Printf("\n=> one full scan costs %s; that is the floor for the scan schedule (DESIGN.md §6, §10.4)\n", format.Duration(elapsed))
}
