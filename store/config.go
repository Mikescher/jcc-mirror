package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The keys of the config table. Everything jcc-mirror can be told is one of
// these: there is no config file, nothing is read from the environment, and only
// true invariants are compiled in (DESIGN.md §6).
const (
	KeyWGPrivateKey   = "wg.private_key"
	KeyWGPeerKey      = "wg.peer_public_key"
	KeyWGPresharedKey = "wg.preshared_key"
	KeyWGEndpoint     = "wg.endpoint"
	KeyWGAddress      = "wg.address"
	KeyWGAllowedIPs   = "wg.allowed_ips"
	KeyWGDNS          = "wg.dns"

	KeyRemoteURL      = "remote.url"
	KeyRemoteUser     = "remote.user"
	KeyRemotePassword = "remote.password"

	KeyScanWorkers    = "scan.workers"
	KeyMTimeTolerance = "scan.mtime_tolerance"
	KeyTransferChunks = "transfer.chunks"
	KeyChunkSize      = "transfer.chunk_size"
	KeyMaxAttempts    = "transfer.max_attempts"
	KeyRetryBackoff   = "transfer.retry_backoff"
	KeyHashAfterCopy  = "transfer.hash"

	KeyDashboardToken = "dashboard.token"
	KeyTimezone       = "general.timezone"
)

// SecretMask replaces a secret's value wherever one is shown or recorded. The
// audit trail says that a password changed, never what it changed to.
const SecretMask = "••••••••"

// KeyDef describes one setting. The setup view is generated from this table, so a
// key added in a later milestone costs no HTML.
type KeyDef struct {
	Name      string
	Group     string
	Label     string
	Help      string
	Default   string
	Secret    bool // masked in the UI and in the audit trail
	Generated bool // created at first start, never typed by hand
	Required  bool // the tunnel or the remote does not come up without it
	Validate  func(string) error
}

