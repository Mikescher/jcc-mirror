package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

// Pair types. A jcc pair is the ClipCornDB directory: it never transfers the
// per-user databases beside the shared one, and the shared one itself is copied
// through a lock gate rather than through the job queue (DESIGN.md §3).
const (
	PairRaw = "raw"
	PairJCC = "jcc"
)

// Pair modes. In additive mode the local tree only ever grows. In mirror mode a
// file the publisher no longer has is quarantined here too, behind the guards of
// DESIGN.md §2.5 - which is why additive is the default: run additive-only until
// the diff is trusted.
const (
	ModeAdditive = "additive"
	ModeMirror   = "mirror"
)

// ErrNoPair is returned when a pair does not exist.
var ErrNoPair = errors.New("no such pair")

// Pair is one directory of the publisher's share mapped onto one directory here
// (DESIGN.md §6).
type Pair struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	RemotePath  string    `json:"remotePath"` // relative to the remote root; "" is the root itself
	LocalPath   string    `json:"localPath"`  // absolute, inside the container
	Mode        string    `json:"mode"`
	Includes    []string  `json:"includes"`
	Excludes    []string  `json:"excludes"`
	DeleteGuard int       `json:"deleteGuard"` // deletions above this many files wait for approval; 0 is no count limit
	Priority    int       `json:"priority"`    // lower runs first
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// Normalize fills in the defaults and rejects what cannot work. The local path
// has to be absolute: it is resolved by a daemon whose working directory is
// nothing in particular, and a relative one would silently mirror into /.
func (p *Pair) Normalize() error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return errors.New("name is required")
	}

	if p.Type == "" {
		p.Type = PairRaw
	}
	if p.Type != PairRaw && p.Type != PairJCC {
		return fmt.Errorf("type %q: want %q or %q", p.Type, PairRaw, PairJCC)
	}

	if p.Mode == "" {
		p.Mode = ModeAdditive
	}
	if p.Mode != ModeAdditive && p.Mode != ModeMirror {
		return fmt.Errorf("mode %q: want %q or %q", p.Mode, ModeAdditive, ModeMirror)
	}

	// The remote side is compared against the paths the SMB client reports, which
	// it hands back NFC-normalized; a remote path in NFD would match nothing at all.
	p.RemotePath = norm.NFC.String(strings.Trim(strings.TrimSpace(p.RemotePath), "/"))
	if p.RemotePath != "" {
		p.RemotePath = strings.TrimPrefix(path.Clean("/"+p.RemotePath), "/")
	}

	p.LocalPath = filepath.Clean(strings.TrimSpace(p.LocalPath))
	if p.LocalPath == "" || p.LocalPath == "." {
		return errors.New("local path is required")
	}
	if !filepath.IsAbs(p.LocalPath) {
		return fmt.Errorf("local path %q must be absolute", p.LocalPath)
	}

	p.Includes = cleanPatterns(p.Includes)
	p.Excludes = cleanPatterns(p.Excludes)

	if p.Priority == 0 {
		p.Priority = 100
	}
	if p.DeleteGuard < 0 {
		return fmt.Errorf("delete guard %d cannot be negative", p.DeleteGuard)
	}
	return nil
}

func cleanPatterns(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, norm.NFC.String(strings.Trim(s, "/")))
		}
	}
	return out
}

// CreatePair stores a new pair and fills in its ID.
func (s *Store) CreatePair(ctx context.Context, p *Pair) error {
	if err := p.Normalize(); err != nil {
		return err
	}

	inc, exc, err := marshalPatterns(p)
	if err != nil {
		return err
	}

	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO pairs (name, type, remote_path, local_path, mode, includes, excludes,
		                    delete_guard, priority, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.Type, p.RemotePath, p.LocalPath, p.Mode, inc, exc,
		p.DeleteGuard, p.Priority, boolInt(p.Enabled), now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return fmt.Errorf("create pair %q: %w", p.Name, err)
	}
	if p.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("create pair %q: %w", p.Name, err)
	}
	p.CreatedAt, p.UpdatedAt = now, now
	return nil
}

