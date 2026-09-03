package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/store"
)

// ReapResult is what one deletion phase did, or why it did nothing.
type ReapResult struct {
	PairID   int64  `json:"pairId"`
	PairName string `json:"pair"`

	Deleted int   `json:"deleted"` // quarantined, not unlinked
	Bytes   int64 `json:"bytes"`
	Missing int   `json:"missing"` // gone from disk already; only the row went
	Failed  int   `json:"failed"`

	// Blocked is the approval request a guard raised. It is set instead of
	// Deleted: a run that trips a threshold deletes nothing at all, rather than
	// deleting up to the threshold and stopping (DESIGN.md §2.5).
	Blocked *store.DeleteApproval `json:"blocked,omitempty"`
	// Approved is the request this run spent, when one had been granted.
	Approved *store.DeleteApproval `json:"approved,omitempty"`
	// Skipped says why nothing happened when nothing was wrong.
	Skipped string `json:"skipped,omitempty"`

	Pruned      []TrashDay    `json:"pruned,omitempty"` // quarantine days the retention swept
	PrunedBytes int64         `json:"prunedBytes,omitempty"`
	Duration    time.Duration `json:"duration"`
}

// Reap is the deletion half of a mirror pair: the files the publisher no longer
// has are moved into the quarantine here, and the quarantine of previous runs is
// emptied once its retention has passed.
//
// It runs after a sync rather than inside one. The tree only ever shrinks once
// the additions have landed, so a run that could not reach the publisher, or
// could not write what it fetched, never gets as far as removing anything
// (DESIGN.md §2.5).
func (e *Engine) Reap(ctx context.Context, pair store.Pair) (res ReapResult, err error) {
	started := time.Now()
	res = ReapResult{PairID: pair.ID, PairName: pair.Name}
	// However this ends - refused, held, or done - it took as long as it took.
	defer func() { res.Duration = time.Since(started) }()

	// Before anything else, and for every pair: a pair switched back to additive
	// still has yesterday's quarantine to let go of.
	e.pruneTrash(ctx, pair, &res)

	if pair.Mode != store.ModeMirror {
		res.Skipped = "the pair is additive: nothing here is ever deleted"
		return res, nil
	}
	// A jcc pair carries hard exclusions that do not exist yet, and this side's own
	// ClipCornUserData.db is exactly the kind of file they exist to protect.
	if err := Transferable(pair); err != nil {
		return res, err
	}

	scan, ok, err := e.store.LastCompletedScan(ctx, pair.ID)
	if err != nil {
		return res, err
	}
	if !ok {
		return res, fmt.Errorf("pair %q: %w", pair.Name, errNoManifest)
	}

	// The non-empty assertion (DESIGN.md §2.5). A share unmounted on the
	// publisher's side lists as an empty directory, and the manifest that walk left
	// behind describes a collection that was deleted in its entirety. The scanner
	// refuses such a walk; this is the same refusal on the acting side, where the
	// cost of being wrong is the collection rather than a re-scan.
	remoteFiles, _, err := e.store.ManifestStats(ctx, pair.ID)
	if err != nil {
		return res, err
	}
	if remoteFiles == 0 {
		return res, fmt.Errorf("pair %q: the manifest holds no files at all, which is a publisher that is not there rather than one who deleted everything", pair.Name)
	}

	// Delete after, never during. Anything still queued or failed is an addition
	// that has not landed, and a tree that shrinks before it has grown is a tree
	// that is briefly missing both.
	queue, err := e.store.Queue(ctx, pair.ID)
	if err != nil {
		return res, err
	}
	if open := queue.Counts[store.JobPending] + queue.Counts[store.JobRunning] + queue.Counts[store.JobVerifying]; open > 0 {
		res.Skipped = fmt.Sprintf("%s transfer(s) are still queued: the tree only shrinks once the additions have landed", format.Comma(open))
		return res, nil
	}
	if failed := queue.Counts[store.JobFailed]; failed > 0 {
		res.Skipped = fmt.Sprintf("%s transfer(s) failed for good: deletion waits until someone has looked at them", format.Comma(failed))
		return res, nil
	}

	files, bytes, err := e.vanished(ctx, pair, nil)
	if err != nil {
		return res, err
	}
	if files == 0 {
		res.Skipped = "the publisher still has everything we hold"
		return res, nil
	}

	localFiles, _, err := e.store.FileStats(ctx, pair.ID)
	if err != nil {
		return res, err
	}

	if reason := guardTrip(pair, e.opts.DeletePercent, files, localFiles); reason != "" {
		decided, err := e.hold(ctx, pair, scan.ID, files, bytes, reason, &res)
		if err != nil || !decided {
			return res, err
		}
	}

	res.Deleted, res.Bytes, res.Missing, res.Failed, err = e.quarantine(ctx, pair, files)
	if err != nil {
		return res, err
	}

	if res.Approved != nil {
		if err := e.store.UseApproval(ctx, res.Approved.ID); err != nil {
			return res, err
		}
	}

	msg := fmt.Sprintf("%s file(s) of %q quarantined, %s, after %s",
		format.Comma(int64(res.Deleted)), pair.Name, format.Bytes(res.Bytes), format.Duration(time.Since(started)))
	level := store.LevelInfo
	if res.Failed > 0 {
		level = store.LevelWarn
		msg += fmt.Sprintf("; %s could not be moved", format.Comma(int64(res.Failed)))
	}
	e.log.Infof("delete: %s", msg)
	e.event(ctx, level, store.KindDeleted, pair.ID, msg, map[string]any{
		"deleted": res.Deleted, "bytes": res.Bytes, "missing": res.Missing, "failed": res.Failed,
		"retention": e.opts.TrashRetention.String(), "seconds": time.Since(started).Seconds(),
	})
	return res, nil
}