var keyDefs = []KeyDef{
	{
		Name: KeyWGPrivateKey, Group: "Tunnel", Label: "Private key",
		Help:      "Generated at first start and never leaves the data volume; paste the public half into the rootserver's peer entry.",
		Secret:    true,
		Generated: true,
		Required:  true,
		Validate:  validateKey,
	},
	{
		Name: KeyWGPeerKey, Group: "Tunnel", Label: "Rootserver public key",
		Help:     "The [Peer] PublicKey of the WireGuard server, base64.",
		Required: true,
		Validate: validateKey,
	},
	{
		Name: KeyWGEndpoint, Group: "Tunnel", Label: "Endpoint",
		Help:     "host:port of the rootserver. Resolved once at startup and never again, so an IP belongs here.",
		Required: true,
		Validate: validateEndpoint,
	},
	{
		Name: KeyWGAddress, Group: "Tunnel", Label: "Our tunnel address",
		Help:     "This container's address inside the tunnel, e.g. 10.13.13.3/32.",
		Required: true,
		Validate: validateAddrList,
	},
	{
		Name: KeyWGAllowedIPs, Group: "Tunnel", Label: "Allowed IPs",
		Help:     "CIDRs routed into the tunnel. Must cover the whole WG subnet, since the publisher is reached via the rootserver - but not 0.0.0.0/0.",
		Required: true,
		Validate: validateAllowedIPs,
	},
	{
		Name: KeyWGPresharedKey, Group: "Tunnel", Label: "Preshared key",
		Help:     "Optional.",
		Secret:   true,
		Validate: validateOptionalKey,
	},
	{
		Name: KeyWGDNS, Group: "Tunnel", Label: "DNS",
		Help:     "Resolvers reachable through the tunnel. Only needed when the WebDAV URL uses a hostname.",
		Validate: validateOptionalAddrList,
	},

	{
		Name: KeyRemoteURL, Group: "Remote", Label: "WebDAV base URL",
		Help:     "The remote root of the mirror, e.g. http://10.13.13.2:5005/media. Plain HTTP is fine and faster: the tunnel already encrypts.",
		Required: true,
		Validate: validateHTTPURL,
	},
	{
		Name: KeyRemoteUser, Group: "Remote", Label: "Username",
		Help: "A read-only account on the publisher's DSM.",
	},
	{
		Name: KeyRemotePassword, Group: "Remote", Label: "Password",
		Help:   "Stored in plaintext: it is a read-only account reached over a private tunnel.",
		Secret: true,
	},

	{
		Name: KeyScanWorkers, Group: "Scan", Label: "Walk concurrency",
		Help:     "PROPFINDs in flight. The walk is latency-bound, so a little parallelism helps a lot and more helps nothing.",
		Default:  "8",
		Validate: validatePositiveInt,
	},
	{
		Name: KeyMTimeTolerance, Group: "Scan", Label: "Mtime tolerance",
		Help:     "How far apart two timestamps may be and still count as the same file. WebDAV dates carry whole seconds only, so this must never be zero.",
		Default:  "2s",
		Validate: validateDuration,
	},

	{
		Name: KeyTransferChunks, Group: "Transfer", Label: "Streams per file",
		Help:     "Parallel ranged streams within one file; 1 is strictly sequential. Files are still transferred one at a time.",
		Default:  "4",
		Validate: validatePositiveInt,
	},
	{
		Name: KeyChunkSize, Group: "Transfer", Label: "Chunk size",
		Help:     "The span one stream fetches per request. Also the resume granularity: an interrupted file loses at most this much per stream.",
		Default:  "64MiB",
		Validate: validateSize,
	},
	{
		Name: KeyMaxAttempts, Group: "Transfer", Label: "Attempts per file",
		Help:     "How often a file is retried before the job is marked failed and waits for someone to look at it.",
		Default:  "5",
		Validate: validatePositiveInt,
	},
	{
		Name: KeyRetryBackoff, Group: "Transfer", Label: "Retry backoff",
		Help:     "The wait after the first failed attempt. It doubles per attempt, up to an hour.",
		Default:  "30s",
		Validate: validateDuration,
	},
	{
		Name: KeyHashAfterCopy, Group: "Transfer", Label: "Hash after copy",
		Help:     "sha256 every file once it has landed and keep it for a future scrub. It costs a second read of everything transferred.",
		Default:  "false",
		Validate: validateBool,
	},

	{
		Name: KeyDashboardToken, Group: "Dashboard", Label: "Bearer token",
		Help:      "Generated at first start and printed to the container log. Required for every non-GET request.",
		Secret:    true,
		Generated: true,
		Required:  true,
	},
	{
		Name: KeyTimezone, Group: "General", Label: "Timezone",
		Help:     "The zone the transfer schedule is expressed in. Deliberately a setting of its own rather than the container's TZ.",
		Default:  "Europe/Berlin",
		Validate: validateTimezone,
	},
}

var keyByName = func() map[string]KeyDef {
	m := make(map[string]KeyDef, len(keyDefs))
	for _, d := range keyDefs {
		m[d.Name] = d
	}
	return m
}()

// Keys returns every defined setting, in the order the setup view renders them.
func Keys() []KeyDef { return append([]KeyDef(nil), keyDefs...) }

// Key looks one setting up by name.
func Key(name string) (KeyDef, bool) {
	d, ok := keyByName[name]
	return d, ok
}

// ErrUnknownKey is returned by ConfigSet for a name that is not in the registry.
// Unknown keys are refused rather than stored: a typo that silently persists is a
// setting that silently never takes effect.
var ErrUnknownKey = errors.New("unknown config key")

// ValidationError is a value the registry rejected. It is a type rather than a
// message so a caller can tell a mistyped endpoint from a broken database.
type ValidationError struct {
	Key   string
	Label string
	Err   error
}

func (e *ValidationError) Error() string { return e.Label + ": " + e.Err.Error() }
func (e *ValidationError) Unwrap() error { return e.Err }

// Values is a full snapshot of the configuration, with defaults filled in for
// everything unset.
type Values map[string]string

func (v Values) Get(key string) string { return v[key] }

// Int, Duration, Size and Bool read a setting in the type it is written in. A
// stored value is validated before it is written, so a parse failure here means
// the table was edited by hand - the registry's default is a better answer than
// a zero.
func (v Values) Int(key string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v[key]))
	if err != nil {
		n, _ = strconv.Atoi(keyByName[key].Default)
	}
	return n
}

func (v Values) Duration(key string) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(v[key]))
	if err != nil {
		d, _ = time.ParseDuration(keyByName[key].Default)
	}
	return d
}

