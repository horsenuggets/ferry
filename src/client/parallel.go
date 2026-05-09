package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
)

// MaxParallelWorkers is the upper bound for --parallel. Above this we lose
// connection-pool efficiency and start contending on the server's locker
// even for unrelated uploads. 16 is comfortably above the practical sweet
// spot (typically 4-8 on a high-RTT link).
const MaxParallelWorkers = 16

// uploadParallel splits the file into n slabs, POSTs n partial uploads,
// PATCHes each slab on its own worker goroutine, then POSTs an
// Upload-Concat: final tying them together. Returns the final upload's
// result.
//
// All workers share the supplied http.Client (its connection pool); the
// per-partial PATCH loops use the same retry/backoff machinery as the
// sequential path.
func (c *Client) uploadParallel(ctx context.Context, filePath string, opts UploadOptions, n int) (*UploadResult, error) {
	if n < 1 {
		return nil, fmt.Errorf("parallel must be >= 1")
	}
	if n > MaxParallelWorkers {
		return nil, fmt.Errorf("parallel must be <= %d", MaxParallelWorkers)
	}

	// Stat the file once: workers each open their own descriptor so seeking
	// doesn't contend.
	st, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("stat file: %w", err)
	}
	if st.IsDir() {
		return nil, fmt.Errorf("%s is a directory", filePath)
	}
	totalSize := st.Size()
	algo := opts.Checksum
	if algo == "" {
		algo = checksumAlgoCRC32C
	}
	switch algo {
	case checksumAlgoCRC32C, checksumAlgoSHA256, checksumAlgoNone:
	default:
		return nil, fmt.Errorf("unsupported checksum algo %q (want crc32c|sha256|none)", algo)
	}

	startTime := time.Now()
	remoteName := opts.RemoteName
	if remoteName == "" {
		remoteName = filepath.Base(filePath)
	}

	// Tiny files don't benefit from parallelism: fall back to a single
	// sequential partial+final dance, which is still slightly more overhead
	// than a regular upload but lets the rest of the workflow stay uniform.
	// For n=1 OR file smaller than n bytes, send everything as one slab.
	slabs := splitSlabs(totalSize, n)

	// progress aggregates across all workers. Progress is not goroutine-
	// safe, so we serialize updates with progMu.
	var progMu sync.Mutex
	addProgress := func(delta int64) {
		if delta == 0 || opts.Progress == nil {
			return
		}
		progMu.Lock()
		opts.Progress.Add(delta)
		progMu.Unlock()
	}

	// 1. Create N partial uploads up front so we know all the URLs before
	//    we start sending bytes. (Doing it serially keeps the create
	//    requests off the wire fast path; it's just N round trips.)
	partialURLs := make([]string, len(slabs))
	for i, sl := range slabs {
		loc, err := c.createPartial(ctx, opts.Namespace, sl.length, remoteName)
		if err != nil {
			return nil, fmt.Errorf("create partial %d: %w", i, err)
		}
		partialURLs[i] = c.absoluteURL(loc)
	}

	// 2. Run N PATCH workers, one per slab.
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errCh := make(chan error, len(slabs))
	for i, sl := range slabs {
		i, sl := i, sl
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.uploadSlab(gctx, filePath, sl, partialURLs[i], opts.ChunkSize, algo, !opts.NoAdaptiveChunks, addProgress); err != nil {
				select {
				case errCh <- fmt.Errorf("slab %d: %w", i, err):
				default:
				}
				cancel()
			}
		}()
	}
	wg.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		return nil, err
	}

	// 3. POST the final concatenation. The server validates each partial,
	//    stitches, and returns the new upload's Location.
	finalURL, finalSize, err := c.postFinalConcat(ctx, opts.Namespace, partialURLs, remoteName)
	if err != nil {
		return nil, fmt.Errorf("post final concat: %w", err)
	}
	if finalSize != totalSize {
		return nil, fmt.Errorf("final size mismatch: server %d, file %d", finalSize, totalSize)
	}

	// 4. HEAD the final to confirm state. Mostly belt-and-suspenders: the
	//    POST already returned the size, but tus convention is to verify.
	off, err := c.headOffset(ctx, finalURL)
	if err != nil {
		return nil, fmt.Errorf("head final: %w", err)
	}
	if off != totalSize {
		return nil, fmt.Errorf("head final offset %d != size %d", off, totalSize)
	}

	res := &UploadResult{
		UploadID:    pathID(finalURL),
		LocationURL: finalURL,
		Size:        totalSize,
		Duration:    time.Since(startTime),
	}
	if opts.Progress != nil {
		opts.Progress.Done(res.UploadID, res.LocationURL)
	}
	return res, nil
}

