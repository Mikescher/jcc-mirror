package wg

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// ipcSample is the shape wireguard-go answers an IPC get with. The keys are hex
// there and base64 everywhere a human sees them, which is the conversion this
// package exists to keep in one place.
var ipcSample = strings.Join([]string{
	"private_key=" + strings.Repeat("11", 32),
	"listen_port=51820",
	"public_key=" + strings.Repeat("22", 32),
	"preshared_key=" + strings.Repeat("00", 32),
	"protocol_version=1",
	"endpoint=203.0.113.7:51820",
	"last_handshake_time_sec=1700000000",
	"last_handshake_time_nsec=500000000",
	"tx_bytes=4096",
	"rx_bytes=8192",
	"persistent_keepalive_interval=25",
	"allowed_ip=10.13.13.0/24",
	"errno=0",
	"",
}, "\n")

func TestParseStatus(t *testing.T) {
	st, err := parseStatus(ipcSample)
	if err != nil {
		t.Fatalf("parseStatus: %v", err)
	}

	if st.ListenPort != 51820 {
		t.Errorf("ListenPort = %d, want 51820", st.ListenPort)
	}
	wantPub, err := PublicKey(base64.StdEncoding.EncodeToString(mustHex(t, strings.Repeat("11", 32))))
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	if st.PublicKey != wantPub {
		t.Errorf("PublicKey = %q, want %q", st.PublicKey, wantPub)
	}

	if len(st.Peers) != 1 {
		t.Fatalf("got %d peers, want 1", len(st.Peers))
	}
	p := st.Peers[0]
	if want := base64.StdEncoding.EncodeToString(mustHex(t, strings.Repeat("22", 32))); p.PublicKey != want {
		t.Errorf("peer PublicKey = %q, want %q", p.PublicKey, want)
	}
	if p.Endpoint != "203.0.113.7:51820" {
		t.Errorf("Endpoint = %q", p.Endpoint)
	}
	if !p.LastHandshake.Equal(time.Unix(1700000000, 500000000)) {
		t.Errorf("LastHandshake = %v", p.LastHandshake)
	}
	if p.TxBytes != 4096 || p.RxBytes != 8192 {
		t.Errorf("counters = %d/%d, want 4096/8192", p.TxBytes, p.RxBytes)
	}
	if p.Keepalive != 25 {
		t.Errorf("Keepalive = %d, want 25", p.Keepalive)
	}
	if len(p.AllowedIPs) != 1 || p.AllowedIPs[0] != "10.13.13.0/24" {
		t.Errorf("AllowedIPs = %v", p.AllowedIPs)
	}

	if _, ok := p.HandshakeAge(); !ok {
		t.Error("HandshakeAge should report a handshake")
	}
}

// TestParseStatusNoHandshake is the state that matters operationally: the device
// is configured and running but has never reached the peer.
func TestParseStatusNoHandshake(t *testing.T) {
	raw := strings.Join([]string{
		"private_key=" + strings.Repeat("11", 32),
		"public_key=" + strings.Repeat("22", 32),
		"last_handshake_time_sec=0",
		"last_handshake_time_nsec=0",
		"errno=0",
		"",
	}, "\n")

	st, err := parseStatus(raw)
	if err != nil {
		t.Fatalf("parseStatus: %v", err)
	}
	if len(st.Peers) != 1 {
		t.Fatalf("got %d peers, want 1", len(st.Peers))
	}
	if !st.Peers[0].LastHandshake.IsZero() {
		t.Errorf("LastHandshake = %v, want the zero time", st.Peers[0].LastHandshake)
	}
	if _, ok := st.Peers[0].HandshakeAge(); ok {
		t.Error("HandshakeAge should report that there has never been one")
	}
}

func TestParseStatusTwoPeers(t *testing.T) {
	raw := strings.Join([]string{
		"private_key=" + strings.Repeat("11", 32),
		"public_key=" + strings.Repeat("22", 32),
		"tx_bytes=1",
		"public_key=" + strings.Repeat("33", 32),
		"tx_bytes=2",
		"errno=0",
		"",
	}, "\n")

	st, err := parseStatus(raw)
	if err != nil {
		t.Fatalf("parseStatus: %v", err)
	}
	if len(st.Peers) != 2 {
		t.Fatalf("got %d peers, want 2", len(st.Peers))
	}
	if st.Peers[0].TxBytes != 1 || st.Peers[1].TxBytes != 2 {
		t.Errorf("counters did not stay with their peer: %d, %d", st.Peers[0].TxBytes, st.Peers[1].TxBytes)
	}
}

func TestParseStatusErrno(t *testing.T) {
	if _, err := parseStatus("errno=1\n"); err == nil {
		t.Error("parseStatus should fail on a non-zero errno")
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex: %v", err)
	}
	return b
}
