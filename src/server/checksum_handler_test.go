package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"hash/crc32"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func crc32cHex(b []byte) string {
	t := crc32.MakeTable(crc32.Castagnoli)
	h := crc32.New(t)
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func sha256Hex(b []byte) string {
	h := sha256.New()
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func sendPatchCS(t *testing.T, r *testRig, loc string, offset int64, body []byte, checksumHeader string) *http.Response {
	t.Helper()
	req := r.newReq(t, "PATCH", strings.TrimPrefix(loc, r.srv.URL), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
	req.ContentLength = int64(len(body))
	if checksumHeader != "" {
		req.Header.Set("Upload-Checksum", checksumHeader)
	}
	return r.do(t, req)
}

// lastPathSegment returns the trailing segment of `/v1/uploads/<ns>/<id>`.
func lastPathSegment(path string) string {
	parts := strings.Split(path, "/")
	return parts[len(parts)-1]
}

func TestPatchChecksumCRC32CSucceeds(t *testing.T) {
	r := newRig(t)
	body := []byte("hello world!")
	loc := r.createUpload(t, "alpha", int64(len(body)), "")
	resp := sendPatchCS(t, r, loc, 0, body, "crc32c "+crc32cHex(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		bodyR, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, bodyR)
	}
	if got := resp.Header.Get("Upload-Offset"); got != strconv.Itoa(len(body)) {
		t.Fatalf("Upload-Offset = %q, want %d", got, len(body))
	}
}

func TestPatchChecksumSHA256Succeeds(t *testing.T) {
	r := newRig(t)
	body := []byte("checksummed")
	loc := r.createUpload(t, "alpha", int64(len(body)), "")
	resp := sendPatchCS(t, r, loc, 0, body, "sha256 "+sha256Hex(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestPatchChecksumMismatchTruncates(t *testing.T) {
	r := newRig(t)
	loc := r.createUpload(t, "alpha", 16, "")

	// First PATCH: 8 good bytes with valid checksum.
	bodyA := []byte("ABCDEFGH")
	resp := sendPatchCS(t, r, loc, 0, bodyA, "crc32c "+crc32cHex(bodyA))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("setup PATCH status = %d", resp.StatusCode)
	}

	// Second PATCH: 8 bytes but lie about the checksum.
	bodyB := []byte("12345678")
	bogus := "crc32c " + crc32cHex([]byte("XXXXXXXX"))
	resp = sendPatchCS(t, r, loc, 8, bodyB, bogus)
	resp.Body.Close()
	if resp.StatusCode != statusChecksumMismatch {
		t.Fatalf("expected 460 on checksum mismatch, got %d", resp.StatusCode)
	}

	// Server should have truncated back to offset 8.
	off, err := r.store.CurrentOffset("alpha", lastPathSegment(loc))
	if err != nil {
		t.Fatalf("CurrentOffset: %v", err)
	}
	if off != 8 {
		t.Fatalf("CurrentOffset after mismatch = %d, want 8 (truncate-back)", off)
	}

	// Retry at offset 8 with correct checksum: should succeed.
	resp = sendPatchCS(t, r, loc, 8, bodyB, "crc32c "+crc32cHex(bodyB))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		bodyR, _ := io.ReadAll(resp.Body)
		t.Fatalf("retry status = %d, body = %s", resp.StatusCode, bodyR)
	}
}

func TestPatchChecksumUnsupportedAlgo(t *testing.T) {
	r := newRig(t)
	body := []byte("anything")
	loc := r.createUpload(t, "alpha", int64(len(body)), "")
	resp := sendPatchCS(t, r, loc, 0, body, "md5 d41d8cd98f00b204e9800998ecf8427e")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on unsupported algo, got %d", resp.StatusCode)
	}
}

func TestPatchChecksumMalformedHeader(t *testing.T) {
	r := newRig(t)
	body := []byte("anything")
	loc := r.createUpload(t, "alpha", int64(len(body)), "")
	resp := sendPatchCS(t, r, loc, 0, body, "garbage-no-space")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on malformed checksum header, got %d", resp.StatusCode)
	}
}

// Idempotency-Key persistence (phase 6 hardening): same key twice must
// return the same upload URL with the second request returning 200 (not
// another 201).
func TestPostIdempotencyReturnsSameUploadAfterCompletion(t *testing.T) {
	r := newRig(t)
	body := []byte("ferry")
	loc1 := r.createUpload(t, "alpha", int64(len(body)), "complete-key")
	id := lastPathSegment(loc1)

	// PATCH to completion.
	req := r.newReq(t, "PATCH", "/v1/uploads/alpha/"+id, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.Header.Set("Upload-Offset", "0")
	req.ContentLength = int64(len(body))
	resp := r.do(t, req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("completion PATCH = %d", resp.StatusCode)
	}

	// Re-POST with the same key after completion: must return 200 + same URL.
	req = r.newReq(t, "POST", "/v1/uploads/alpha", nil)
	req.Header.Set("Upload-Length", strconv.FormatInt(int64(len(body)), 10))
	req.Header.Set("Idempotency-Key", "complete-key")
	resp = r.do(t, req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-POST after completion status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != loc1 {
		t.Fatalf("re-POST Location = %q, want %q", got, loc1)
	}

	// HEAD that location: should report state=complete (offset == size).
	req = r.newReq(t, "HEAD", "/v1/uploads/alpha/"+id, nil)
	resp = r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD after re-POST = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Upload-Offset"); got != strconv.Itoa(len(body)) {
		t.Fatalf("HEAD Upload-Offset = %q, want %d", got, len(body))
	}
}
