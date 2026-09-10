package main

import (
	"context"
	"fmt"

	"blackforestbytes.com/jcc-mirror/wg"
)

// cmdPubkey derives the public key belonging to the configured private one. It
// is what the WireGuard server's peer entry for this container should hold, so
// it is worth comparing after importing a config; it needs no tunnel and no
// network.
func cmdPubkey(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("pubkey")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := cfg.fromStore(ctx); err != nil {
		return err
	}
	if cfg.wgPrivateKey == "" {
		return fmt.Errorf("missing -wg-key, and no -data directory holding one")
	}

	pub, err := wg.PublicKey(cfg.wgPrivateKey)
	if err != nil {
		return err
	}
	fmt.Println(pub)
	return nil
}
