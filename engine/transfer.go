package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/store"
)

// maxRetryWait is how long a run will sit and wait for a backing-off job before
// leaving it to the next one. A short blip should heal inside the run that hit
// it; a publisher that is off for the night should not hold a run open.
const maxRetryWait = 2 * time.Minute

// maxConsecutiveFailures ends a run that is failing at everything. Grinding
// through thirty thousand files, each failing five times, tells nobody anything
// the first five did not.
const maxConsecutiveFailures = 5

// SyncResult is what one run of the queue did.
type SyncResult struct {
	PairID   int64         `json:"pairId"`
	PairName string        `json:"pair"`
	Files    int           `json:"files"`    // transferred
	Bytes    int64         `json:"bytes"`    // moved in this run, so a resumed file counts only its remainder
	InPlace  int           `json:"inPlace"`  // already correct on disk when the job ran
	Failed   int           `json:"failed"`   // out of attempts
	Retrying int           `json:"retrying"` // failed this time, queued for another go
	Pending  int64         `json:"pending"`  // still queued when the run ended
	Duration time.Duration `json:"duration"`
}

// Sync runs a pair's job queue until it is empty. Files go one at a time, as
// intended - the parallelism is inside a file, where several ranged streams fill
// a link that one TCP connection cannot (DESIGN.md §2.4).
func (e *Engine) Sync(ctx context.Context, pair store.Pair) (SyncResult, error) {
	started := time.Now()
	res := SyncResult{PairID: pair.ID, PairName: pair.Name}

	// Anything left running belonged to a process that is gone: a crash, a
	// restart, a self-update or a transfer window that closed.
	requeued, err := e.store.RequeueRunning(ctx, pair.ID)
	if err != nil {
		return res, err
	}
	if requeued > 0 {
		e.log.Infof("sync: requeued %d job(s) left in flight by an earlier run", requeued)
	}

	queue, err := e.store.Queue(ctx, pair.ID)
	if err != nil {
		return res, err
	}
	// Before a byte moves: a plan that cannot land is better refused than run
	// until the volume is full (DESIGN.md §2.6).
	if err := e.checkSpace(pair, queue.PendingBytes); err != nil {
		e.event(ctx, store.LevelError, store.KindSpaceLow, pair.ID,
			"sync of "+pair.Name+" refused: "+err.Error(), map[string]any{"need": queue.PendingBytes})
		return res, err
	}
	e.beginRun(pair, int(queue.Counts[store.JobPending]), queue.PendingBytes)
	defer e.endRun()

	e.event(ctx, store.LevelInfo, store.KindSyncStarted, pair.ID,
		fmt.Sprintf("sync of %q started: %s files, %s queued",
			pair.Name, format.Comma(queue.Counts[store.JobPending]), format.Bytes(queue.PendingBytes)),
		map[string]any{"files": queue.Counts[store.JobPending], "bytes": queue.PendingBytes})

	var (
		consecutive int
		noSpace     *SpaceError
	)
	for ctx.Err() == nil {
		job, ok, err := e.store.ClaimJob(ctx, pair.ID, time.Now())
		if err != nil {
			return res, err
		}
		if !ok {
			wait, ok := e.retryWait(ctx, pair.ID)
			if !ok {
				break
			}
			e.log.Infof("sync: waiting %s for a retry", format.Duration(wait))
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
			}
			break
		}

		moved, err := e.runJob(ctx, pair, job)
		switch {
		case err == nil:
			consecutive = 0
			if moved {
				res.Files++
			} else {
				res.InPlace++
			}
		case ctx.Err() != nil:
			// An interrupted transfer is not a failed one: the job goes back to
			// pending with its watermark intact and the next run continues it.
			if rerr := e.store.ReleaseJob(context.WithoutCancel(ctx), job.ID); rerr != nil {
				e.log.Errorf("sync: %v", rerr)
			}
		case errors.As(err, &noSpace):
			// Not this file's fault, and not something more attempts can fix. The
			// run ends with its retry budget untouched.
			if rerr := e.store.ReleaseJob(ctx, job.ID); rerr != nil {
				e.log.Errorf("sync: %v", rerr)
			}
			e.event(ctx, store.LevelError, store.KindSpaceLow, pair.ID,
				"sync of "+pair.Name+" stopped: "+err.Error(), map[string]any{"path": job.Path})
			res.Duration = time.Since(started)
			e.finishSync(ctx, pair, &res, err)
			return res, err
		default:
			consecutive++
			retrying, ferr := e.store.FailJob(ctx, job.ID, err, e.opts.MaxAttempts, e.opts.RetryBackoff)
			if ferr != nil {
				return res, ferr
			}
			if retrying {
				res.Retrying++
				e.log.Warnf("sync: %q failed (attempt %d/%d), will retry: %v", job.Path, job.Attempts, e.opts.MaxAttempts, err)
			} else {
				res.Failed++
				e.log.Errorf("sync: %q failed for good after %d attempts: %v", job.Path, job.Attempts, err)
				e.change(ctx, &store.Change{PairID: pair.ID, Path: job.Path, Op: store.ChangeFail, Error: err.Error()})
				e.event(ctx, store.LevelError, store.KindTransferFail, pair.ID,
					fmt.Sprintf("%s: %s gave up after %d attempts", pair.Name, job.Path, job.Attempts),
					map[string]any{"path": job.Path, "error": err.Error()})
			}
			if consecutive >= maxConsecutiveFailures {
				res.Duration = time.Since(started)
				e.finishSync(ctx, pair, &res, err)
				return res, fmt.Errorf("%d transfers failed in a row, stopping the run: %w", consecutive, err)
			}
		}
	}

	res.Duration = time.Since(started)
	e.finishSync(ctx, pair, &res, ctx.Err())
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	return res, nil
}

