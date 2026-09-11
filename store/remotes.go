package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrNoRemote is returned when a remote does not exist.
var ErrNoRemote = errors.New("no such remote")

// ErrRemoteInUse is returned when a remote that pairs still read from is removed.
var ErrRemoteInUse = errors.New("remote is in use")

// Remote is one SMB share on the publisher's side, rooted at a directory inside
// it. A pair names the remote it reads from; its own remote path is relative to
// that root (DESIGN.md §6).
type Remote struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Host  string `json:"host"`  // host or host:port; 445 when no port is given
	Share string `json:"share"` // the share name on its own
	Path  string `json:"path"`  // directory inside the share; "" is the share itself
	User  string `json:"user"`
	// Password is never marshalled: the API says whether one is stored and
	// nothing more, so it cannot leak out of a page or a log.
	Password    string    `json:"-"`
	PasswordSet bool      `json:"passwordSet"`
	Domain      string    `json:"domain"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// Normalize trims what was typed and rejects what cannot work. The rules are the
// ones the remote.* settings had, so a remote that was valid as settings is valid
// as a row.
func (r *Remote) Normalize() error {
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		return errors.New("name is required")
	}

	r.Host = strings.TrimSpace(r.Host)
	if err := validateHostPort(r.Host); err != nil {
		return fmt.Errorf("host: %w", err)
	}

	r.Share = strings.TrimSpace(r.Share)
	if err := validateShareName(r.Share); err != nil {
		return fmt.Errorf("share: %w", err)
	}

	r.Path = strings.Trim(strings.TrimSpace(r.Path), "/")
	if err := validateRelPath(r.Path); err != nil {
		return fmt.Errorf("path: %w", err)
	}

	r.User = strings.TrimSpace(r.User)
	if r.User == "" {
		return errors.New("username is required: SMB has no usable anonymous mode, even for a share open to everyone")
	}

	r.Domain = strings.TrimSpace(r.Domain)
	r.PasswordSet = r.Password != ""
	return nil
}

// CreateRemote stores a new remote and fills in its ID.
func (s *Store) CreateRemote(ctx context.Context, r *Remote) error {
	if err := r.Normalize(); err != nil {
		return err
	}

	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO remotes (name, host, share, path, username, password, domain, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Name, r.Host, r.Share, r.Path, r.User, r.Password, r.Domain, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return remoteWriteError("create", r.Name, err)
	}
	if r.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("create remote %q: %w", r.Name, err)
	}
	r.CreatedAt, r.UpdatedAt = now, now
	return nil
}

// UpdateRemote writes back every field but the ID and the creation time, the
// password included: keeping a stored one is the caller's decision.
func (s *Store) UpdateRemote(ctx context.Context, r *Remote) error {
	if err := r.Normalize(); err != nil {
		return err
	}

	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE remotes SET name = ?, host = ?, share = ?, path = ?, username = ?, password = ?,
		                    domain = ?, updated_at = ?
		 WHERE id = ?`,
		r.Name, r.Host, r.Share, r.Path, r.User, r.Password, r.Domain, now.UnixMilli(), r.ID)
	if err != nil {
		return remoteWriteError("update", r.Name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w with id %d", ErrNoRemote, r.ID)
	}
	r.UpdatedAt = now
	return nil
}

