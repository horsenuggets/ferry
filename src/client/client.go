package client

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
)

const (
	tusVersion        = "1.0.0"
	patchContentType  = "application/offset+octet-stream"
	headerTusResume   = "Tus-Resumable"
	headerUploadOff   = "Upload-Offset"
	headerUploadLen   = "Upload-Length"
	headerUploadMeta  = "Upload-Metadata"
	headerIdemKey     = "Idempotency-Key"
	headerLocation    = "Location"
	headerAuth        = "Authorization"
	headerContentType = "Content-Type"
)

// Client is a stateful uploader bound to a single peer URL + token. Reuse
// across multiple uploads is fine; the underlying http.Client maintains a
// connection pool.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client

	// Hooks for tests; nil in production.
	NewBackoff func() backoff.BackOff
}

// NewClient returns a Client wired with NewHTTPClient and the default
// chunk-backoff schedule.
func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Token:      token,
		HTTP:       NewHTTPClient(),
		NewBackoff: NewChunkBackoff,
	}
}

// UploadOptions captures the per-call knobs for Upload.
type UploadOptions struct {
	Namespace      string
	RemoteName     string // becomes Upload-Metadata "filename"
	ChunkSize      int64
	IdempotencyKey string // optional
	Progress       *Progress
}

// UploadResult summarizes a successful upload.
type UploadResult struct {
	UploadID    string
	LocationURL string // absolute URL, e.g. http://host/v1/uploads/ns/<id>
	Size        int64
	Duration    time.Duration
}

// PermanentError wraps an error that should not be retried (e.g. 401, 403,
// 400, or another 4xx that isn't 409). Surfaced to the caller so it can exit
// non-zero with a clear reason.
type PermanentError struct {
	Status int
	Body   string
	Err    error
}

func (e *PermanentError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("server returned %d %s: %s", e.Status, http.StatusText(e.Status), e.Body)
}

func (e *PermanentError) Unwrap() error { return e.Err }

// Upload uploads filePath to the peer. Returns the result on success, or an
// error (possibly *PermanentError) on failure.
func (c *Client) Upload(ctx context.Context, filePath string, opts UploadOptions) (*UploadResult, error) {
	if opts.Namespace == "" {
		return nil, errors.New("namespace is required")
	}
	if opts.ChunkSize < 1 || opts.ChunkSize > MaxChunkSizeBytes {
		return nil, fmt.Errorf("chunk size out of range: %d (max %d)", opts.ChunkSize, MaxChunkSizeBytes)
	}

	chunker, err := NewChunker(filePath, opts.ChunkSize)
	if err != nil {
		return nil, err
	}
	defer func() { _ = chunker.Close() }()

	remoteName := opts.RemoteName
	if remoteName == "" {
		remoteName = filepath.Base(filePath)
	}

	startTime := time.Now()

	// Step 1: POST to create. May return 200 (idempotent reuse) or 201
	// (fresh creation). On 200 we HEAD to learn the existing offset.
	location, status, err := c.createUpload(ctx, opts.Namespace, chunker.Size(), remoteName, opts.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	uploadURL := c.absoluteURL(location)
	uploadID := pathID(location)

	// If the server returned 200, this is an existing upload (idempotent
	// re-POST). Discover its current offset so we resume rather than start
	// from zero.
	if status == http.StatusOK {
		off, err := c.headOffset(ctx, uploadURL)
		if err != nil {
			return nil, fmt.Errorf("head existing upload: %w", err)
		}
		if err := chunker.SeekTo(off); err != nil {
			return nil, fmt.Errorf("seek to server offset %d: %w", off, err)
		}
		if opts.Progress != nil {
			opts.Progress.SetBytes(off)
		}
	}

	// Step 2: PATCH chunks until done.
	for !chunker.Done() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := c.patchOne(ctx, uploadURL, chunker, opts.Progress); err != nil {
			return nil, err
		}
	}

	// 0-byte file: confirm offset == 0 == size. Otherwise verify final offset.
	finalOff, err := c.headOffset(ctx, uploadURL)
	if err != nil {
		return nil, fmt.Errorf("final head: %w", err)
	}
	if finalOff != chunker.Size() {
		return nil, fmt.Errorf("final offset %d != size %d", finalOff, chunker.Size())
	}

	res := &UploadResult{
		UploadID:    uploadID,
		LocationURL: uploadURL,
		Size:        chunker.Size(),
		Duration:    time.Since(startTime),
	}
	if opts.Progress != nil {
		opts.Progress.Done(res.UploadID, res.LocationURL)
	}
	return res, nil
}

