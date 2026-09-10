package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/smb"
)

// cmdGet is M0 step 5: pull a real, multi-gigabyte file off the share.
//
// -chunks is the question behind it. One TCP stream over a high-BDP link with any
// loss will not fill the pipe, so the transfer engine is designed for two to four
// bounded streams per file; this is where that gets measured against DSM rather
// than assumed (DESIGN.md §2.4).
func cmdGet(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("get")
	path := fs.String("path", "", "remote file, relative to the remote root")
	out := fs.String("out", "", "write here; empty discards the bytes and only measures throughput")
	offset := fs.Int64("offset", 0, "start at this byte offset")
	length := fs.Int64("length", 0, "transfer at most this many bytes, 0 for the rest of the file")
	chunks := fs.Int("chunks", 1, "parallel ranged streams; 1 reproduces strictly sequential behaviour")
	hash := fs.Bool("hash", false, "sha256 the transferred bytes (needs -out when -chunks > 1)")
	progress := fs.Duration("progress", 5*time.Second, "progress interval, 0 to disable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return fmt.Errorf("missing -path")
	}
	if *chunks < 1 {
		return fmt.Errorf("-chunks must be at least 1")
	}
	if *chunks > 1 && *hash && *out == "" {
		return fmt.Errorf("-hash with -chunks > 1 needs -out: the chunks arrive out of order and there is nothing to hash in sequence")
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
	logger.Infof("get: %s is %s, mtime %s", entry.Path, format.Bytes(entry.Size), entry.MTime.Format(time.RFC3339))

	span := *length
	if span <= 0 {
		span = entry.Size - *offset
	}
	if span <= 0 {
		return fmt.Errorf("nothing to transfer: offset %d in a %d byte file", *offset, entry.Size)
	}

	var file *os.File
	if *out != "" {
		file, err = os.Create(*out)
		if err != nil {
			return fmt.Errorf("create %q: %w", *out, err)
		}
		defer file.Close()
	}

	var (
		done atomic.Int64
		last atomic.Int64
	)
	stop := reportBytes(ctx, logger, "get", &done, span, *progress)

	start := time.Now()
	sum, err := fetch(ctx, client, *path, *offset, span, *chunks, file, *hash && *chunks == 1, &done, &last)
	elapsed := time.Since(start)
	stop()

	if err != nil {
		return err
	}

	logger.Infof("%s", transferSummary("get", done.Load(), elapsed))
	if got := done.Load(); got != span {
		return fmt.Errorf("short transfer: %d bytes of %d - a size check is the only truncation defence there is (DESIGN.md §2.3)", got, span)
	}

	if *hash {
		if sum == "" {
			sum, err = hashFile(*out)
			if err != nil {
				return err
			}
		}
		logger.Infof("get: sha256 %s", sum)
	}
	return nil
}

// fetch transfers [offset, offset+span) using n parallel ranged streams. inline
// hashing is only possible with a single stream, where the bytes arrive in order.
func fetch(ctx context.Context, client *smb.Client, path string, offset, span int64, n int, file *os.File, inlineHash bool, done, last *atomic.Int64) (string, error) {
	// A range shorter than the stream count would produce empty chunks, and an
	// empty length means "to the end of the file" - each of them would then pull
	// the whole file.
	if n == 1 || span < int64(n) {
		return fetchChunk(ctx, client, path, offset, span, offset, file, inlineHash, done, last)
	}

	size := span / int64(n)
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for i := 0; i < n; i++ {
		from := offset + int64(i)*size
		count := size
		if i == n-1 {
			count = offset + span - from
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := fetchChunk(ctx, client, path, from, count, offset, file, false, done, last); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		return "", fmt.Errorf("chunked get: %d of %d streams failed: %w", len(errs), n, errs[0])
	}
	return "", nil
}

// fetchChunk transfers one range. base is the offset the output file starts at, so
// a chunk lands at the right place in it.
func fetchChunk(ctx context.Context, client *smb.Client, path string, from, count, base int64, file *os.File, inlineHash bool, done, last *atomic.Int64) (string, error) {
	body, _, err := client.OpenRange(ctx, path, from, count)
	if err != nil {
		return "", err
	}
	defer body.Close()

	var src io.Reader = countingReader{r: body, bytes: done, last: last}

	hasher := sha256.New()
	if inlineHash {
		src = io.TeeReader(src, hasher)
	}

	var dst io.Writer = io.Discard
	if file != nil {
		// WriteAt rather than Write: parallel chunks share the file and each one
		// owns its own region of it.
		dst = &sectionWriter{f: file, off: from - base}
	}

	if _, err := io.Copy(dst, src); err != nil {
		return "", fmt.Errorf("get %q at %d: %w", path, from, err)
	}
	if inlineHash {
		return hex.EncodeToString(hasher.Sum(nil)), nil
	}
	return "", nil
}

// sectionWriter writes sequentially into a file starting at a fixed offset.
type sectionWriter struct {
	f   *os.File
	off int64
}

func (w *sectionWriter) Write(p []byte) (int, error) {
	n, err := w.f.WriteAt(p, w.off)
	w.off += int64(n)
	return n, err
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
