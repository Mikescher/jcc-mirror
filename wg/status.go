package wg

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Status is the device state as reported over the wireguard-go IPC interface.
type Status struct {
	PublicKey  string // ours, base64
	ListenPort int
	Peers      []Peer
}

// Peer is one configured peer.
type Peer struct {
	PublicKey     string // base64
	Endpoint      string
	LastHandshake time.Time // zero when the handshake has never completed
	TxBytes       int64
	RxBytes       int64
	Keepalive     int
	AllowedIPs    []string
}

// HandshakeAge reports how long ago the last handshake completed. ok is false
// when there has never been one, which is a different thing from "a long time
// ago" and reads differently on a dashboard.
func (p Peer) HandshakeAge() (age time.Duration, ok bool) {
	if p.LastHandshake.IsZero() {
		return 0, false
	}
	return time.Since(p.LastHandshake), true
}

// Status queries the running device.
func (t *Tunnel) Status() (Status, error) {
	raw, err := t.dev.IpcGet()
	if err != nil {
		return Status{}, fmt.Errorf("wireguard ipc get: %w", err)
	}
	return parseStatus(raw)
}

// parseStatus decodes the "key=value" lines of an IPC get response. The device's
// own settings come first; every public_key line starts a new peer.
func parseStatus(raw string) (Status, error) {
	var (
		st          Status
		cur         *Peer
		hsSec, hsNs int64
	)

	flush := func() {
		if cur == nil {
			return
		}
		if hsSec != 0 || hsNs != 0 {
			cur.LastHandshake = time.Unix(hsSec, hsNs)
		}
		st.Peers = append(st.Peers, *cur)
		cur, hsSec, hsNs = nil, 0, 0
	}

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		switch key {
		case "private_key":
			pub, err := publicFromHexPrivate(val)
			if err != nil {
				return Status{}, err
			}
			st.PublicKey = pub
		case "listen_port":
			st.ListenPort = atoiOr(val, 0)
		case "public_key":
			flush()
			b64, err := hexToBase64(val)
			if err != nil {
				return Status{}, fmt.Errorf("peer public key: %w", err)
			}
			cur = &Peer{PublicKey: b64}
		case "errno":
			if n := atoiOr(val, 0); n != 0 {
				return Status{}, fmt.Errorf("wireguard ipc get: errno %d", n)
			}
		}

		if cur == nil {
			continue
		}
		switch key {
		case "endpoint":
			cur.Endpoint = val
		case "last_handshake_time_sec":
			hsSec = int64(atoiOr(val, 0))
		case "last_handshake_time_nsec":
			hsNs = int64(atoiOr(val, 0))
		case "tx_bytes":
			cur.TxBytes = int64(atoiOr(val, 0))
		case "rx_bytes":
			cur.RxBytes = int64(atoiOr(val, 0))
		case "persistent_keepalive_interval":
			cur.Keepalive = atoiOr(val, 0)
		case "allowed_ip":
			cur.AllowedIPs = append(cur.AllowedIPs, val)
		}
	}
	flush()

	return st, nil
}

// publicFromHexPrivate derives the base64 public key from the hex private key the
// IPC interface reports back.
func publicFromHexPrivate(hexPriv string) (string, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(hexPriv))
	if err != nil {
		return "", fmt.Errorf("decode private key: %w", err)
	}
	return PublicKey(base64.StdEncoding.EncodeToString(raw))
}

func hexToBase64(s string) (string, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return "", fmt.Errorf("decode key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}