func (v Values) Size(key string) int64 {
	n, err := ParseSize(v[key])
	if err != nil {
		n, _ = ParseSize(keyByName[key].Default)
	}
	return n
}

func (v Values) Bool(key string) bool {
	b, err := strconv.ParseBool(strings.TrimSpace(v[key]))
	if err != nil {
		b, _ = strconv.ParseBool(keyByName[key].Default)
	}
	return b
}

// Config returns every setting, defaults included.
func (s *Store) Config(ctx context.Context) (Values, error) {
	out := make(Values, len(keyDefs))
	for _, d := range keyDefs {
		out[d.Name] = d.Default
	}

	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM config`)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan config: %w", err)
		}
		// A key dropped in a later version stays in the table but is not handed out:
		// nothing reads it, and surfacing it would only invite editing it.
		if _, ok := keyByName[k]; ok {
			out[k] = v
		}
	}
	return out, rows.Err()
}

// ConfigGet reads one setting, falling back to its default.
func (s *Store) ConfigGet(ctx context.Context, key string) (string, error) {
	def, ok := keyByName[key]
	if !ok {
		return "", fmt.Errorf("%w %q", ErrUnknownKey, key)
	}

	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM config WHERE key = ?`, key).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return def.Default, nil
	case err != nil:
		return "", fmt.Errorf("read config %q: %w", key, err)
	}
	return v, nil
}

// ConfigSet writes settings and records every change in config_audit. It is
// all-or-nothing: a batch with one bad value writes none of it, so a half-applied
// tunnel configuration cannot happen. changed lists the keys whose value actually
// moved, which is what the caller uses to decide whether to re-open the tunnel.
func (s *Store) ConfigSet(ctx context.Context, values map[string]string, actor string) (changed []string, err error) {
	names := make([]string, 0, len(values))
	for k := range values {
		names = append(names, k)
	}
	sort.Strings(names)

	for _, k := range names {
		def, ok := keyByName[k]
		if !ok {
			return nil, fmt.Errorf("%w %q", ErrUnknownKey, k)
		}
		if def.Validate != nil {
			if err := def.Validate(values[k]); err != nil {
				return nil, &ValidationError{Key: k, Label: def.Label, Err: err}
			}
		}
	}

	now := time.Now().UnixMilli()
	err = s.tx(ctx, func(tx *sql.Tx) error {
		for _, k := range names {
			var old sql.NullString
			err := tx.QueryRowContext(ctx, `SELECT value FROM config WHERE key = ?`, k).Scan(&old)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("read config %q: %w", k, err)
			}
			if old.Valid && old.String == values[k] {
				continue
			}

			if _, err := tx.ExecContext(ctx,
				`INSERT INTO config (key, value, updated_at) VALUES (?, ?, ?)
				 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
				k, values[k], now); err != nil {
				return fmt.Errorf("write config %q: %w", k, err)
			}

			auditOld, auditNew := old, values[k]
			if keyByName[k].Secret {
				if auditOld.Valid {
					auditOld.String = SecretMask
				}
				auditNew = SecretMask
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO config_audit (ts, key, old_value, new_value, actor) VALUES (?, ?, ?, ?, ?)`,
				now, k, auditOld, auditNew, actor); err != nil {
				return fmt.Errorf("audit config %q: %w", k, err)
			}
			changed = append(changed, k)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return changed, nil
}

// AuditEntry is one row of the configuration's history.
type AuditEntry struct {
	ID    int64     `json:"id"`
	TS    time.Time `json:"ts"`
	Key   string    `json:"key"`
	Old   string    `json:"old"`
	New   string    `json:"new"`
	Actor string    `json:"actor"`
}

// ConfigAudit returns the most recent configuration changes, newest first.
func (s *Store) ConfigAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ts, key, old_value, new_value, actor FROM config_audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("read config audit: %w", err)
	}
	defer rows.Close()

	var out []AuditEntry
	for rows.Next() {
		var (
			e  AuditEntry
			ms int64
			ov sql.NullString
		)
		if err := rows.Scan(&e.ID, &ms, &e.Key, &ov, &e.New, &e.Actor); err != nil {
			return nil, fmt.Errorf("scan config audit: %w", err)
		}
		e.TS = time.UnixMilli(ms)
		e.Old = ov.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// EnsureGenerated creates the settings that are never typed by hand - the
// WireGuard private key and the dashboard token - and returns the names of the
// ones it had to create. generate is called per key; it exists so the wg package
// stays out of this one's imports.
func (s *Store) EnsureGenerated(ctx context.Context, generate func(key string) (string, error)) ([]string, error) {
	var created []string
	for _, d := range keyDefs {
		if !d.Generated {
			continue
		}
		cur, err := s.ConfigGet(ctx, d.Name)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(cur) != "" {
			continue
		}
		v, err := generate(d.Name)
		if err != nil {
			return nil, fmt.Errorf("generate %s: %w", d.Name, err)
		}
		if _, err := s.ConfigSet(ctx, map[string]string{d.Name: v}, "system"); err != nil {
			return nil, err
		}
		created = append(created, d.Name)
	}
	return created, nil
}

func validateKey(s string) error {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("not base64: %w", err)
	}
	if len(raw) != 32 {
		return fmt.Errorf("%d bytes, want 32", len(raw))
	}
	return nil
}

func validateOptionalKey(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return validateKey(s)
}

func validateEndpoint(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("required")
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return fmt.Errorf("expected host:port: %w", err)
	}
	if host == "" {
		return errors.New("expected host:port")
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return fmt.Errorf("bad port %q", port)
	}
	return nil
}

