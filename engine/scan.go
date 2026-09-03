package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/remote"
	"blackforestbytes.com/jcc-mirror/store"
)

// listAttempts is how often one directory is retried before the scan gives up.
// Giving up is cheap - the walk resumes where it stopped - and carrying on with
// a hole in the manifest is not: the sweep would read that hole as "the
// publisher deleted all of this".
const listAttempts = 3

// scanProgressInterval is how often a running walk says where it is. A scan of
// the real collection runs for minutes, and silence for that long is
// indistinguishable from a hang.
const scanProgressInterval = 15 * time.Second

// ScanResult is what one walk did.
type ScanResult struct {
	Scan     store.Scan    `json:"scan"`
	Resumed  bool          `json:"resumed"`
	Swept    int64         `json:"swept"` // manifest rows the walk no longer found
	Duration time.Duration `json:"duration"`
}

// Scan walks the pair's remote root into the manifest, one PROPFIND per
// directory, and sweeps what it did not find. With resume it continues an
// interrupted walk instead of starting over (DESIGN.md §2.3).
func (e *Engine) Scan(ctx context.Context, pair store.Pair, resume bool) (ScanResult, error) {
	started := time.Now()

	sc, resumed, err := e.store.BeginScan(ctx, pair.ID, resume)
	if err != nil {
		return ScanResult{}, err
	}

	verb := "started"
	if resumed {
		verb = "resumed"
	}
	e.log.Infof("scan: %s on %q (%s)", verb, pair.Name, remoteLabel(pair))
	e.event(ctx, store.LevelInfo, store.KindScanStarted, pair.ID,
		fmt.Sprintf("scan %s for %q", verb, pair.Name), map[string]any{"scanId": sc.ID, "resumed": resumed})

	w := &walk{engine: e, pair: pair, scanID: sc.ID, filter: newFilter(pair)}
	stop := w.report()
	walkErr := w.run(ctx)
	stop()

	// Whatever stopped the walk, the bookkeeping still has to be written - and it
	// is usually the context dying that stopped it.
	ctx = context.WithoutCancel(ctx)

	// The counters come from the scan row rather than this run's: a resumed walk
	// finished what an earlier one started, and the assertion below has to see
	// both halves.
	counted, err := e.store.ScanByID(ctx, sc.ID)
	if err != nil {
		return ScanResult{}, err
	}

	state, msg := store.ScanDone, ""
	switch {
	case walkErr != nil:
		// Not a failure: a walk that stopped early is picked up where it left off.
		// The alternative is throwing away ten minutes of PROPFINDs because a
		// transfer window closed.
		state, msg = store.ScanInterrupted, walkErr.Error()

	// The non-empty assertion (DESIGN.md §2.5). A share that has been unmounted on
	// the publisher's side answers every PROPFIND with an empty directory, and
	// sweeping on that would erase the only record of what he has.
	//
	// It catches a share that was already gone when the walk started, not one that
	// goes away halfway through: that walk still finds files, completes, and
	// sweeps everything it did not re-see. The guard for the partial case is the
	// deletion threshold of §2.5, which stops a sweep that large from being acted
	// on until a person has looked at it.
	case counted.Files == 0:
		state = store.ScanFailed
		msg = "the walk found no files at all - the remote root is empty or the share is not mounted, which is an error rather than a deletion"
	}

	swept, err := e.store.FinishScan(ctx, sc.ID, state, msg)
	if err != nil {
		return ScanResult{}, err
	}

	final, err := e.store.ScanByID(ctx, sc.ID)
	if err != nil {
		return ScanResult{}, err
	}
	res := ScanResult{Scan: final, Resumed: resumed, Swept: swept, Duration: time.Since(started)}
	data := map[string]any{
		"scanId": sc.ID, "dirs": final.Dirs, "files": final.Files, "bytes": final.Bytes,
		"swept": swept, "excluded": w.skipped.Load(), "seconds": res.Duration.Seconds(),
	}

	switch state {
	case store.ScanInterrupted:
		e.log.Warnf("scan: %q interrupted after %s dirs, resumable: %v", pair.Name, format.Comma(final.Dirs), walkErr)
		e.event(ctx, store.LevelWarn, store.KindScanFinished, pair.ID,
			fmt.Sprintf("scan of %q interrupted and left resumable: %s", pair.Name, msg), data)
		return res, walkErr

	case store.ScanFailed:
		e.log.Errorf("scan: %q failed: %s", pair.Name, msg)
		e.event(ctx, store.LevelError, store.KindScanFinished, pair.ID,
			"scan of "+pair.Name+" failed: "+msg, data)
		return res, fmt.Errorf("scan of %q: %s", pair.Name, msg)
	}

	e.log.Infof("scan: %q done in %s - %s dirs, %s files, %s, %s swept",
		pair.Name, format.Duration(res.Duration), format.Comma(final.Dirs), format.Comma(final.Files),
		format.Bytes(final.Bytes), format.Comma(swept))
	e.event(ctx, store.LevelInfo, store.KindScanFinished, pair.ID,
		fmt.Sprintf("scan of %q found %s files in %s", pair.Name, format.Comma(final.Files), format.Duration(res.Duration)),
		data)
	return res, nil
}