func (e *Engine) finishSync(ctx context.Context, pair store.Pair, res *SyncResult, cause error) {
	// The run may be ending because the context died; the record of why still has
	// to be written.
	ctx = context.WithoutCancel(ctx)
	res.Bytes = e.runBytes.Load()

	if q, err := e.store.Queue(ctx, pair.ID); err == nil {
		res.Pending = q.Counts[store.JobPending]
	}

	level, msg := store.LevelInfo, fmt.Sprintf("sync of %q finished: %s files, %s in %s",
		pair.Name, format.Comma(int64(res.Files)), format.Bytes(res.Bytes), format.Duration(res.Duration))
	if res.Failed > 0 {
		level = store.LevelWarn
		msg += fmt.Sprintf(", %d failed", res.Failed)
	}
	if cause != nil {
		level = store.LevelWarn
		msg += ", interrupted: " + cause.Error()
	}

	e.log.Infof("sync: %s", msg)
	e.event(ctx, level, store.KindSyncFinished, pair.ID, msg, map[string]any{
		"files": res.Files, "bytes": res.Bytes, "inPlace": res.InPlace,
		"failed": res.Failed, "retrying": res.Retrying, "pending": res.Pending,
		"seconds": res.Duration.Seconds(),
	})
}

// retryWait reports how long until the next backing-off job is due, and whether
// this run should wait for it at all.
func (e *Engine) retryWait(ctx context.Context, pairID int64) (time.Duration, bool) {
	q, err := e.store.Queue(ctx, pairID)
	if err != nil || q.NextAttempt == nil || q.Counts[store.JobPending] == 0 {
		return 0, false
	}
	wait := time.Until(*q.NextAttempt)
	if wait <= 0 || wait > maxRetryWait {
		return 0, false
	}
	return wait, true
}

