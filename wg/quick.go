package wg

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Quick is the result of reading a wg-quick file: the tunnel it describes, plus
// the directives that were understood well enough to be recognised and then
// dropped. Ignored is carried out of the parser rather than logged inside it,
// because the person who pasted the file is the one who needs to hear that half
// of it did nothing.
type Quick struct {
	Config  Config
	Ignored []string
}

// ignoredInterfaceKeys are the [Interface] directives wg-quick honours by driving
// the host - a routing table, a firewall mark, shell hooks - and this daemon
// cannot: the tunnel lives in a userspace netstack with no host side to touch.
// They are dropped rather than refused, because a server-generated file carries
// them and rejecting it over one would defeat the point of pasting it in.
var ignoredInterfaceKeys = map[string]bool{
	"listenport": true,
	"table":      true,
	"preup":      true,
	"postup":     true,
	"predown":    true,
	"postdown":   true,
	"saveconfig": true,
	"fwmark":     true,
}

// ParseQuick parses a wg-quick / WireGuard client config file.
func ParseQuick(text string) (Quick, error) {
	var (
		out     Quick
		section string
		peers   int
		seenKA  bool
	)

	for n, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if i := strings.IndexAny(line, "#;"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		lineNo := n + 1

		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return Quick{}, fmt.Errorf("line %d: %q is not a section header", lineNo, line)
			}
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			switch section {
			case "interface":
			case "peer":
				peers++
				if peers > 1 {
					return Quick{}, errors.New("this config has more than one [Peer]; jcc-mirror talks to exactly one WireGuard server, so import a config with a single peer")
				}
			default:
				return Quick{}, fmt.Errorf("line %d: unknown section [%s]", lineNo, section)
			}
			continue
		}

		name, value, ok := strings.Cut(line, "=")
		if !ok {
			return Quick{}, fmt.Errorf("line %d: %q is not a Key = Value line", lineNo, line)
		}
		key := strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)

		switch section {
		case "interface":
			switch key {
			case "privatekey":
				out.Config.PrivateKey = value
			case "address":
				out.Config.Addresses = value
			case "dns":
				out.Config.DNS = value
			case "mtu":
				mtu, err := strconv.Atoi(value)
				if err != nil {
					return Quick{}, fmt.Errorf("line %d: MTU %q is not a number", lineNo, value)
				}
				out.Config.MTU = mtu
			default:
				if !ignoredInterfaceKeys[key] {
					return Quick{}, fmt.Errorf("line %d: unknown [Interface] key %q", lineNo, strings.TrimSpace(name))
				}
				out.Ignored = append(out.Ignored, strings.TrimSpace(name))
			}

		case "peer":
			switch key {
			case "publickey":
				out.Config.PeerPublicKey = value
			case "presharedkey":
				out.Config.PresharedKey = value
			case "allowedips":
				out.Config.AllowedIPs = value
			case "endpoint":
				out.Config.Endpoint = value
			case "persistentkeepalive":
				// "off" is what wg-quick writes for a peer that should not keep the
				// NAT mapping open, and 0 means the same thing to wireguard-go.
				if strings.EqualFold(value, "off") {
					out.Config.Keepalive, seenKA = 0, true
					break
				}
				ka, err := strconv.Atoi(value)
				if err != nil {
					return Quick{}, fmt.Errorf("line %d: PersistentKeepalive %q is not a number of seconds", lineNo, value)
				}
				out.Config.Keepalive, seenKA = ka, true
			default:
				return Quick{}, fmt.Errorf("line %d: unknown [Peer] key %q", lineNo, strings.TrimSpace(name))
			}

		default:
			return Quick{}, fmt.Errorf("line %d: %q comes before any [Interface] or [Peer] section", lineNo, line)
		}
	}

	for _, missing := range []struct {
		field string
		value string
	}{
		{"[Interface] PrivateKey", out.Config.PrivateKey},
		{"[Interface] Address", out.Config.Addresses},
		{"[Peer] PublicKey", out.Config.PeerPublicKey},
		{"[Peer] Endpoint", out.Config.Endpoint},
		{"[Peer] AllowedIPs", out.Config.AllowedIPs},
	} {
		if strings.TrimSpace(missing.value) == "" {
			return Quick{}, fmt.Errorf("this config has no %s", missing.field)
		}
	}

	if out.Config.MTU == 0 {
		out.Config.MTU = DefaultMTU
	}
	if !seenKA {
		// Both ends of this topology sit behind NAT, so a file that says nothing
		// about the keepalive gets one anyway rather than a tunnel that only works
		// in the direction it last sent.
		out.Config.Keepalive = DefaultKeepalive
	}
	return out, nil
}
