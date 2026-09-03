package main

import (
	"context"
	"fmt"

	"blackforestbytes.com/jcc-mirror/wg"
)

// cmdPubkey derives the public key belonging to -wg-key. It is the one
// value the rootserver needs in order to add this container as a peer, and it
// needs no tunnel and no network.
func cmdPubkey(_ context.Context, args []string) error {
	fs, cfg := newFlagSet("pubkey")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if cfg.wgPrivateKey == "" {
		return fmt.Errorf("missing -wg-key (generate one with `wg genkey`)")
	}

	pub, err := wg.PublicKey(cfg.wgPrivateKey)
	if err != nil {
		return err
	}
	fmt.Println(pub)
	return nil
}