// hold is what happens to a deletion the guards stopped: it goes ahead only on an
// approval that still describes it, stays quiet on a refusal that does, and
// otherwise becomes the request a person answers.
func (e *Engine) hold(ctx context.Context, pair store.Pair, scanID, files, bytes int64, reason string, res *ReapResult) (bool, error) {
	last, ok, err := e.store.LatestApproval(ctx, pair.ID)
	if err != nil {
		return false, err
	}

	switch {
	case ok && last.Covers(scanID, files):
		res.Approved = &last
		e.log.Infof("delete: %q was approved by %s for up to %s file(s); going ahead",
			pair.Name, last.DecidedBy, format.Comma(last.Files))
		return true, nil

	case ok && last.Refuses(scanID, files):
		// Answered, and the answer was no. Asking again every hour until the next
		// walk would make the one event that matters here unreadable.
		res.Skipped = fmt.Sprintf("%s file(s) would go, and %s refused that %s; the next walk asks again",
			format.Comma(files), last.DecidedBy, decidedWhen(last))
		e.log.Infof("delete: %s", res.Skipped)
		return false, nil
	}

	blocked, err := e.store.RequestDeleteApproval(ctx, store.DeleteApproval{
		PairID: pair.ID, ScanID: scanID, Files: files, Bytes: bytes, Reason: reason,
	})
	if err != nil {
		return false, err
	}
	res.Blocked = &blocked

	// Only a request nobody has seen yet is worth an event. A scheduled sync comes
	// back to the same held deletion every hour, and one warning per hour about a
	// thing that has not changed is a log nobody reads (DESIGN.md §4).
	if ok && last.ID == blocked.ID && last.ScanID == blocked.ScanID && last.Files == blocked.Files {
		e.log.Infof("delete: %q is still waiting for approval of %s file(s)", pair.Name, format.Comma(files))
		return false, nil
	}

	msg := fmt.Sprintf("deletion of %s file(s) from %q is waiting for approval: %s",
		format.Comma(files), pair.Name, reason)
	e.log.Warnf("delete: %s", msg)
	e.event(ctx, store.LevelWarn, store.KindDeleteBlocked, pair.ID, msg, map[string]any{
		"files": files, "bytes": bytes, "reason": reason, "scanId": scanID, "approvalId": blocked.ID,
	})
	return false, nil
}

func decidedWhen(a store.DeleteApproval) string {
	if a.DecidedAt == nil {
		return "already"
	}
	return "on " + a.DecidedAt.Format("2006-01-02 15:04")
}

// guardTrip reports why a deletion needs a person to look at it first, or "" when
// it is small enough to run unattended. Both limits are off at zero, which is a
// setting an operator can make and not one they can arrive at by accident: the
// stored defaults are 100 files and 10 percent (DESIGN.md §2.5).
func guardTrip(pair store.Pair, percent int, files, localFiles int64) string {
	if pair.DeleteGuard > 0 && files > int64(pair.DeleteGuard) {
		return fmt.Sprintf("%s files would be deleted and this pair's guard is %s",
			format.Comma(files), format.Comma(int64(pair.DeleteGuard)))
	}
	if percent > 0 && localFiles > 0 && files*100 > int64(percent)*localFiles {
		return fmt.Sprintf("%s of the %s files here would be deleted, over the %d%% threshold",
			format.Comma(files), format.Comma(localFiles), percent)
	}
	return ""
}