// UpdatePair writes back every field but the ID and the creation time.
func (s *Store) UpdatePair(ctx context.Context, p *Pair) error {
	if err := p.Normalize(); err != nil {
		return err
	}

	inc, exc, err := marshalPatterns(p)
	if err != nil {
		return err
	}

	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE pairs SET name = ?, type = ?, remote_path = ?, local_path = ?, mode = ?,
		                  includes = ?, excludes = ?, delete_guard = ?, priority = ?,
		                  enabled = ?, updated_at = ?
		 WHERE id = ?`,
		p.Name, p.Type, p.RemotePath, p.LocalPath, p.Mode, inc, exc,
		p.DeleteGuard, p.Priority, boolInt(p.Enabled), now.UnixMilli(), p.ID)
	if err != nil {
		return fmt.Errorf("update pair %d: %w", p.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w with id %d", ErrNoPair, p.ID)
	}
	p.UpdatedAt = now
	return nil
}

// DeletePair removes a pair and, by cascade, its manifest, files, jobs and scans.
// The local files themselves are never touched.
func (s *Store) DeletePair(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM pairs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete pair %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w with id %d", ErrNoPair, id)
	}
	return nil
}

const pairColumns = `id, name, type, remote_path, local_path, mode, includes, excludes,
                     delete_guard, priority, enabled, created_at, updated_at`

// Pairs returns every configured pair, in the order a sync run walks them.
func (s *Store) Pairs(ctx context.Context) ([]Pair, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pairColumns+` FROM pairs ORDER BY priority, id`)
	if err != nil {
		return nil, fmt.Errorf("read pairs: %w", err)
	}
	defer rows.Close()

	var out []Pair
	for rows.Next() {
		p, err := scanPair(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PairByID returns one pair.
func (s *Store) PairByID(ctx context.Context, id int64) (Pair, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+pairColumns+` FROM pairs WHERE id = ?`, id)
	p, err := scanPair(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Pair{}, fmt.Errorf("%w with id %d", ErrNoPair, id)
	}
	return p, err
}

// FindPair resolves what an operator typed: a name, or an id. Names win, so a
// pair called "2" is still reachable by its name.
func (s *Store) FindPair(ctx context.Context, ref string) (Pair, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Pair{}, fmt.Errorf("%w: no pair given", ErrNoPair)
	}

	row := s.db.QueryRowContext(ctx, `SELECT `+pairColumns+` FROM pairs WHERE name = ?`, ref)
	p, err := scanPair(row)
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Pair{}, err
	}

	if id, convErr := strconv.ParseInt(ref, 10, 64); convErr == nil {
		return s.PairByID(ctx, id)
	}
	return Pair{}, fmt.Errorf("%w %q", ErrNoPair, ref)
}

// rowScanner is what *sql.Row and *sql.Rows have in common.
type rowScanner interface{ Scan(...any) error }

func scanPair(row rowScanner) (Pair, error) {
	var (
		p                Pair
		inc, exc         string
		enabled          int
		created, updated int64
	)
	if err := row.Scan(&p.ID, &p.Name, &p.Type, &p.RemotePath, &p.LocalPath, &p.Mode,
		&inc, &exc, &p.DeleteGuard, &p.Priority, &enabled, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Pair{}, err
		}
		return Pair{}, fmt.Errorf("scan pair: %w", err)
	}

	if err := json.Unmarshal([]byte(inc), &p.Includes); err != nil {
		return Pair{}, fmt.Errorf("pair %d: decode includes: %w", p.ID, err)
	}
	if err := json.Unmarshal([]byte(exc), &p.Excludes); err != nil {
		return Pair{}, fmt.Errorf("pair %d: decode excludes: %w", p.ID, err)
	}
	p.Enabled = enabled != 0
	p.CreatedAt, p.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	return p, nil
}

func marshalPatterns(p *Pair) (string, string, error) {
	inc, err := json.Marshal(orEmpty(p.Includes))
	if err != nil {
		return "", "", fmt.Errorf("encode includes: %w", err)
	}
	exc, err := json.Marshal(orEmpty(p.Excludes))
	if err != nil {
		return "", "", fmt.Errorf("encode excludes: %w", err)
	}
	return string(inc), string(exc), nil
}

// orEmpty keeps a nil slice out of the column: the schema defaults to '[]' and a
// stored "null" would decode back to nil everywhere it is read.
func orEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
