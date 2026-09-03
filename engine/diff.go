package engine

import (
	"context"
	"fmt"
	"time"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/store"
)

// PlanEntry is one file the next sync would touch.
type PlanEntry struct {
	Path       string    `json:"path"`
	Op         string    `json:"op"`
	Size       int64     `json:"size"`                 // on the publisher
	SizeBefore int64     `json:"sizeBefore,omitempty"` // here, for a replace
	MTime      time.Time `json:"mtime"`
}

// Plan is the difference between the last walk and local truth: what a sync
// would do, in numbers, before it does any of it. Every plan can be computed and
// displayed without executing (DESIGN.md §6).
type Plan struct {
	PairID    int64     `json:"pairId"`
	PairName  string    `json:"pair"`
	ScannedAt time.Time `json:"scannedAt"`

	Add          int   `json:"add"`
	AddBytes     int64 `json:"addBytes"`
	Replace      int   `json:"replace"`
	ReplaceBytes int64 `json:"replaceBytes"`

	// Vanished is what we hold and the publisher no longer has. M2 counts it and
	// stops there; deleting is M4, behind the guards of DESIGN.md §2.5.
	Vanished      int   `json:"vanished"`
	VanishedBytes int64 `json:"vanishedBytes"`

	// Excluded counts the differences the pair's globs dropped, so a plan that is
	// unexpectedly small says why.
	Excluded int `json:"excluded"`

	RemoteFiles int64 `json:"remoteFiles"`
	RemoteBytes int64 `json:"remoteBytes"`
	LocalFiles  int64 `json:"localFiles"`
	LocalBytes  int64 `json:"localBytes"`

	// The free-space picture at the destination (DESIGN.md §2.6). Shortfall is
	// how much more room the plan needs than there is; anything above zero means
	// a sync would be refused, which is worth knowing from a dry run rather than
	// from the run that stops.
	Free      int64 `json:"free"`
	Reserve   int64 `json:"reserve"`
	Shortfall int64 `json:"shortfall"`

	Entries   []PlanEntry `json:"entries,omitempty"`
	Truncated bool        `json:"truncated,omitempty"`

	// Queued is how many transfers Enqueue actually wrote. It stays zero for a
	// dry run.
	Queued int `json:"queued"`
}

// Transfers is how many files the plan would move, and TransferBytes how much.
func (p Plan) Transfers() int       { return p.Add + p.Replace }
func (p Plan) TransferBytes() int64 { return p.AddBytes + p.ReplaceBytes }
func (p Plan) String() string {
	return fmt.Sprintf("%s: %d add (%s), %d replace (%s), %d vanished (%s)",
		p.PairName, p.Add, format.Bytes(p.AddBytes), p.Replace, format.Bytes(p.ReplaceBytes),
		p.Vanished, format.Bytes(p.VanishedBytes))
}

// Plan computes what a sync would do. sample caps how many entries come back for
// display; the counts are always complete.
func (e *Engine) Plan(ctx context.Context, pair store.Pair, sample int) (Plan, error) {
	return e.plan(ctx, pair, sample, nil)
}

// Enqueue computes the plan and writes its transfers into the job queue. It is
// idempotent: a file already queued is refreshed rather than queued twice, and a
// file whose remote copy changed since it was queued loses its resume watermark,
// because the bytes in its .part file belong to a version that no longer exists.
func (e *Engine) Enqueue(ctx context.Context, pair store.Pair) (Plan, error) {
	if err := Transferable(pair); err != nil {
		return Plan{}, err
	}

	var queued int
	p, err := e.plan(ctx, pair, 0, func(entry PlanEntry) error {
		if entry.Op == store.OpDelete {
			// Counted by the plan, never queued: the tree only ever grows until the
			// guards of DESIGN.md §2.5 exist to shrink it safely.
			return nil
		}
		if _, err := e.store.EnqueueJob(ctx, store.Job{
			PairID:     pair.ID,
			Path:       entry.Path,
			Op:         entry.Op,
			BytesTotal: entry.Size,
			MTime:      entry.MTime,
		}); err != nil {
			return err
		}
		queued++
		return nil
	})
	if err != nil {
		return p, err
	}
	p.Queued = queued
	return p, nil
}

// plan walks both directions of the diff, calling fn for every entry. Nothing is
// held in memory but the sample: the manifest of the real collection is six
// figures of rows.
func (e *Engine) plan(ctx context.Context, pair store.Pair, sample int, fn func(PlanEntry) error) (Plan, error) {
	scan, ok, err := e.store.LastCompletedScan(ctx, pair.ID)
	if err != nil {
		return Plan{}, err
	}
	if !ok {
		return Plan{}, fmt.Errorf("pair %q: %w", pair.Name, errNoManifest)
	}

	p := Plan{PairID: pair.ID, PairName: pair.Name}
	if scan.FinishedAt != nil {
		p.ScannedAt = *scan.FinishedAt
	}
	if p.RemoteFiles, p.RemoteBytes, err = e.store.ManifestStats(ctx, pair.ID); err != nil {
		return Plan{}, err
	}
	if p.LocalFiles, p.LocalBytes, err = e.store.FileStats(ctx, pair.ID); err != nil {
		return Plan{}, err
	}
	p.Reserve = e.opts.Reserve
	if space, err := FreeSpace(pair.LocalPath); err == nil {
		p.Free = space.Free
	}

	f := newFilter(pair)
	keep := func(entry PlanEntry) error {
		if len(p.Entries) < sample {
			p.Entries = append(p.Entries, entry)
		} else if sample > 0 {
			p.Truncated = true
		}
		if fn == nil {
			return nil
		}
		return fn(entry)
	}

	err = e.store.ChangedFiles(ctx, pair.ID, e.opts.MTimeTolerance, func(d store.DiffRow) error {
		if !f.allows(d.Path) {
			p.Excluded++
			return nil
		}

		entry := PlanEntry{Path: d.Path, Op: store.OpAdd, Size: d.Size, MTime: d.MTime}
		if d.Have {
			entry.Op, entry.SizeBefore = store.OpReplace, d.LocalSize
			p.Replace++
			p.ReplaceBytes += d.Size
		} else {
			p.Add++
			p.AddBytes += d.Size
		}
		return keep(entry)
	})
	if err != nil {
		return Plan{}, err
	}

	// A file the globs no longer cover is not a deletion: excluding a subtree
	// stops mirroring it, it does not empty it.
	err = e.store.VanishedFiles(ctx, pair.ID, func(relpath string, size int64) error {
		if !f.allows(relpath) {
			p.Excluded++
			return nil
		}
		p.Vanished++
		p.VanishedBytes += size
		return keep(PlanEntry{Path: relpath, Op: store.OpDelete, SizeBefore: size})
	})
	if err != nil {
		return Plan{}, err
	}

	// A replace stages its .part beside the file it replaces, so what it needs is
	// the new file in full - not the difference between the two.
	if p.Free > 0 {
		if short := p.TransferBytes() + p.Reserve - p.Free; short > 0 {
			p.Shortfall = short
		}
	}
	return p, nil
}
