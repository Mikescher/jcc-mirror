// Package notify sends push messages through SimpleCloudNotifier, the channel
// that makes a stale source lock or a dead tunnel visible without anyone opening
// the pull-only dashboard. It is deliberately thin - the coalescing that keeps
// jcc-mirror inside the daily quota, and the edge-triggering that keeps a
// persistent condition to one message, belong to the policy layer above
// (DESIGN.md §4.1).
package notify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultEndpoint is the SCN send handler; "/send" is the same handler.
const DefaultEndpoint = "https://simplecloudnotifier.de/"

// How much of an error response is kept and how much more is swallowed to get to
// EOF, both in bytes, and how long a send may take.
const (
	maxExcerpt     = 512
	maxDrain       = 1 << 20
	defaultTimeout = 15 * time.Second
)

var (
	// ErrQuota is what a 403 means: the day's notifications are used up. The
	// policy layer treats it differently from a network failure, so it has to
	// survive as a sentinel rather than as a status code in a string.
	ErrQuota = errors.New("daily notification quota exhausted")

	// ErrDisabled separates "not configured" from "failed", so a fresh install
	// does not report an error for every notification it was never asked to send.
	ErrDisabled = errors.New("notifications are not configured")
)

// Config is the account half of a notification, held in the config table.
type Config struct {
	UserID  string
	UserKey string
	Channel string
	Sender  string
}

// Enabled reports whether the credentials are filled in. SCN authenticates with
// an id and a key together, so half a pair is as unusable as none.
func (c Config) Enabled() bool {
	return strings.TrimSpace(c.UserID) != "" && strings.TrimSpace(c.UserKey) != ""
}

// Message is the notification half. The zero Timestamp means now.
type Message struct {
	Title     string
	Content   string
	Priority  int    // 0, 1 or 2
	MsgID     string // idempotency key, see MsgID
	Timestamp time.Time
}

// Client sends messages to an SCN endpoint. The zero value works.
type Client struct {
	Endpoint string       // "" means DefaultEndpoint
	HTTP     *http.Client // nil means sharedHTTP
}

// The client used when none is supplied. Shared rather than built per send, so a
// run that emits a handful of notifications reuses the connection.
var sharedHTTP = &http.Client{Timeout: defaultTimeout}

// payload is the JSON body. user_id is a number, not a string, which is why the
// configured value has to be parsed rather than passed through.
type payload struct {
	UserID     int    `json:"user_id"`
	UserKey    string `json:"user_key"`
	Title      string `json:"title"`
	Content    string `json:"content,omitempty"`
	Channel    string `json:"channel,omitempty"`
	SenderName string `json:"sender_name,omitempty"`
	Priority   int    `json:"priority"`
	MsgID      string `json:"msg_id,omitempty"`
	Timestamp  int64  `json:"timestamp"` // unix seconds
}

// Send posts one message. It returns ErrDisabled if cfg is not filled in, and an
// error wrapping ErrQuota if the day's allowance is gone.
func (c *Client) Send(ctx context.Context, cfg Config, m Message) error {
	if !cfg.Enabled() {
		return ErrDisabled
	}
	userID, err := strconv.Atoi(strings.TrimSpace(cfg.UserID))
	if err != nil {
		return fmt.Errorf("notify.user_id %q is not an integer: %w", cfg.UserID, err)
	}
	if strings.TrimSpace(m.Title) == "" {
		return errors.New("notification has no title")
	}
	if m.Priority < 0 || m.Priority > 2 {
		return fmt.Errorf("notification priority %d is out of range, want 0, 1 or 2", m.Priority)
	}

	ts := m.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	body, err := json.Marshal(payload{
		UserID:     userID,
		UserKey:    strings.TrimSpace(cfg.UserKey),
		Title:      m.Title,
		Content:    m.Content,
		Channel:    strings.TrimSpace(cfg.Channel),
		SenderName: strings.TrimSpace(cfg.Sender),
		Priority:   m.Priority,
		MsgID:      strings.TrimSpace(m.MsgID),
		Timestamp:  ts.Unix(),
	})
	if err != nil {
		return fmt.Errorf("encode notification: %w", err)
	}

	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build notification request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "jcc-mirror")

	hc := c.HTTP
	if hc == nil {
		hc = sharedHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("send notification: %w", err)
	}
	defer resp.Body.Close()

	// Read on every path, including success: a body left unread keeps the
	// connection out of the idle pool.
	excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, maxExcerpt))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("send notification: %s: %w", resp.Status, ErrQuota)
	default:
		return fmt.Errorf("send notification: %s: %s", resp.Status, strings.TrimSpace(string(excerpt)))
	}
}

// MsgID is the idempotency key SCN dedups on: a hash of the parts that identify
// the notification, typically event kind, pair and day. Deriving it rather than
// recording what was already sent makes a retry after a network blip free, and
// collapses a repeated daily alert server-side (DESIGN.md §4.1). Parts are joined
// with NUL so that ("a", "bc") and ("ab", "c") cannot hash alike.
func MsgID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}
