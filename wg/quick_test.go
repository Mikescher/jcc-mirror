package wg

import (
	"encoding/base64"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

var testPSK = base64.StdEncoding.EncodeToString(bytesOf(0x33))

// serverConfig is the shape a WireGuard server hands out: every field filled in,
// a default route, and a preshared key. It is the file this parser exists for, so
// it is what the happy path is measured against.
var serverConfig = `[Interface]
PrivateKey = ` + testPrivate + `
Address = 10.13.13.3/32, fd00::3/128
DNS = 10.13.13.1
MTU = 1420

[Peer]
PublicKey = ` + testPeer + `
PresharedKey = ` + testPSK + `
AllowedIPs = 0.0.0.0/0, ::/0
Endpoint = vpn.example.com:51820
PersistentKeepalive = 25
`

// The minimal halves the error cases are assembled from.
var (
	quickHead = "[Interface]\nPrivateKey = " + testPrivate + "\nAddress = 10.0.0.2/32\n"
	quickPeer = "[Peer]\nPublicKey = " + testPeer + "\nAllowedIPs = 10.0.0.0/24\nEndpoint = 1.2.3.4:1\n"
)

func TestParseQuick(t *testing.T) {
	got, err := ParseQuick(serverConfig)
	if err != nil {
		t.Fatalf("ParseQuick: %v", err)
	}
	if len(got.Ignored) != 0 {
		t.Errorf("Ignored = %v, want nothing", got.Ignored)
	}

	want := Config{
		PrivateKey:    testPrivate,
		PeerPublicKey: testPeer,
		PresharedKey:  testPSK,
		Endpoint:      "vpn.example.com:51820",
		Addresses:     "10.13.13.3/32, fd00::3/128",
		AllowedIPs:    "0.0.0.0/0, ::/0",
		DNS:           "10.13.13.1",
		Keepalive:     25,
		MTU:           1420,
	}
	// DeepEqual rather than ==: Config carries a log function, which ParseQuick
	// leaves nil and the compiler will not compare.
	if !reflect.DeepEqual(got.Config, want) {
		t.Errorf("Config = %+v\nwant %+v", got.Config, want)
	}
}

// A file that imports cleanly and then will not open a tunnel is the failure this
// whole feature exists to avoid, so the parsed config is fed to the same
// functions Open uses.
func TestParseQuickFeedsTheTunnel(t *testing.T) {
	got, err := ParseQuick(serverConfig)
	if err != nil {
		t.Fatalf("ParseQuick: %v", err)
	}
	if _, err := parseAddrs(got.Config.Addresses); err != nil {
		t.Errorf("parseAddrs: %v", err)
	}
	if _, err := parsePrefixes(got.Config.AllowedIPs); err != nil {
		t.Errorf("parsePrefixes: %v", err)
	}
	if _, err := parseAddrs(got.Config.DNS); err != nil {
		t.Errorf("parseAddrs(DNS): %v", err)
	}

	uapi, err := uapiConfig(got.Config, netip.MustParseAddrPort("203.0.113.7:51820"), nil)
	if err != nil {
		t.Fatalf("uapiConfig: %v", err)
	}
	if !strings.Contains(uapi, "preshared_key=") {
		t.Error("the preshared key from the file did not reach the device")
	}
}

func TestParseQuickShapes(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		check func(*testing.T, Quick)
	}{
		{
			name: "case insensitive",
			text: "[INTERFACE]\nprivatekey=" + testPrivate + "\nADDRESS=10.0.0.2/32\n" +
				"[peer]\nPUBLICkey=" + testPeer + "\nallowedips=10.0.0.0/24\nENDPOINT=1.2.3.4:1\n",
			check: func(t *testing.T, q Quick) {
				if q.Config.PrivateKey != testPrivate || q.Config.Endpoint != "1.2.3.4:1" {
					t.Errorf("Config = %+v", q.Config)
				}
			},
		},
		{
			name: "comments, blanks and CRLF",
			text: "# a config the server mailed over\r\n\r\n[Interface] ; the local half\r\n" +
				"PrivateKey   =   " + testPrivate + "   # keep this one secret\r\n" +
				"Address = 10.0.0.2/32\r\n\r\n[Peer]\r\nPublicKey=" + testPeer + "\r\n" +
				"AllowedIPs = 10.0.0.0/24\r\nEndpoint = 1.2.3.4:1\r\n",
			check: func(t *testing.T, q Quick) {
				if q.Config.PrivateKey != testPrivate {
					t.Errorf("PrivateKey = %q", q.Config.PrivateKey)
				}
				if q.Config.Addresses != "10.0.0.2/32" {
					t.Errorf("Addresses = %q", q.Config.Addresses)
				}
			},
		},
		{
			name: "host directives are dropped, not refused",
			text: "[Interface]\nPrivateKey = " + testPrivate + "\nAddress = 10.0.0.2/32\n" +
				"ListenPort = 51820\nTable = off\nPostUp = iptables -A FORWARD -j ACCEPT\nSaveConfig = true\n" +
				"FwMark = 0x1234\nPreUp = true\nPreDown = true\nPostDown = true\n" + quickPeer,
			check: func(t *testing.T, q Quick) {
				want := "ListenPort,Table,PostUp,SaveConfig,FwMark,PreUp,PreDown,PostDown"
				if got := strings.Join(q.Ignored, ","); got != want {
					t.Errorf("Ignored = %q, want %q", got, want)
				}
				if q.Config.PrivateKey == "" {
					t.Error("the rest of the file was not parsed")
				}
			},
		},
		{
			name: "defaults for what the file leaves out",
			text: quickHead + quickPeer,
			check: func(t *testing.T, q Quick) {
				if q.Config.MTU != DefaultMTU {
					t.Errorf("MTU = %d, want the default %d", q.Config.MTU, DefaultMTU)
				}
				if q.Config.Keepalive != DefaultKeepalive {
					t.Errorf("Keepalive = %d, want the default %d", q.Config.Keepalive, DefaultKeepalive)
				}
				if q.Config.DNS != "" || q.Config.PresharedKey != "" {
					t.Errorf("optional fields were invented: %+v", q.Config)
				}
			},
		},
		{
			name: "keepalive off stays off",
			text: quickHead + quickPeer + "PersistentKeepalive = off\n",
			check: func(t *testing.T, q Quick) {
				if q.Config.Keepalive != 0 {
					t.Errorf(`Keepalive = %d; "off" must not be replaced by the default`, q.Config.Keepalive)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseQuick(c.text)
			if err != nil {
				t.Fatalf("ParseQuick: %v", err)
			}
			c.check(t, got)
		})
	}
}

func TestParseQuickErrors(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string // a fragment of the message the dashboard shows verbatim
	}{
		{"empty", "", "PrivateKey"},
		{"no private key", "[Interface]\nAddress = 10.0.0.2/32\n" + quickPeer, "[Interface] PrivateKey"},
		{"no address", "[Interface]\nPrivateKey = " + testPrivate + "\n" + quickPeer, "[Interface] Address"},
		{"no peer key", quickHead + "[Peer]\nAllowedIPs = 10.0.0.0/24\nEndpoint = 1.2.3.4:1\n", "[Peer] PublicKey"},
		{"no endpoint", quickHead + "[Peer]\nPublicKey = " + testPeer + "\nAllowedIPs = 10.0.0.0/24\n", "[Peer] Endpoint"},
		{"no allowed ips", quickHead + "[Peer]\nPublicKey = " + testPeer + "\nEndpoint = 1.2.3.4:1\n", "[Peer] AllowedIPs"},
		{"two peers", quickHead + quickPeer + quickPeer, "more than one [Peer]"},
		{"unknown section", quickHead + "[Router]\nx = 1\n" + quickPeer, "[router]"},
		{"unknown interface key", "[Interface]\nPrivateKey = " + testPrivate + "\nAdress = 10.0.0.2/32\n" + quickPeer, `"Adress"`},
		{"unknown peer key", quickHead + quickPeer + "Keepalive = 25\n", `"Keepalive"`},
		{"stray line", "PrivateKey = " + testPrivate + "\n", "before any [Interface]"},
		{"no equals", quickHead + "Address\n" + quickPeer, "Key = Value"},
		{"unterminated section", "[Interface\n", "section header"},
		{"mtu is not a number", "[Interface]\nPrivateKey = " + testPrivate + "\nAddress = 10.0.0.2/32\nMTU = big\n" + quickPeer, "MTU"},
		{"keepalive is not a number", quickHead + quickPeer + "PersistentKeepalive = soon\n", "PersistentKeepalive"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseQuick(c.text)
			if err == nil {
				t.Fatal("ParseQuick accepted it")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to name %q", err, c.want)
			}
		})
	}
}
