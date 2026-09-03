package wg

import (
	"encoding/base64"
	"net/netip"
	"strings"
	"testing"
)

var (
	testPrivate = base64.StdEncoding.EncodeToString(bytesOf(0x11))
	testPeer    = base64.StdEncoding.EncodeToString(bytesOf(0x22))
)

func bytesOf(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestParseAddrs(t *testing.T) {
	// The /prefix form is accepted so a wg-quick "Address = 10.13.13.3/32" can be
	// pasted straight into the environment.
	got, err := parseAddrs("10.13.13.3/32, fd00::3 ,")
	if err != nil {
		t.Fatalf("parseAddrs: %v", err)
	}
	if len(got) != 2 || got[0] != netip.MustParseAddr("10.13.13.3") || got[1] != netip.MustParseAddr("fd00::3") {
		t.Errorf("parseAddrs = %v", got)
	}

	if _, err := parseAddrs("not-an-address"); err == nil {
		t.Error("parseAddrs should reject nonsense")
	}
}

func TestParsePrefixes(t *testing.T) {
	got, err := parsePrefixes("10.13.13.0/24, 10.20.0.5")
	if err != nil {
		t.Fatalf("parsePrefixes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d prefixes, want 2", len(got))
	}
	if got[0] != netip.MustParsePrefix("10.13.13.0/24") {
		t.Errorf("prefix = %v", got[0])
	}
	if got[1] != netip.MustParsePrefix("10.20.0.5/32") {
		t.Errorf("a bare address should become a host route, got %v", got[1])
	}
}

func TestResolveEndpoint(t *testing.T) {
	got, err := resolveEndpoint("203.0.113.7:51820")
	if err != nil {
		t.Fatalf("resolveEndpoint: %v", err)
	}
	if got != netip.MustParseAddrPort("203.0.113.7:51820") {
		t.Errorf("resolveEndpoint = %v", got)
	}

	if _, err := resolveEndpoint(""); err == nil {
		t.Error("resolveEndpoint should reject an empty endpoint")
	}
	if _, err := resolveEndpoint("203.0.113.7"); err == nil {
		t.Error("resolveEndpoint should reject a missing port")
	}
}

func TestUapiConfig(t *testing.T) {
	cfg := Config{
		PrivateKey:    testPrivate,
		PeerPublicKey: testPeer,
		Keepalive:     DefaultKeepalive,
	}
	got, err := uapiConfig(cfg, netip.MustParseAddrPort("203.0.113.7:51820"), []netip.Prefix{netip.MustParsePrefix("10.13.13.0/24")})
	if err != nil {
		t.Fatalf("uapiConfig: %v", err)
	}

	for _, want := range []string{
		"private_key=" + strings.Repeat("11", 32),
		"public_key=" + strings.Repeat("22", 32),
		"endpoint=203.0.113.7:51820",
		"persistent_keepalive_interval=25",
		"allowed_ip=10.13.13.0/24",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("uapi config is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "preshared_key") {
		t.Error("no preshared key was configured, so none should be sent")
	}
}

func TestUapiConfigRejectsBadKeys(t *testing.T) {
	_, err := uapiConfig(Config{PrivateKey: "obviously not base64!", PeerPublicKey: testPeer}, netip.MustParseAddrPort("203.0.113.7:51820"), nil)
	if err == nil {
		t.Error("uapiConfig should reject an unparsable private key")
	}

	short := base64.StdEncoding.EncodeToString([]byte("too short"))
	if _, err := uapiConfig(Config{PrivateKey: short, PeerPublicKey: testPeer}, netip.MustParseAddrPort("203.0.113.7:51820"), nil); err == nil {
		t.Error("uapiConfig should reject a key that is not 32 bytes")
	}
}

func TestPublicKey(t *testing.T) {
	pub, err := PublicKey(testPrivate)
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(pub)
	if err != nil || len(raw) != 32 {
		t.Errorf("PublicKey = %q, want 32 base64 bytes", pub)
	}
	if pub == testPrivate {
		t.Error("PublicKey returned the private key")
	}
}
