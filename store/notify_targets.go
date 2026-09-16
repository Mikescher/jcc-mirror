package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ErrNoNotifyTarget is returned when a notification target does not exist.
var ErrNoNotifyTarget = errors.New("no such notification target")

// DefaultNotifySender is the sender name a target without one sends as.
const DefaultNotifySender = "jcc-mirror"

// NotifyTopic is one thing a target can choose to hear about. Several
// notification kinds may share a topic.
type NotifyTopic struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Help    string `json:"help"`
	Default bool   `json:"default"`
}

// NotifyTopics is every topic, in the order a target's events are stored and
// shown.
var NotifyTopics = []NotifyTopic{
	{
		Key: "sync_failed", Label: "Sync failed", Default: true,
		Help: "A run that failed, or finished with files it could not transfer. One message per run, never one per file.",
	},
	{
		Key: "sync_ok", Label: "Sync finished cleanly", Default: false,
		Help: "Off by default: a mirror that works is not news, and the daily quota is finite.",
	},
	{
		Key: "delete_blocked", Label: "Deletion awaiting approval", Default: true,
		Help: "A guard stopped a deletion and it is waiting for a person. Until someone answers, the mirror stops shrinking.",
	},
	{
		Key: "space_low", Label: "Free space below the reserve", Default: true,
		Help: "The destination volume no longer has room for what is queued.",
	},
	{
		Key: "lock_stale", Label: "Source lock stale", Default: true,
		Help: "The publisher's lock file has sat unchanged past the jCC threshold, so the database is silently not syncing.",
	},
	{
		Key: "tunnel_down", Label: "Tunnel down", Default: true,
		Help: "No WireGuard handshake for longer than the tunnel down grace period.",
	},
	{
		Key: "db_replaced", Label: "Database replaced", Default: true,
		Help: "ClipCornDB.db was copied over. Low priority, but it is the one file whose replacement is worth seeing.",
	},
	{
		Key: "update", Label: "Self-update", Default: true,
		Help: "A new binary was downloaded and put in place, or the supervisor had to put the old one back. The second is the one worth a message.",
	},
}

// NotifyTopicFor is the topic that decides whether a notification kind reaches a
// target. A kind with no topic is never sent.
func NotifyTopicFor(kind string) (string, bool) {
	topic, ok := notifyKindTopics[kind]
	return topic, ok
}

var notifyKindTopics = map[string]string{
	NotifySyncFailed:    "sync_failed",
	NotifySyncOK:        "sync_ok",
	NotifyDeleteBlocked: "delete_blocked",
	NotifySpaceLow:      "space_low",
	NotifyLockStale:     "lock_stale",
	NotifyTunnelDown:    "tunnel_down",
	NotifyDBReplaced:    "db_replaced",
	// Two kinds because they carry different priorities, one topic because
	// nobody wants to hear about half of an update.
	NotifyUpdateApplied:    "update",
	NotifyUpdateRolledBack: "update",
}

// DefaultNotifyEvents is the topics a target hears about unless told otherwise.
func DefaultNotifyEvents() []string {
	var out []string
	for _, t := range NotifyTopics {
		if t.Default {
			out = append(out, t.Key)
		}
	}
	return out
}