// walk is one run of the breadth-first listing. The frontier is the manifest
// itself - directories of this scan that are not listed yet - so the walk holds
// no state a crash could lose.
type walk struct {
	engine *Engine
	pair   store.Pair
	scanID int64
	filter filter

	dirs    atomic.Int64
	files   atomic.Int64
	bytes   atomic.Int64
	skipped atomic.Int64
}

func (w *walk) run(ctx context.Context) error {
	workers := w.engine.opts.ScanWorkers
	batch := workers * 8
	if batch < 32 {
		batch = 32
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		dirs, err := w.engine.store.PendingDirs(ctx, w.pair.ID, w.scanID, batch)
		if err != nil {
			return err
		}
		if len(dirs) == 0 {
			return nil
		}
		if err := w.listBatch(ctx, dirs, workers); err != nil {
			return err
		}
	}
}

// listBatch lists one level's worth of directories with a bounded number of
// requests in flight. The first failure cancels the rest: an incomplete manifest
// is the one thing the walk must never hand on as complete.
func (w *walk) listBatch(ctx context.Context, dirs []string, workers int) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)

	in := make(chan string)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for dir := range in {
				if err := w.listDir(ctx, dir); err != nil {
					mu.Lock()
					if first == nil {
						first = err
					}
					mu.Unlock()
					cancel()
					return
				}
			}
		}()
	}

	for _, dir := range dirs {
		select {
		case in <- dir:
		case <-ctx.Done():
		}
	}
	close(in)
	wg.Wait()

	if first != nil {
		return first
	}
	return ctx.Err()
}

// listDir lists one directory and writes its children. Both happen before the
// directory is marked listed, so an interrupted scan repeats at most one
// directory rather than skipping one.
func (w *walk) listDir(ctx context.Context, dir string) error {
	entries, err := w.list(ctx, dir)
	if err != nil {
		return err
	}

	out := make([]store.ManifestEntry, 0, len(entries))
	var files, bytes, skipped int64

	for _, e := range entries {
		rel, ok := pairRel(w.pair, e.Path)
		if !ok {
			// A server that answers with an href outside the tree it was asked about
			// is confused, or the base URL is wrong; either way it is not ours to
			// store under this pair.
			w.engine.log.Warnf("scan: %q lies outside the pair root, skipping", e.Path)
			continue
		}
		if e.IsDir {
			if !w.filter.allowsDir(rel) {
				skipped++
				continue
			}
		} else if !w.filter.allows(rel) {
			skipped++
			continue
		}

		out = append(out, store.ManifestEntry{Path: rel, Size: e.Size, MTime: e.MTime, IsDir: e.IsDir})
		if !e.IsDir {
			files++
			bytes += e.Size
		}
	}

	if err := w.engine.store.PutListing(ctx, w.pair.ID, w.scanID, dir, out); err != nil {
		return err
	}

	w.dirs.Add(1)
	w.files.Add(files)
	w.bytes.Add(bytes)
	w.skipped.Add(skipped)
	return nil
}

// list retries a directory a few times before giving up on the whole scan. DSM
// drops the odd request under load, and one of those should not cost a walk that
// has been running for ten minutes - but a directory that keeps failing has to
// stop the scan rather than leave a hole in it.
func (w *walk) list(ctx context.Context, dir string) ([]remote.Entry, error) {
	var err error
	for attempt := 1; attempt <= listAttempts; attempt++ {
		var entries []remote.Entry
		entries, err = w.engine.remote.List(ctx, remotePath(w.pair, dir))
		if err == nil {
			return entries, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt < listAttempts {
			w.engine.log.Debugf("scan: %q attempt %d: %v", dir, attempt, err)
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return nil, fmt.Errorf("list %q after %d attempts: %w", dir, listAttempts, err)
}

func (w *walk) report() func() {
	done := make(chan struct{})
	var once sync.Once

	go func() {
		tick := time.NewTicker(scanProgressInterval)
		defer tick.Stop()
		start := time.Now()

		for {
			select {
			case <-done:
				return
			case <-tick.C:
				w.engine.log.Infof("scan: %s dirs, %s files, %s, %.1f dirs/s",
					format.Comma(w.dirs.Load()), format.Comma(w.files.Load()), format.Bytes(w.bytes.Load()),
					float64(w.dirs.Load())/time.Since(start).Seconds())
			}
		}
	}()

	return func() { once.Do(func() { close(done) }) }
}

// pairRel turns a path the remote reported - which is relative to the WebDAV
// base URL - into one relative to the pair's remote root.
func pairRel(p store.Pair, remotePath string) (string, bool) {
	if p.RemotePath == "" {
		return remotePath, true
	}
	if remotePath == p.RemotePath {
		return "", true
	}
	if rest, ok := strings.CutPrefix(remotePath, p.RemotePath+"/"); ok {
		return rest, true
	}
	return "", false
}

func remoteLabel(p store.Pair) string {
	if p.RemotePath == "" {
		return "the remote root"
	}
	return p.RemotePath
}