// slab describes one worker's byte range.
type slab struct {
	start  int64
	length int64
}

// splitSlabs divides total bytes into n contiguous, non-overlapping slabs.
// The last slab may be smaller. Empty file produces a single 0-length slab
// so callers can still drive a single create+final round trip uniformly.
func splitSlabs(total int64, n int) []slab {
	if n < 1 {
		n = 1
	}
	if total <= 0 {
		return []slab{{0, 0}}
	}
	// If n exceeds total bytes, downshift so we don't make zero-length
	// trailing slabs (which the server accepts but waste round trips).
	if int64(n) > total {
		n = int(total)
	}
	base := total / int64(n)
	rem := total % int64(n)
	out := make([]slab, n)
	var off int64
	for i := 0; i < n; i++ {
		ln := base
		if int64(i) < rem {
			ln++
		}
		out[i] = slab{start: off, length: ln}
		off += ln
	}
	return out
}

// createPartial sends POST with Upload-Concat: partial and returns the
// Location header. Same retry policy as the sequential createUpload.
func (c *Client) createPartial(ctx context.Context, namespace string, size int64, remoteName string) (string, error) {
	url := fmt.Sprintf("%s/v1/uploads/%s", c.BaseURL, namespace)
	var location string
	doOnce := func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
		if err != nil {
			return backoff.Permanent(err)
		}
		req.Header.Set(headerTusResume, tusVersion)
		req.Header.Set(headerAuth, "Bearer "+c.Token)
		req.Header.Set(headerUploadLen, strconv.FormatInt(size, 10))
		req.Header.Set("Upload-Concat", "partial")
		if remoteName != "" {
			meta := "filename " + base64.StdEncoding.EncodeToString([]byte(remoteName))
			req.Header.Set(headerUploadMeta, meta)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if !IsRetryable(err) {
				return backoff.Permanent(err)
			}
			return err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode != http.StatusCreated {
			_, _, perr := classifyResponse(resp.StatusCode, string(body))
			if isPermanent(perr) {
				return backoff.Permanent(perr)
			}
			return perr
		}
		loc := resp.Header.Get(headerLocation)
		if loc == "" {
			return backoff.Permanent(errors.New("partial create returned no Location"))
		}
		location = loc
		return nil
	}
	bo := c.NewBackoff()
	if err := backoff.Retry(doOnce, backoff.WithContext(bo, ctx)); err != nil {
		return "", err
	}
	return location, nil
}