// DeleteRemote removes a remote that no pair reads from. One that is still in use
// is refused with the pairs named: they would otherwise be left reading from
// nothing, and the foreign key's own message does not say which.
func (s *Store) DeleteRemote(ctx context.Context, id int64) error {
	users, err := s.PairNamesForRemote(ctx, id)
	if err != nil {
		return err
	}
	switch len(users) {
	case 0:
	case 1:
		return fmt.Errorf("%w: pair %s reads from it; point it at another remote first",
			ErrRemoteInUse, quoteList(users))
	default:
		return fmt.Errorf("%w: pairs %s read from it; point them at another remote first",
			ErrRemoteInUse, quoteList(users))
	}

	res, err := s.db.ExecContext(ctx, `DELETE FROM remotes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete remote %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w with id %d", ErrNoRemote, id)
	}
	return nil
}

// PairNamesForRemote lists the pairs that read from a remote, in the order a sync
// walks them.
func (s *Store) PairNamesForRemote(ctx context.Context, id int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name FROM pairs WHERE remote_id = ? ORDER BY priority, id`, id)
	if err != nil {
		return nil, fmt.Errorf("read pairs of remote %d: %w", id, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan pair name: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

const remoteColumns = `id, name, host, share, path, username, password, domain, created_at, updated_at`

// Remotes returns every remote, oldest first. The first of them is the one the
// settings that name no remote fall back to.
func (s *Store) Remotes(ctx context.Context) ([]Remote, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+remoteColumns+` FROM remotes ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read remotes: %w", err)
	}
	defer rows.Close()

	var out []Remote
	for rows.Next() {
		r, err := scanRemote(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RemoteByID returns one remote.
func (s *Store) RemoteByID(ctx context.Context, id int64) (Remote, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+remoteColumns+` FROM remotes WHERE id = ?`, id)
	r, err := scanRemote(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Remote{}, fmt.Errorf("%w with id %d", ErrNoRemote, id)
	}
	return r, err
}

// FindRemote resolves what an operator typed: a name, or an id. Names win, as
// they do for pairs.
func (s *Store) FindRemote(ctx context.Context, ref string) (Remote, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Remote{}, fmt.Errorf("%w: no remote given", ErrNoRemote)
	}

	row := s.db.QueryRowContext(ctx, `SELECT `+remoteColumns+` FROM remotes WHERE name = ?`, ref)
	r, err := scanRemote(row)
	if err == nil {
		return r, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Remote{}, err
	}

	if id, convErr := strconv.ParseInt(ref, 10, 64); convErr == nil {
		return s.RemoteByID(ctx, id)
	}
	return Remote{}, fmt.Errorf("%w %q", ErrNoRemote, ref)
}

// ResolveRemote is FindRemote for a reference that may be left empty, which
// means the first remote. It is what a setting or a flag that names a remote
// goes through: with one remote configured, nobody has to name it.
func (s *Store) ResolveRemote(ctx context.Context, ref string) (Remote, error) {
	if strings.TrimSpace(ref) != "" {
		return s.FindRemote(ctx, ref)
	}

	row := s.db.QueryRowContext(ctx, `SELECT `+remoteColumns+` FROM remotes ORDER BY id LIMIT 1`)
	r, err := scanRemote(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Remote{}, fmt.Errorf("%w: none is configured yet", ErrNoRemote)
	}
	return r, err
}

func scanRemote(row rowScanner) (Remote, error) {
	var (
		r                Remote
		created, updated int64
	)
	if err := row.Scan(&r.ID, &r.Name, &r.Host, &r.Share, &r.Path, &r.User, &r.Password,
		&r.Domain, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Remote{}, err
		}
		return Remote{}, fmt.Errorf("scan remote: %w", err)
	}
	r.PasswordSet = r.Password != ""
	r.CreatedAt, r.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	return r, nil
}

// remoteWriteError turns the unique index on the name into a sentence. sqlite's
// own message names the column, which is not what the person typing wants to read.
func remoteWriteError(op, name string, err error) error {
	if strings.Contains(err.Error(), "UNIQUE constraint failed: remotes.name") {
		return fmt.Errorf("a remote called %q already exists", name)
	}
	return fmt.Errorf("%s remote %q: %w", op, name, err)
}

// checkRemote makes a pair's remote reference fail as ErrNoRemote rather than as
// a foreign key violation, which does not say what was wrong.
func (s *Store) checkRemote(ctx context.Context, id int64) error {
	if id == 0 {
		return nil
	}
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM remotes WHERE id = ?`, id).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w with id %d", ErrNoRemote, id)
	case err != nil:
		return fmt.Errorf("read remote %d: %w", id, err)
	}
	return nil
}

func quoteList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = strconv.Quote(n)
	}
	return strings.Join(quoted, ", ")
}