// runJob transfers one file. moved is false when the file turned out to be
// correct on disk already, which is what a crash between the rename and the
// database write leaves behind.
func (e *Engine) runJob(ctx context.Context, pair store.Pair, job store.Job) (moved bool, err error) {
	dst := localPath(pair, job.Path)

	if e.matchesOnDisk(dst, job) {
		e.log.Debugf("sync: %q is already on disk, adopting it", job.Path)
		if err := e.recordFile(ctx, pair, job.Path, job.BytesTotal, job.MTime, "", store.FileAdopted); err != nil {
			return false, err
		}
		return false, e.store.SetJobState(ctx, job.ID, store.JobDone)
	}

	e.beginFile(job.Path, job.BytesTotal)
	defer e.endFile()

	part := partPath(pair, job.Path)
	if err := makeDir(pair, filepath.Dir(part)); err != nil {
		return false, fmt.Errorf("create the staging directory: %w", err)
	}
	if err := makeDir(pair, filepath.Dir(dst)); err != nil {
		return false, fmt.Errorf("create %q: %w", filepath.Dir(dst), err)
	}

	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return false, fmt.Errorf("open the partial file: %w", err)
	}
	closed := false
	closeFile := func() error {
		if closed {
			return nil
		}
		closed = true
		return f.Close()
	}
	defer func() { _ = closeFile() }()

	from, err := resumeOffset(f, job)
	if err != nil {
		return false, err
	}
	// Again per file: the run may have started days ago, and something else on
	// the NAS can fill the volume while it works.
	if err := e.checkSpace(pair, job.BytesTotal-from); err != nil {
		return false, err
	}
	if from > 0 {
		e.log.Infof("sync: %q resuming at %s of %s", job.Path, format.Bytes(from), format.Bytes(job.BytesTotal))
	}

	watermark, err := e.download(ctx, f, pair, job, from)
	if err != nil {
		// Whatever arrived before the failure stays: the watermark is the whole
		// reason an interrupted 40 GB file does not start again from zero.
		if watermark != job.BytesDone {
			if perr := e.store.JobProgress(context.WithoutCancel(ctx), job.ID, watermark); perr != nil {
				e.log.Errorf("sync: %v", perr)
			}
		}
		return false, err
	}

	if err := e.store.SetJobState(ctx, job.ID, store.JobVerifying); err != nil {
		return false, err
	}

	// A remote file that shrank leaves the tail of the longer previous attempt
	// behind, and the size check below would pass on a file with the wrong end.
	if err := f.Truncate(job.BytesTotal); err != nil {
		return false, fmt.Errorf("truncate the partial file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return false, fmt.Errorf("flush the partial file: %w", err)
	}

	st, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("stat the partial file: %w", err)
	}
	// No source-side checksum exists over SMB, and none is needed: WireGuard
	// authenticates every packet. Truncation is the failure mode that remains, and
	// this is what catches it (DESIGN.md §2.3).
	if st.Size() != job.BytesTotal {
		return false, fmt.Errorf("short transfer: %d bytes of %d", st.Size(), job.BytesTotal)
	}

	var hash string
	if e.opts.HashAfterCopy {
		if hash, err = hashFile(f); err != nil {
			return false, err
		}
	}
	if err := closeFile(); err != nil {
		return false, fmt.Errorf("close the partial file: %w", err)
	}

	if err := settleFile(pair, part); err != nil {
		return false, err
	}

	// Before the rename, not after: the local mtime has to be the publisher's from
	// the moment the file appears, or the next scan sees a change and the file
	// transfers again, forever (DESIGN.md §2.3).
	if err := os.Chtimes(part, job.MTime, job.MTime); err != nil {
		return false, fmt.Errorf("set the timestamp: %w", err)
	}
	if err := os.Rename(part, dst); err != nil {
		return false, fmt.Errorf("move the finished file into place: %w", err)
	}

	if err := e.recordFile(ctx, pair, job.Path, job.BytesTotal, job.MTime, hash, store.FileSynced); err != nil {
		return false, err
	}
	if err := e.store.SetJobState(ctx, job.ID, store.JobDone); err != nil {
		return false, err
	}

	e.log.Debugf("sync: %q done, %s", job.Path, format.Bytes(job.BytesTotal))
	return true, nil
}

// recordFile writes local truth and the per-file history in one place, so a
// transfer, an in-place adoption and the database's own gate cannot drift apart.
func (e *Engine) recordFile(ctx context.Context, pair store.Pair, relpath string, size int64, mtime time.Time, hash, state string) error {
	var (
		op        = store.ChangeAdd
		before    *int64
		unchanged bool
	)
	old, have, err := e.store.FileAt(ctx, pair.ID, relpath)
	if err != nil {
		return err
	}
	if have {
		op = store.ChangeReplace
		oldSize := old.Size
		before = &oldSize

		// The row already describes this exact file, so the queue was out of date
		// with local truth rather than the file being out of date with the
		// publisher. A file this process transferred keeps saying 'synced': that
		// provenance is the whole value of the state column to a later scrub.
		unchanged = old.Size == size && within(old.MTime, mtime, e.opts.MTimeTolerance)
		if unchanged {
			state = old.State
		}
	}

	if err := e.store.PutFile(ctx, store.File{
		PairID: pair.ID, Path: relpath, Size: size,
		MTime: mtime, Hash: hash, State: state, VerifiedAt: time.Now(),
	}); err != nil {
		return err
	}
	if unchanged {
		return nil // nothing moved, so the history has nothing to record
	}

	after := size
	e.change(ctx, &store.Change{
		PairID: pair.ID, Path: relpath, Op: op,
		SizeBefore: before, SizeAfter: &after,
	})
	return nil
}

// matchesOnDisk reports whether the destination already is what the job would
// produce. It costs one stat and it is what makes a crash between the rename and
// the database write cheap rather than a re-transfer.
func (e *Engine) matchesOnDisk(dst string, job store.Job) bool {
	st, err := os.Stat(dst)
	if err != nil || st.IsDir() || st.Size() != job.BytesTotal {
		return false
	}
	return within(st.ModTime(), job.MTime, e.opts.MTimeTolerance)
}

