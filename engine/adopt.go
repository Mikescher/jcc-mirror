package engine

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/text/unicode/norm"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/store"
)

// adoptBatch is how many rows go into one transaction. The bootstrap adopts tens
// of thousands of files and a transaction per row would spend the whole run in
// fsync.
const adoptBatch = 500

// AdoptResult is what an adoption found. Every number is here so the answer can
// be checked by eye before the first sync runs against it (DESIGN.md §2.4).
type AdoptResult struct {
	PairID   int64  `json:"pairId"`
	PairName string `json:"pair"`

	Matched      int   `json:"matched"` // adopted: same size as the publisher's
	MatchedBytes int64 `json:"matchedBytes"`
	SizeMismatch int   `json:"sizeMismatch"` // here and there, different size - they will transfer
	Extra        int   `json:"extra"`        // here, not on the publisher
	ExtraBytes   int64 `json:"extraBytes"`
	Excluded     int   `json:"excluded"`     // dropped by the pair's globs
	AlreadyKnown int   `json:"alreadyKnown"` // already local truth, left alone
	Missing      int   `json:"missing"`      // on the publisher, not here - they will transfer

	RemoteFiles int64         `json:"remoteFiles"`
	Duration    time.Duration `json:"duration"`
}

// Adopt matches the local tree against the last remote walk **on size alone** and
// records what agrees as local truth. It is what turns the ~30 TB USB bootstrap
// into a mirror that is already up to date, and without it the first sync would
// transfer all of it again.
//
// Mtimes are deliberately not compared: a USB copy that did not preserve them
// would otherwise make every file look changed. The publisher's mtime is what
// gets recorded, so the next diff agrees with the manifest (DESIGN.md §2.4).
func (e *Engine) Adopt(ctx context.Context, pair store.Pair) (res AdoptResult, err error) {
	started := time.Now()
	res = AdoptResult{PairID: pair.ID, PairName: pair.Name}
	// Stamped on every path out, not just the one that finishes: an adoption that
	// stopped early still took the time it took, and that is worth reporting.
	defer func() { res.Duration = time.Since(started) }()

	if _, ok, err := e.store.LastCompletedScan(ctx, pair.ID); err != nil {
		return res, err
	} else if !ok {
		return res, fmt.Errorf("pair %q: %w", pair.Name, errNoManifest)
	}

	f := newFilter(pair, e.opts.JCC)
	if err = e.store.RemoteFiles(ctx, pair.ID, func(entry store.ManifestEntry) error {
		if f.allows(entry.Path) {
			res.RemoteFiles++
		}
		return nil
	}); err != nil {
		return res, err
	}

	batch := make([]store.File, 0, adoptBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := e.store.PutFiles(ctx, batch); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}

	root := pair.LocalPath
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		rel = norm.NFC.String(filepath.ToSlash(rel))

		if d.IsDir() {
			if rel == PartDir {
				return fs.SkipDir // staging, not content
			}
			if rel != "." && !f.allowsDir(rel) {
				return fs.SkipDir
			}
			return nil
		}
		// Only ordinary files are content. A symlink, a socket or a device node in
		// a media tree is not something the mirror has an opinion about.
		if !d.Type().IsRegular() {
			return nil
		}
		if !f.allows(rel) {
			res.Excluded++
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		entry, ok, err := e.store.ManifestEntryAt(ctx, pair.ID, rel)
		if err != nil {
			return err
		}
		switch {
		case !ok || entry.IsDir:
			res.Extra++
			res.ExtraBytes += info.Size()
			return nil
		case entry.Size != info.Size():
			res.SizeMismatch++
			return nil
		}

		res.Matched++
		res.MatchedBytes += entry.Size

		// A row that already agrees is left alone: overwriting it would relabel a
		// file this process actually transferred as one it only ever measured.
		if old, ok, err := e.store.FileAt(ctx, pair.ID, rel); err != nil {
			return err
		} else if ok && old.Size == entry.Size && within(old.MTime, entry.MTime, e.opts.MTimeTolerance) {
			res.AlreadyKnown++
			return nil
		}

		batch = append(batch, store.File{
			PairID: pair.ID, Path: rel, Size: entry.Size, MTime: entry.MTime,
			State: store.FileAdopted, VerifiedAt: time.Now(),
		})
		if len(batch) >= adoptBatch {
			return flush()
		}
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return res, fmt.Errorf("adopt %q: the local root %q does not exist", pair.Name, root)
		}
		return res, fmt.Errorf("adopt %q: %w", pair.Name, err)
	}
	if err := flush(); err != nil {
		return res, err
	}

	res.Missing = int(res.RemoteFiles) - res.Matched - res.SizeMismatch
	if res.Missing < 0 {
		res.Missing = 0
	}
	res.Duration = time.Since(started)

	e.log.Infof("adopt: %q matched %s files (%s) in %s; %d differ in size, %d missing here, %d extra",
		pair.Name, format.Comma(int64(res.Matched)), format.Bytes(res.MatchedBytes), format.Duration(time.Since(started)),
		res.SizeMismatch, res.Missing, res.Extra)
	e.event(ctx, store.LevelInfo, store.KindAdopted, pair.ID,
		fmt.Sprintf("adopted %s files (%s) of %q", format.Comma(int64(res.Matched)), format.Bytes(res.MatchedBytes), pair.Name),
		map[string]any{
			"matched": res.Matched, "matchedBytes": res.MatchedBytes, "sizeMismatch": res.SizeMismatch,
			"extra": res.Extra, "missing": res.Missing, "excluded": res.Excluded,
			"seconds": time.Since(started).Seconds(),
		})
	return res, nil
}

func within(a, b time.Time, tolerance time.Duration) bool {
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	return d <= tolerance
}