// postFinalConcat sends POST with Upload-Concat: final;<urls>. Returns the
// final's absolute URL and its declared size from the response headers.
func (c *Client) postFinalConcat(ctx context.Context, namespace string, partialURLs []string, remoteName string) (string, int64, error) {
	url := fmt.Sprintf("%s/v1/uploads/%s", c.BaseURL, namespace)
	hdr := "final;" + joinPartialURLs(partialURLs)
	var (
		location string
		size     int64
	)
	doOnce := func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
		if err != nil {
			return backoff.Permanent(err)
		}
		req.Header.Set(headerTusResume, tusVersion)
		req.Header.Set(headerAuth, "Bearer "+c.Token)
		req.Header.Set("Upload-Concat", hdr)
		if remoteName != "" {
			meta := "filename " + base64.StdEncoding.EncodeToString([]byte(remoteName))
			req.Header.Set(headerUploadMeta, meta)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if !IsRetryable(err) {
				return backoff.Permanent(err)
			}
			return err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode != http.StatusCreated {
			_, _, perr := classifyResponse(resp.StatusCode, string(body))
			if isPermanent(perr) {
				return backoff.Permanent(perr)
			}
			return perr
		}
		loc := resp.Header.Get(headerLocation)
		if loc == "" {
			return backoff.Permanent(errors.New("final create returned no Location"))
		}
		location = loc
		// Upload-Length on the response tells us the stitched total.
		if v := resp.Header.Get(headerUploadLen); v != "" {
			if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
				size = parsed
			}
		}
		return nil
	}
	bo := c.NewBackoff()
	if err := backoff.Retry(doOnce, backoff.WithContext(bo, ctx)); err != nil {
		return "", 0, err
	}
	return c.absoluteURL(location), size, nil
}

// joinPartialURLs produces "url1 url2 url3" with single-space separators.
// We pass the absolute URL paths (e.g. "/v1/uploads/ns/id") rather than
// fully-qualified URLs; the server accepts either, and bare paths avoid
// scheme/host coupling on shared hosts.
func joinPartialURLs(urls []string) string {
	paths := make([]string, len(urls))
	for i, u := range urls {
		paths[i] = pathOnly(u)
	}
	return strings.Join(paths, " ")
}

// pathOnly strips scheme://host from a URL, leaving just the path. Bare
// paths pass through unchanged.
func pathOnly(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		j := strings.IndexByte(rest, '/')
		if j < 0 {
			return ""
		}
		return rest[j:]
	}
	return u
}

// uploadSlab drives PATCHes against a single partial upload until the slab
// is fully transferred. Each chunk gets its own checksum (if algo != none)
// and uses the existing 409/5xx retry policy. Adaptive chunk sizing applies
// per-slab.
func (c *Client) uploadSlab(ctx context.Context, filePath string, sl slab, uploadURL string,
	chunkSize int64, algo string, adaptive bool, addProgress func(int64)) error {
	if sl.length == 0 {
		return nil
	}
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	// On any retry we re-HEAD the partial to discover the server's offset
	// and resume from there. The server's offset is RELATIVE to the slab
	// (since each partial is its own upload), so absolute file offset is
	// sl.start + serverOffset.
	sizer := newAdaptiveSizer(chunkSize, adaptive)
	var sentSlabBytes int64 // bytes confirmed accepted by server

	for sentSlabBytes < sl.length {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunkLen := sizer.size()
		if chunkLen <= 0 {
			chunkLen = chunkSize
		}
		if remaining := sl.length - sentSlabBytes; remaining < chunkLen {
			chunkLen = remaining
		}
		if err := c.patchSlabChunk(ctx, f, sl.start, &sentSlabBytes, chunkLen, uploadURL, algo, sizer, addProgress); err != nil {
			return err
		}
	}
	return nil
}

