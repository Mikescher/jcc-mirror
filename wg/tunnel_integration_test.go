package wg

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// TestTunnelEndToEnd runs a real WireGuard handshake between two userspace
// devices over loopback UDP. It covers the parts that cannot be checked by
// inspection - base64-to-hex key conversion, the IPC config, ping over the
// netstack ICMP socket, and HTTP through the transport - without needing a
// rootserver or a second machine.
//
// The counterparty is built with wireguard-go directly rather than through Open,
// because Open is the thing under test.
func TestTunnelEndToEnd(t *testing.T) {
	serverPriv, serverPub := genKeypair(t)
	clientPriv, clientPub := genKeypair(t)

	const (
		serverAddr = "10.13.13.1"
		clientAddr = "10.13.13.2"
	)
	port := freeUDPPort(t)

	serverNet := startPeer(t, serverPriv, clientPub, serverAddr, clientAddr+"/32", port)

	// A listener on the far side, which is what `serve` relies on: the publisher
	// reaching the dashboard over the tunnel with nothing configured on his side.
	ln, err := serverNet.ListenTCP(&net.TCPAddr{Port: 8080})
	if err != nil {
		t.Fatalf("listen inside the tunnel: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello from the publisher")
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	tun, err := Open(Config{
		PrivateKey:    clientPriv,
		PeerPublicKey: serverPub,
		Endpoint:      fmt.Sprintf("127.0.0.1:%d", port),
		Addresses:     clientAddr + "/32",
		AllowedIPs:    "10.13.13.0/24",
		Keepalive:     DefaultKeepalive,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer tun.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := tun.WaitHandshake(ctx); err != nil {
		t.Fatalf("WaitHandshake: %v", err)
	}

	st, err := tun.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(st.Peers) != 1 {
		t.Fatalf("got %d peers, want 1", len(st.Peers))
	}
	if st.Peers[0].PublicKey != serverPub {
		t.Errorf("peer key = %q, want %q", st.Peers[0].PublicKey, serverPub)
	}
	if _, ok := st.Peers[0].HandshakeAge(); !ok {
		t.Error("the peer should have a handshake after WaitHandshake returned")
	}

	rtt, err := tun.Ping(ctx, netip.MustParseAddr(serverAddr), 5*time.Second)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	t.Logf("ping rtt over loopback: %v", rtt)

	client := &http.Client{Transport: tun.Transport()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+serverAddr+":8080/", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("http through the tunnel: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "hello from the publisher" {
		t.Errorf("body = %q", body)
	}
}

// startPeer brings up the far side of the tunnel and returns its network stack.
func startPeer(t *testing.T, privateKey, peerPublicKey, addr, peerAllowedIP string, port int) *netstack.Net {
	t.Helper()

	tun, tnet, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr(addr)}, nil, DefaultMTU)
	if err != nil {
		t.Fatalf("create peer tun: %v", err)
	}

	dev := device.NewDevice(tun, conn.NewDefaultBind(), &device.Logger{
		Verbosef: func(string, ...any) {},
		Errorf:   func(f string, a ...any) { t.Logf("peer device: "+f, a...) },
	})
	t.Cleanup(dev.Close)

	priv, err := hexKey(privateKey, "private key")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	pub, err := hexKey(peerPublicKey, "peer public key")
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	uapi := fmt.Sprintf("private_key=%s\nlisten_port=%d\npublic_key=%s\nallowed_ip=%s\n", priv, port, pub, peerAllowedIP)
	if err := dev.IpcSet(uapi); err != nil {
		t.Fatalf("configure peer: %v", err)
	}
	if err := dev.Up(); err != nil {
		t.Fatalf("bring peer up: %v", err)
	}
	return tnet
}

func genKeypair(t *testing.T) (privB64, pubB64 string) {
	t.Helper()

	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	k[0] &= 248
	k[31] &= 127
	k[31] |= 64

	pub, err := curve25519.X25519(k[:], curve25519.Basepoint)
	if err != nil {
		t.Fatalf("derive public key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(k[:]), base64.StdEncoding.EncodeToString(pub)
}

// freeUDPPort picks a port the WireGuard device can bind. There is a race between
// closing this socket and the device binding it, which in a test is acceptable.
func freeUDPPort(t *testing.T) int {
	t.Helper()

	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a port: %v", err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}