// vanishedPage is how many rows the deletion pass reads at a time. Flat in
// memory over a collection of any size, and one query per thousand files.
const vanishedPage = 1000

// vanished counts what we hold and the publisher no longer has, and calls fn for
// each of them when there is one. It runs twice per deletion - once to count for
// the guards, once to act - because the alternative is holding six figures of
// paths in memory to save an indexed join.
func (e *Engine) vanished(ctx context.Context, pair store.Pair, fn func(relpath string, size int64) error) (files, bytes int64, err error) {
	f := newFilter(pair)

	for after := ""; ; {
		page, err := e.store.VanishedPage(ctx, pair.ID, after, vanishedPage)
		if err != nil {
			return files, bytes, err
		}
		if len(page) == 0 {
			return files, bytes, nil
		}

		for _, v := range page {
			after = v.Path
			// A file the globs no longer cover is not a deletion: excluding a subtree
			// stops mirroring it, it does not empty it.
			if !f.allows(v.Path) {
				continue
			}
			files++
			bytes += v.Size
			if fn == nil {
				continue
			}
			if err := fn(v.Path, v.Size); err != nil {
				return files, bytes, err
			}
		}
	}
}

// quarantine moves the vanished files into the day's trash directory and drops
// their row of local truth. A file that will not move is counted and stepped
// over: one directory with the wrong permissions must not stop the other nine
// thousand deletions.
func (e *Engine) quarantine(ctx context.Context, pair store.Pair, expect int64) (deleted int, bytes int64, missing, failed int, err error) {
	dir := trashDirFor(pair, time.Now())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("create the quarantine directory: %w", err)
	}
	e.log.Infof("delete: quarantining %s file(s) of %q into %s", format.Comma(expect), pair.Name, dir)

	_, _, err = e.vanished(ctx, pair, func(relpath string, size int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		src := localPath(pair, relpath)
		switch _, statErr := os.Lstat(src); {
		case errors.Is(statErr, fs.ErrNotExist):
			// The row outlived the file: someone removed it on this side. Nothing to
			// quarantine, but local truth still has to stop claiming we hold it.
			missing++

		case statErr != nil:
			e.log.Errorf("delete: %q: %v", relpath, statErr)
			failed++
			return nil

		default:
			dst, err := trashPath(dir, relpath)
			if err != nil {
				e.log.Errorf("delete: %v", err)
				failed++
				return nil
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				e.log.Errorf("delete: %q: %v", relpath, err)
				failed++
				return nil
			}
			if err := os.Rename(src, dst); err != nil {
				e.log.Errorf("delete: %q: %v", relpath, err)
				failed++
				return nil
			}
			pruneEmptyDirs(pair, relpath)
			deleted++
			bytes += size
		}

		// Local truth stops claiming the file either way: the row is what a later
		// walk would read as "we hold this".
		if err := e.store.ForgetFile(ctx, pair.ID, relpath); err != nil {
			return err
		}
		before := size
		e.change(ctx, &store.Change{PairID: pair.ID, Path: relpath, Op: store.ChangeDelete, SizeBefore: &before})
		return nil
	})
	return deleted, bytes, missing, failed, err
}

// pruneTrash empties the quarantine of everything past its retention. A failure
// here is logged and swallowed: a deletion that cannot let go of last week's
// files is still a deletion that can quarantine this week's.
func (e *Engine) pruneTrash(ctx context.Context, pair store.Pair, res *ReapResult) {
	removed, err := PruneTrash(pair, e.opts.TrashRetention, time.Now())
	if err != nil {
		e.log.Errorf("trash: %v", err)
	}
	if len(removed) == 0 {
		return
	}

	var (
		files int
		bytes int64
		days  []string
	)
	for _, d := range removed {
		files += d.Files
		bytes += d.Bytes
		days = append(days, d.Day)
	}
	res.Pruned, res.PrunedBytes = removed, bytes

	msg := fmt.Sprintf("%s quarantined file(s) of %q removed for good, %s, from %s",
		format.Comma(int64(files)), pair.Name, format.Bytes(bytes), strings.Join(days, ", "))
	e.log.Infof("trash: %s", msg)
	e.event(ctx, store.LevelInfo, store.KindTrashPruned, pair.ID, msg, map[string]any{
		"files": files, "bytes": bytes, "days": days, "retention": e.opts.TrashRetention.String(),
	})
}