// patchSlabChunk PATCHes one chunk of the slab and updates sentSlabBytes on
// success. Mirrors patchOne's retry/HEAD-recovery flow, but works in
// slab-relative coordinates.
func (c *Client) patchSlabChunk(ctx context.Context, f *os.File, slabStart int64,
	sentSlabBytes *int64, chunkLen int64, uploadURL, algo string, sizer *adaptiveSizer,
	addProgress func(int64)) error {

	bo := c.NewBackoff()

	// Capture the slab-relative offset for this chunk; if a retry HEADs
	// and learns a different offset, we update startSlabOff so subsequent
	// reads come from the right place.
	startSlabOff := *sentSlabBytes

	op := func() error {
		// Read chunk from disk via SectionReader to keep concurrent
		// workers' file offsets independent.
		sect := io.NewSectionReader(f, slabStart+startSlabOff, chunkLen)
		var reqBody io.Reader = sect
		var cksumHeaderVal string
		if algo != checksumAlgoNone {
			buf := make([]byte, chunkLen)
			if _, rerr := io.ReadFull(sect, buf); rerr != nil {
				return backoff.Permanent(fmt.Errorf("read chunk: %w", rerr))
			}
			cv, cerr := computeSlabChecksum(algo, buf)
			if cerr != nil {
				return backoff.Permanent(cerr)
			}
			cksumHeaderVal = cv
			reqBody = bytes.NewReader(buf)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPatch, uploadURL, reqBody)
		if err != nil {
			return backoff.Permanent(err)
		}
		req.Header.Set(headerTusResume, tusVersion)
		req.Header.Set(headerAuth, "Bearer "+c.Token)
		req.Header.Set(headerContentType, patchContentType)
		req.Header.Set(headerUploadOff, strconv.FormatInt(startSlabOff, 10))
		if cksumHeaderVal != "" {
			req.Header.Set(headerUploadCksum, cksumHeaderVal)
		}
		req.ContentLength = chunkLen

		attemptStart := time.Now()
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if !IsRetryable(err) {
				return backoff.Permanent(err)
			}
			// Refresh: HEAD to learn the server's actual offset.
			off, herr := c.headOffset(ctx, uploadURL)
			if herr == nil {
				delta := off - startSlabOff
				if delta > 0 {
					addProgress(delta)
				}
				startSlabOff = off
				*sentSlabBytes = off
			}
			return err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

		if resp.StatusCode == http.StatusNoContent {
			newOff, perr := strconv.ParseInt(resp.Header.Get(headerUploadOff), 10, 64)
			if perr != nil {
				return backoff.Permanent(fmt.Errorf("invalid Upload-Offset in 204: %w", perr))
			}
			delta := newOff - startSlabOff
			if delta < 0 {
				return backoff.Permanent(fmt.Errorf("server offset went backwards"))
			}
			startSlabOff = newOff
			*sentSlabBytes = newOff
			if delta > 0 {
				addProgress(delta)
			}
			sizer.observe(delta, time.Since(attemptStart))
			return nil
		}

		retry, headThenResume := ClassifyStatus(resp.StatusCode)
		switch {
		case headThenResume:
			off, herr := c.headOffset(ctx, uploadURL)
			if herr != nil {
				if !IsRetryable(herr) {
					return backoff.Permanent(herr)
				}
				return herr
			}
			delta := off - startSlabOff
			if delta > 0 {
				addProgress(delta)
			}
			startSlabOff = off
			*sentSlabBytes = off
			return fmt.Errorf("offset conflict (now %d), resuming", off)
		case retry:
			off, herr := c.headOffset(ctx, uploadURL)
			if herr == nil {
				delta := off - startSlabOff
				if delta > 0 {
					addProgress(delta)
				}
				startSlabOff = off
				*sentSlabBytes = off
			}
			return fmt.Errorf("server returned %d %s: %s", resp.StatusCode, http.StatusText(resp.StatusCode), string(body))
		default:
			return backoff.Permanent(&PermanentError{
				Status: resp.StatusCode,
				Body:   string(body),
			})
		}
	}

	return backoff.Retry(op, backoff.WithContext(bo, ctx))
}

// computeSlabChecksum mirrors computeChunkChecksum but is callable from
// parallel.go without dragging the Chunker dependency through.
func computeSlabChecksum(algo string, chunk []byte) (string, error) {
	switch algo {
	case checksumAlgoCRC32C:
		h := crc32.New(crc32cClientTable)
		_, _ = h.Write(chunk)
		return checksumAlgoCRC32C + " " + hex.EncodeToString(h.Sum(nil)), nil
	case checksumAlgoSHA256:
		h := sha256.New()
		_, _ = h.Write(chunk)
		return checksumAlgoSHA256 + " " + hex.EncodeToString(h.Sum(nil)), nil
	default:
		return "", fmt.Errorf("unsupported checksum algo %q", algo)
	}
}