func validateAddrList(s string) error {
	fields := splitList(s)
	if len(fields) == 0 {
		return errors.New("required")
	}
	for _, f := range fields {
		if _, err := netip.ParsePrefix(f); err == nil {
			continue
		}
		if _, err := netip.ParseAddr(f); err != nil {
			return fmt.Errorf("%q is not an IP address: %w", f, err)
		}
	}
	return nil
}

func validateOptionalAddrList(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return validateAddrList(s)
}

// validateAllowedIPs refuses a default route. Routing everything into the tunnel
// would make the rootserver this container's way out to the internet, which is
// not what a mirror wants and is a slow, surprising way to discover it.
func validateAllowedIPs(s string) error {
	if err := validateAddrList(s); err != nil {
		return err
	}
	for _, f := range splitList(s) {
		p, err := netip.ParsePrefix(f)
		if err != nil {
			continue
		}
		if p.Bits() == 0 {
			return fmt.Errorf("%q would make the tunnel this container's default route; route the WG subnet, not everything", f)
		}
	}
	return nil
}

func validateHTTPURL(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("required")
	}
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("needs an http or https scheme")
	}
	if u.Host == "" {
		return errors.New("no host")
	}
	return nil
}

func validatePositiveInt(s string) error {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("not a number: %w", err)
	}
	if n < 1 {
		return errors.New("must be at least 1")
	}
	return nil
}

func validateDuration(s string) error {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("not a duration like \"30s\" or \"5m\": %w", err)
	}
	if d <= 0 {
		return errors.New("must be positive")
	}
	return nil
}

func validateSize(s string) error {
	_, err := ParseSize(s)
	return err
}

func validateBool(s string) error {
	if _, err := strconv.ParseBool(strings.TrimSpace(s)); err != nil {
		return errors.New("want true or false")
	}
	return nil
}

// ParseSize reads a byte count, with or without a binary suffix: "67108864",
// "64MiB" and "64M" are the same number. Sizes in this project are quoted in
// whichever of the two an operator happens to reach for.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("required")
	}

	digits := strings.TrimRight(s, "kKmMgGtTiIbB \t")
	suffix := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(s, digits)))
	suffix = strings.TrimSuffix(strings.TrimSuffix(suffix, "b"), "i")

	n, err := strconv.ParseInt(strings.TrimSpace(digits), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("not a size like \"64MiB\": %w", err)
	}

	mult := int64(1)
	switch suffix {
	case "":
	case "k":
		mult = 1 << 10
	case "m":
		mult = 1 << 20
	case "g":
		mult = 1 << 30
	case "t":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("unknown size suffix %q", suffix)
	}
	if n <= 0 {
		return 0, errors.New("must be positive")
	}
	return n * mult, nil
}

func validateTimezone(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("required")
	}
	_, err := time.LoadLocation(s)
	return err
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