// resumeOffset decides where to carry on from. The watermark is only trusted as
// far as the partial file actually reaches: it may have been cleaned out from
// under us, and writing at an offset past the end would leave a hole.
func resumeOffset(f *os.File, job store.Job) (int64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat the partial file: %w", err)
	}

	from := job.BytesDone
	if from > st.Size() {
		from = st.Size()
	}
	if from > job.BytesTotal || from < 0 {
		from = 0
	}
	if from == 0 && st.Size() > 0 {
		// Starting over means the bytes that are there belong to a version that no
		// longer exists.
		if err := f.Truncate(0); err != nil {
			return 0, fmt.Errorf("truncate the partial file: %w", err)
		}
	}
	return from, nil
}

// chunk is one ranged request.
type chunk struct {
	off    int64
	length int64
}

// download fetches [from, size) in rounds of at most Chunks streams and returns
// how far the file is known good.
//
// The watermark only ever advances over a completed round, so it is exact: a
// crash costs at most the round in flight. A straggler in a round holds up the
// next one, and that is the trade - a watermark that has to describe holes is
// not worth the last few percent of throughput.
func (e *Engine) download(ctx context.Context, f *os.File, pair store.Pair, job store.Job, from int64) (int64, error) {
	watermark := from
	e.fileDone.Store(from)
	src := remotePath(pair, job.Path)

	for watermark < job.BytesTotal {
		if err := ctx.Err(); err != nil {
			return watermark, err
		}

		var round []chunk
		for off := watermark; off < job.BytesTotal && len(round) < e.opts.Chunks; off += e.opts.ChunkSize {
			length := e.opts.ChunkSize
			if rest := job.BytesTotal - off; rest < length {
				length = rest
			}
			round = append(round, chunk{off: off, length: length})
		}

		if err := e.fetchRound(ctx, f, src, round); err != nil {
			return watermark, err
		}

		last := round[len(round)-1]
		watermark = last.off + last.length
		if err := e.store.JobProgress(ctx, job.ID, watermark); err != nil {
			return watermark, err
		}
	}
	return watermark, nil
}

// fetchRound runs one round of chunks in parallel. All of them have to succeed:
// a round with a hole in it cannot advance the watermark.
func (e *Engine) fetchRound(ctx context.Context, f *os.File, src string, round []chunk) error {
	if len(round) == 1 {
		return e.fetchChunk(ctx, f, src, round[0])
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	for _, c := range round {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.fetchChunk(ctx, f, src, c); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
				cancel()
			}
		}()
	}
	wg.Wait()
	return first
}

func (e *Engine) fetchChunk(ctx context.Context, f *os.File, src string, c chunk) error {
	body, _, err := e.remote.OpenRange(ctx, src, c.off, c.length)
	if err != nil {
		return err
	}
	defer body.Close()

	// WriteAt through an offset writer: the chunks of a round share the file and
	// each one owns its own region of it.
	n, err := io.Copy(io.NewOffsetWriter(f, c.off), &countingReader{r: body, ctx: ctx, engine: e})
	if err != nil {
		return fmt.Errorf("transfer bytes %d-%d: %w", c.off, c.off+c.length-1, err)
	}
	if n != c.length {
		return fmt.Errorf("transfer bytes %d-%d: got %d of %d bytes", c.off, c.off+c.length-1, n, c.length)
	}
	return nil
}

// countingReader feeds the live progress. The counters are atomics rather than
// anything locked: every chunk stream touches them on every read.
type countingReader struct {
	r      io.Reader
	ctx    context.Context
	engine *Engine
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.engine.runBytes.Add(int64(n))
		c.engine.fileDone.Add(int64(n))
		// After the read rather than before it: what shapes the rate is the pause
		// before the next one, and the connection's own window absorbs the wait.
		// The limiter is shared, so the cap is the sum over every stream of the
		// file, not one cap each.
		if werr := c.engine.opts.Limiter.wait(c.ctx, n); werr != nil && err == nil {
			err = werr
		}
	}
	return n, err
}

// partPath is where a file is staged while it downloads. The name is a hash of
// the relative path rather than something random: a random name cannot be found
// again after a restart, which throws away the resume (DESIGN.md §2.4).
func partPath(pair store.Pair, relpath string) string {
	sum := sha256.Sum256([]byte(relpath))
	return filepath.Join(pair.LocalPath, PartDir, hex.EncodeToString(sum[:])+".part")
}

func hashFile(f *os.File) (string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind for hashing: %w", err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
