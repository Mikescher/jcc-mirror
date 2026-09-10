package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/smb"
)

// cmdResume is the other half of M0 step 5, and the one that decides whether the
// transfer engine can exist as designed: abort a read mid-file, reopen at the
// byte offset reached, and prove the result is identical to a straight download.
//
// Everything else rests on this. Reading from an offset is what makes a transfer
// survive a restart, a self-update and a closed transfer window; without it a
// 40 GB file interrupted at 39 GB starts again from zero (DESIGN.md §2.4).
func cmdResume(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("resume")
	path := fs.String("path", "", "remote file, relative to the remote root")
	offset := fs.Int64("offset", 0, "start of the region to test")
	length := fs.Int64("length", 64<<20, "size of the region to test")
	abortAt := fs.Int64("abort-at", 0, "abort the first stream after this many bytes, 0 for half of -length")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return fmt.Errorf("missing -path")
	}

	logger := &logs.Logger{Verbose: cfg.verbose}
	client, _, closeFn, err := cfg.openBoth(ctx, logger)
	if err != nil {
		return err
	}
	defer closeFn()

	entry, err := client.Stat(ctx, *path)
	if err != nil {
		return err
	}

	span := *length
	if span <= 0 || *offset+span > entry.Size {
		span = entry.Size - *offset
	}
	if span <= 0 {
		return fmt.Errorf("nothing to test: offset %d in a %d byte file", *offset, entry.Size)
	}
	cut := *abortAt
	if cut <= 0 || cut >= span {
		cut = span / 2
	}
	logger.Infof("resume: %s, testing [%d, %d), aborting after %s", entry.Path, *offset, *offset+span, format.Bytes(cut))

	// Reference: the same region in one uninterrupted stream.
	start := time.Now()
	want, err := hashRange(ctx, client, *path, *offset, span)
	if err != nil {
		return fmt.Errorf("reference download: %w", err)
	}
	logger.Infof("%s", transferSummary("resume: reference", span, time.Since(start)))

	// Interrupted: read cut bytes, drop the connection without draining it, then
	// pick up exactly where it stopped.
	start = time.Now()
	h := sha256.New()

	first, _, err := client.OpenRange(ctx, *path, *offset, span)
	if err != nil {
		return err
	}
	n, err := io.CopyN(h, first, cut)
	first.Close()
	if err != nil {
		return fmt.Errorf("first leg: %w", err)
	}
	logger.Infof("resume: dropped the connection after %s", format.Bytes(n))

	second, _, err := client.OpenRange(ctx, *path, *offset+n, span-n)
	if err != nil {
		return fmt.Errorf("second leg: %w", err)
	}
	m, err := io.Copy(h, second)
	second.Close()
	if err != nil {
		return fmt.Errorf("second leg: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	logger.Infof("%s", transferSummary("resume: interrupted", n+m, time.Since(start)))

	if n+m != span {
		return fmt.Errorf("resume transferred %d bytes, want %d", n+m, span)
	}
	if got != want {
		return fmt.Errorf("resume produced different bytes:\n  straight    %s\n  interrupted %s\nranged resume is not usable against this server", want, got)
	}

	logger.Infof("resume: identical, sha256 %s", got)
	logger.Infof("=> ranged resume works; the transfer engine can restart mid-file")
	return nil
}

// hashRange downloads a byte range and returns its sha256, keeping nothing.
func hashRange(ctx context.Context, client *smb.Client, path string, offset, length int64) (string, error) {
	body, _, err := client.OpenRange(ctx, path, offset, length)
	if err != nil {
		return "", err
	}
	defer body.Close()

	h := sha256.New()
	n, err := io.Copy(h, body)
	if err != nil {
		return "", err
	}
	if n != length {
		return "", fmt.Errorf("read %d bytes, want %d", n, length)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