// createUpload sends POST and returns the Location header + the HTTP status
// (201 or 200, see Upload). Permanent failures return *PermanentError.
func (c *Client) createUpload(ctx context.Context, namespace string, size int64, remoteName, idemKey string) (location string, status int, err error) {
	url := fmt.Sprintf("%s/v1/uploads/%s", c.BaseURL, namespace)

	doOnce := func() (string, int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
		if err != nil {
			return "", 0, err
		}
		req.Header.Set(headerTusResume, tusVersion)
		req.Header.Set(headerAuth, "Bearer "+c.Token)
		req.Header.Set(headerUploadLen, strconv.FormatInt(size, 10))
		if remoteName != "" {
			meta := "filename " + base64.StdEncoding.EncodeToString([]byte(remoteName))
			req.Header.Set(headerUploadMeta, meta)
		}
		if idemKey != "" {
			req.Header.Set(headerIdemKey, idemKey)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return "", 0, err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

		switch {
		case resp.StatusCode == http.StatusCreated, resp.StatusCode == http.StatusOK:
			loc := resp.Header.Get(headerLocation)
			if loc == "" {
				return "", resp.StatusCode, fmt.Errorf("server returned %d without Location header", resp.StatusCode)
			}
			return loc, resp.StatusCode, nil
		default:
			return classifyResponse(resp.StatusCode, string(body))
		}
	}

	// Apply the same retry policy as PATCHes for transient failures.
	bo := c.NewBackoff()
	op := func() error {
		loc, st, e := doOnce()
		if e == nil {
			location, status = loc, st
			return nil
		}
		if isPermanent(e) {
			return backoff.Permanent(e)
		}
		if !IsRetryable(e) {
			return backoff.Permanent(e)
		}
		return e
	}
	err = backoff.Retry(op, backoff.WithContext(bo, ctx))
	return location, status, err
}

// patchOne sends a single PATCH. Handles 409 (HEAD + resume), 5xx (retry with
// HEAD reseed each attempt), and 4xx-not-409 (permanent error). On full
// success, advances the chunker and the progress reporter.
func (c *Client) patchOne(ctx context.Context, uploadURL string, ch *Chunker, prog *Progress) error {
	bo := c.NewBackoff()

	// We capture the chunk metadata once per outer call. Each attempt
	// re-seeks (after a HEAD) and re-reads from the file.
	startOffset := ch.Offset()
	chunkLen := ch.ChunkSize()
	if remaining := ch.Size() - startOffset; remaining < chunkLen {
		chunkLen = remaining
	}
	// If the file is empty, there is no PATCH to send. Caller handles this
	// upstream by checking Done(); we should never be invoked with chunkLen
	// == 0, but guard anyway.
	if chunkLen == 0 {
		return nil
	}

	op := func() error {
		// (Re-)seek to whatever offset the server believes is current.
		// On the first attempt this is a no-op. On a retry, we HEAD first
		// to learn the actual server offset (which may differ from
		// startOffset if a prior attempt absorbed some bytes).
		bodyR, rerr := ch.ReaderAt(startOffset, chunkLen)
		if rerr != nil {
			return backoff.Permanent(rerr)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPatch, uploadURL, bodyR)
		if err != nil {
			return backoff.Permanent(err)
		}
		req.Header.Set(headerTusResume, tusVersion)
		req.Header.Set(headerAuth, "Bearer "+c.Token)
		req.Header.Set(headerContentType, patchContentType)
		req.Header.Set(headerUploadOff, strconv.FormatInt(startOffset, 10))
		req.ContentLength = chunkLen

		resp, err := c.HTTP.Do(req)
		if err != nil {
			if !IsRetryable(err) {
				return backoff.Permanent(err)
			}
			// Refresh offset before next attempt: server may have absorbed
			// part of the chunk before the connection died.
			if err := c.refreshChunker(ctx, uploadURL, ch, &startOffset, &chunkLen); err != nil {
				return err // already classified
			}
			return err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

		if resp.StatusCode == http.StatusNoContent {
			// Success. Trust the server's reported offset.
			newOff, perr := strconv.ParseInt(resp.Header.Get(headerUploadOff), 10, 64)
			if perr != nil {
				return backoff.Permanent(fmt.Errorf("invalid Upload-Offset in 204: %w", perr))
			}
			delta := newOff - startOffset
			if delta < 0 {
				return backoff.Permanent(fmt.Errorf("server offset went backwards: was %d, now %d", startOffset, newOff))
			}
			if err := ch.SeekTo(newOff); err != nil {
				return backoff.Permanent(err)
			}
			if prog != nil && delta > 0 {
				prog.Add(delta)
			}
			return nil
		}

		retry, headThenResume := ClassifyStatus(resp.StatusCode)
		switch {
		case headThenResume:
			// 409 Conflict: HEAD to learn truth, seek there, retry.
			off, herr := c.headOffset(ctx, uploadURL)
			if herr != nil {
				if !IsRetryable(herr) {
					return backoff.Permanent(herr)
				}
				return herr
			}
			startOffset = off
			remaining := ch.Size() - startOffset
			chunkLen = ch.ChunkSize()
			if remaining < chunkLen {
				chunkLen = remaining
			}
			if chunkLen == 0 {
				// Server already has everything; sync chunker and we're done with this chunk.
				if err := ch.SeekTo(off); err != nil {
					return backoff.Permanent(err)
				}
				return nil
			}
			if err := ch.SeekTo(startOffset); err != nil {
				return backoff.Permanent(err)
			}
			// Treat as retryable so the backoff machinery re-invokes us.
			return fmt.Errorf("offset conflict (now %d), resuming", off)
		case retry:
			// 5xx / 408 / 429: refresh offset and retry.
			if err := c.refreshChunker(ctx, uploadURL, ch, &startOffset, &chunkLen); err != nil {
				return err
			}
			return &PermanentError{Status: resp.StatusCode, Body: string(body)} // wrapped retryable below
		default:
			// 4xx-other (including 401, 403, 400, 413, 415): permanent.
			return backoff.Permanent(&PermanentError{
				Status: resp.StatusCode,
				Body:   string(body),
			})
		}
	}

	// Wrap op so 5xx (PermanentError-but-retryable) gets recycled.
	wrapped := func() error {
		err := op()
		if err == nil {
			return nil
		}
		// Already-marked permanent errors propagate as-is.
		var pErr *backoff.PermanentError
		if errors.As(err, &pErr) {
			return err
		}
		// Bare *PermanentError returned by the 5xx branch: it's actually
		// retryable in our policy; let backoff re-run.
		return err
	}

	return backoff.Retry(wrapped, backoff.WithContext(bo, ctx))
}

// refreshChunker HEADs the upload to learn the server's current offset, then
// seeks the chunker to it and updates startOffset/chunkLen. Used between
// retries so the next attempt sends the correct bytes.
func (c *Client) refreshChunker(ctx context.Context, uploadURL string, ch *Chunker, startOffset, chunkLen *int64) error {
	off, herr := c.headOffset(ctx, uploadURL)
	if herr != nil {
		if !IsRetryable(herr) {
			return backoff.Permanent(herr)
		}
		// HEAD also failed transiently; let backoff re-run the outer op.
		return herr
	}
	*startOffset = off
	remaining := ch.Size() - off
	*chunkLen = ch.ChunkSize()
	if remaining < *chunkLen {
		*chunkLen = remaining
	}
	if err := ch.SeekTo(off); err != nil {
		return backoff.Permanent(err)
	}
	return nil
}

// headOffset returns the server's current Upload-Offset for the upload at
// uploadURL.
func (c *Client) headOffset(ctx context.Context, uploadURL string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, uploadURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set(headerTusResume, tusVersion)
	req.Header.Set(headerAuth, "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_, _, perr := classifyResponse(resp.StatusCode, string(body))
		if perr != nil {
			return 0, perr
		}
		return 0, fmt.Errorf("HEAD returned %d", resp.StatusCode)
	}
	off, err := strconv.ParseInt(resp.Header.Get(headerUploadOff), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid Upload-Offset on HEAD: %w", err)
	}
	return off, nil
}

// StatusInfo is what `ferry status` reports.
type StatusInfo struct {
	UploadURL string
	Offset    int64
	Size      int64
	Complete  bool
}

// Status HEADs the upload and returns its current state.
func (c *Client) Status(ctx context.Context, namespace, uploadID string) (*StatusInfo, error) {
	url := fmt.Sprintf("%s/v1/uploads/%s/%s", c.BaseURL, namespace, uploadID)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(headerTusResume, tusVersion)
	req.Header.Set(headerAuth, "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &PermanentError{Status: resp.StatusCode, Body: string(body)}
	}
	off, err := strconv.ParseInt(resp.Header.Get(headerUploadOff), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid Upload-Offset: %w", err)
	}
	size, err := strconv.ParseInt(resp.Header.Get(headerUploadLen), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid Upload-Length: %w", err)
	}
	return &StatusInfo{
		UploadURL: url,
		Offset:    off,
		Size:      size,
		Complete:  off == size && size >= 0,
	}, nil
}

// absoluteURL resolves a Location value (which may be a relative path) to
// the absolute peer URL.
func (c *Client) absoluteURL(loc string) string {
	if strings.HasPrefix(loc, "http://") || strings.HasPrefix(loc, "https://") {
		return loc
	}
	return c.BaseURL + loc
}

// pathID extracts the upload id from a /v1/uploads/<ns>/<id> path or URL.
func pathID(loc string) string {
	// Strip query/fragment if any.
	if i := strings.IndexAny(loc, "?#"); i >= 0 {
		loc = loc[:i]
	}
	loc = strings.TrimRight(loc, "/")
	idx := strings.LastIndex(loc, "/")
	if idx < 0 {
		return loc
	}
	return loc[idx+1:]
}

// classifyResponse turns a non-2xx status into either a PermanentError (4xx
// non-409) or a sentinel error suitable for retry. Returns ("", status, err).
func classifyResponse(status int, body string) (string, int, error) {
	if status == http.StatusConflict {
		return "", status, errors.New("offset conflict (use HEAD to recover)")
	}
	if status >= 500 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
		// Retryable.
		return "", status, fmt.Errorf("server returned %d: %s", status, body)
	}
	return "", status, &PermanentError{Status: status, Body: body}
}

// isPermanent reports whether err is already a *PermanentError or wraps
// *backoff.PermanentError.
func isPermanent(err error) bool {
	var pe *PermanentError
	if errors.As(err, &pe) {
		return true
	}
	var bpe *backoff.PermanentError
	return errors.As(err, &bpe)
}