// NotifyTarget is one SCN account a notification can go to, with the topics it
// wants to hear about.
type NotifyTarget struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	UserID string `json:"userId"`
	// UserKey is never marshalled: the API says whether one is stored and
	// nothing more.
	UserKey    string    `json:"-"`
	UserKeySet bool      `json:"userKeySet"`
	Channel    string    `json:"channel"`
	Sender     string    `json:"sender"`
	Enabled    bool      `json:"enabled"`
	Events     []string  `json:"events"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Wants reports whether a notification kind is one of the target's topics.
func (t NotifyTarget) Wants(kind string) bool {
	topic, ok := NotifyTopicFor(kind)
	return ok && slices.Contains(t.Events, topic)
}

// Normalize trims what was typed and rejects what cannot work.
func (t *NotifyTarget) Normalize() error {
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" {
		return errors.New("name is required")
	}

	t.UserID = strings.TrimSpace(t.UserID)
	if t.UserID == "" {
		return errors.New("SCN user id is required")
	}
	if _, err := strconv.Atoi(t.UserID); err != nil {
		return fmt.Errorf("SCN user id %q is not a number", t.UserID)
	}

	t.UserKey = strings.TrimSpace(t.UserKey)
	if t.UserKey == "" {
		return errors.New("SCN user key is required")
	}

	t.Channel = strings.TrimSpace(t.Channel)
	t.Sender = strings.TrimSpace(t.Sender)
	if t.Sender == "" {
		t.Sender = DefaultNotifySender
	}

	events, err := normalizeTopics(t.Events)
	if err != nil {
		return err
	}
	t.Events = events
	t.UserKeySet = true
	return nil
}

// ParseNotifyEvents splits a comma-separated topic list. Blank items are
// dropped, so "" is no topics at all.
func ParseNotifyEvents(s string) []string {
	out := []string{}
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// normalizeTopics dedupes and puts topics in table order, refusing unknown ones.
// The result is never nil, so a target with no topics marshals as [].
func normalizeTopics(in []string) ([]string, error) {
	want := make(map[string]bool, len(in))
	for _, k := range in {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if !slices.ContainsFunc(NotifyTopics, func(t NotifyTopic) bool { return t.Key == k }) {
			return nil, fmt.Errorf("unknown notification topic %q", k)
		}
		want[k] = true
	}
	out := []string{}
	for _, t := range NotifyTopics {
		if want[t.Key] {
			out = append(out, t.Key)
		}
	}
	return out, nil
}

// CreateNotifyTarget stores a new target and fills in its ID.
func (s *Store) CreateNotifyTarget(ctx context.Context, t *NotifyTarget) error {
	if err := t.Normalize(); err != nil {
		return err
	}

	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO notify_targets (name, user_id, user_key, channel, sender, enabled, events, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Name, t.UserID, t.UserKey, t.Channel, t.Sender, boolInt(t.Enabled),
		strings.Join(t.Events, ","), now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return notifyTargetWriteError("create", t.Name, err)
	}
	if t.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("create notification target %q: %w", t.Name, err)
	}
	t.CreatedAt, t.UpdatedAt = now, now
	return nil
}

// UpdateNotifyTarget writes back every field but the ID and the creation time,
// the user key included: keeping a stored one is the caller's decision.
func (s *Store) UpdateNotifyTarget(ctx context.Context, t *NotifyTarget) error {
	if err := t.Normalize(); err != nil {
		return err
	}

	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE notify_targets SET name = ?, user_id = ?, user_key = ?, channel = ?, sender = ?,
		                           enabled = ?, events = ?, updated_at = ?
		 WHERE id = ?`,
		t.Name, t.UserID, t.UserKey, t.Channel, t.Sender, boolInt(t.Enabled),
		strings.Join(t.Events, ","), now.UnixMilli(), t.ID)
	if err != nil {
		return notifyTargetWriteError("update", t.Name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w with id %d", ErrNoNotifyTarget, t.ID)
	}
	t.UpdatedAt = now
	return nil
}

// DeleteNotifyTarget removes a target.
func (s *Store) DeleteNotifyTarget(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM notify_targets WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete notification target %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w with id %d", ErrNoNotifyTarget, id)
	}
	return nil
}

const notifyTargetColumns = `id, name, user_id, user_key, channel, sender, enabled, events, created_at, updated_at`

// NotifyTargets returns every target, oldest first. The result is never nil.
func (s *Store) NotifyTargets(ctx context.Context) ([]NotifyTarget, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+notifyTargetColumns+` FROM notify_targets ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read notification targets: %w", err)
	}
	defer rows.Close()

	out := []NotifyTarget{}
	for rows.Next() {
		t, err := scanNotifyTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// NotifyTargetByID returns one target.
func (s *Store) NotifyTargetByID(ctx context.Context, id int64) (NotifyTarget, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+notifyTargetColumns+` FROM notify_targets WHERE id = ?`, id)
	t, err := scanNotifyTarget(row)
	if errors.Is(err, sql.ErrNoRows) {
		return NotifyTarget{}, fmt.Errorf("%w with id %d", ErrNoNotifyTarget, id)
	}
	return t, err
}

func scanNotifyTarget(row rowScanner) (NotifyTarget, error) {
	var (
		t                NotifyTarget
		enabled          int64
		events           string
		created, updated int64
	)
	if err := row.Scan(&t.ID, &t.Name, &t.UserID, &t.UserKey, &t.Channel, &t.Sender,
		&enabled, &events, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return NotifyTarget{}, err
		}
		return NotifyTarget{}, fmt.Errorf("scan notification target: %w", err)
	}
	t.Enabled = enabled != 0
	t.Events = ParseNotifyEvents(events)
	t.UserKeySet = t.UserKey != ""
	t.CreatedAt, t.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	return t, nil
}

func notifyTargetWriteError(op, name string, err error) error {
	if strings.Contains(err.Error(), "UNIQUE constraint failed: notify_targets.name") {
		return fmt.Errorf("a notification target called %q already exists", name)
	}
	return fmt.Errorf("%s notification target %q: %w", op, name, err)
}
