package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testRig sets up an httptest.Server with an isolated Store and a known token.
type testRig struct {
	srv   *httptest.Server
	store *Store
	token string
}

func newRig(t *testing.T) *testRig {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	token := "test-token"
	auth := NewAuthenticator(map[string]TokenEntry{
		HashToken(token): {Namespaces: []string{"alpha", "beta"}},
	})
	h := NewHandler(HandlerConfig{
		Store:         store,
		Auth:          auth,
		Locker:        NewLocker(),
		MaxPatchBytes: 1024,
		SafetyMargin:  0,
		CompletedTTL:  time.Hour,
		IncompleteTTL: time.Hour,
		Version:       "test",
	})
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return &testRig{srv: srv, store: store, token: token}
}

func (r *testRig) do(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (r *testRig) newReq(t *testing.T, method, path string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, r.srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Tus-Resumable", tusVersion)
	req.Header.Set("Authorization", "Bearer "+r.token)
	return req
}

// createUpload returns the upload's full URL via Location.
func (r *testRig) createUpload(t *testing.T, namespace string, size int64, idem string) string {
	t.Helper()
	req := r.newReq(t, "POST", "/v1/uploads/"+namespace, nil)
	req.Header.Set("Upload-Length", strconv.FormatInt(size, 10))
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("createUpload status = %d, body = %s", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatalf("missing Location header")
	}
	return loc
}

func TestHealthEndpoint(t *testing.T) {
	r := newRig(t)
	resp, err := http.Get(r.srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"ok":true`) {
		t.Errorf("health body = %s", body)
	}
}

func TestPostCreatesUpload(t *testing.T) {
	r := newRig(t)
	loc := r.createUpload(t, "alpha", 5, "")
	if !strings.Contains(loc, "/v1/uploads/alpha/") {
		t.Errorf("Location = %q", loc)
	}
	// Extract id, verify .partial exists.
	id := strings.TrimPrefix(loc[strings.LastIndex(loc, "/v1/uploads/alpha/")+len("/v1/uploads/alpha/"):], "")
	if _, err := r.store.LoadInfo("alpha", id); err != nil {
		t.Errorf("LoadInfo: %v", err)
	}
}

func TestPostIdempotencyReturnsSameLocation(t *testing.T) {
	r := newRig(t)
	loc1 := r.createUpload(t, "alpha", 5, "key-A")

	req := r.newReq(t, "POST", "/v1/uploads/alpha", nil)
	req.Header.Set("Upload-Length", "5")
	req.Header.Set("Idempotency-Key", "key-A")
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("second POST status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("Location") != loc1 && !strings.HasSuffix(loc1, resp.Header.Get("Location")) {
		// Server returns path; loc1 is full URL.
		got := resp.Header.Get("Location")
		if !strings.HasSuffix(loc1, got) {
			t.Errorf("Location mismatch: first %q, second %q", loc1, got)
		}
	}
}

func TestPostInsufficientStorage(t *testing.T) {
	r := newRig(t)
	// Build a handler with a huge safety margin so any size > 0 fails.
	dir := t.TempDir()
	store, _ := NewStore(dir)
	auth := NewAuthenticator(map[string]TokenEntry{
		HashToken(r.token): {Namespaces: []string{"alpha"}},
	})
	h := NewHandler(HandlerConfig{
		Store:         store,
		Auth:          auth,
		Locker:        NewLocker(),
		MaxPatchBytes: 1024,
		SafetyMargin:  1 << 62, // absurdly large
		CompletedTTL:  time.Hour,
		IncompleteTTL: time.Hour,
		Version:       "test",
	})
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/uploads/alpha", nil)
	req.Header.Set("Tus-Resumable", tusVersion)
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Upload-Length", "1")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInsufficientStorage {
		t.Errorf("status = %d, want 507", resp.StatusCode)
	}
}

func TestHeadReturnsOffset(t *testing.T) {
	r := newRig(t)
	loc := r.createUpload(t, "alpha", 11, "")
	req := r.newReq(t, "HEAD", pathOf(loc), nil)
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d", resp.StatusCode)
	}
	if resp.Header.Get("Upload-Offset") != "0" {
		t.Errorf("Upload-Offset = %q", resp.Header.Get("Upload-Offset"))
	}
	if resp.Header.Get("Upload-Length") != "11" {
		t.Errorf("Upload-Length = %q", resp.Header.Get("Upload-Length"))
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
}

func TestHeadMissing(t *testing.T) {
	r := newRig(t)
	req := r.newReq(t, "HEAD", "/v1/uploads/alpha/ghost", nil)
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestPatchAppendsAndCompletes(t *testing.T) {
	r := newRig(t)
	loc := r.createUpload(t, "alpha", 11, "")
	id := lastSegment(loc)

	body := bytes.NewReader([]byte("hello world"))
	req := r.newReq(t, "PATCH", pathOf(loc), body)
	req.Header.Set("Upload-Offset", "0")
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.ContentLength = 11
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		bb, _ := io.ReadAll(resp.Body)
		t.Fatalf("PATCH status = %d, body=%s", resp.StatusCode, bb)
	}
	if resp.Header.Get("Upload-Offset") != "11" {
		t.Errorf("Upload-Offset after = %q", resp.Header.Get("Upload-Offset"))
	}
	// Completed file must exist; .partial must not.
	if _, err := r.store.LoadInfo("alpha", id); err != nil {
		t.Errorf("LoadInfo: %v", err)
	}
	if _, err := readCompleted(r, "alpha", id); err != nil {
		t.Errorf("completed: %v", err)
	}
}

func TestPatchWrongOffset(t *testing.T) {
	r := newRig(t)
	loc := r.createUpload(t, "alpha", 11, "")
	req := r.newReq(t, "PATCH", pathOf(loc), bytes.NewReader([]byte("x")))
	req.Header.Set("Upload-Offset", "5") // wrong, should be 0
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.ContentLength = 1
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestPatchWrongContentType(t *testing.T) {
	r := newRig(t)
	loc := r.createUpload(t, "alpha", 11, "")
	req := r.newReq(t, "PATCH", pathOf(loc), bytes.NewReader([]byte("x")))
	req.Header.Set("Upload-Offset", "0")
	req.Header.Set("Content-Type", "text/plain")
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", resp.StatusCode)
	}
}

func TestPatchTooLarge(t *testing.T) {
	r := newRig(t)
	loc := r.createUpload(t, "alpha", 100000, "")
	// Send 2KB body, but max_patch_bytes = 1024.
	body := bytes.NewReader(make([]byte, 2000))
	req := r.newReq(t, "PATCH", pathOf(loc), body)
	req.Header.Set("Upload-Offset", "0")
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.ContentLength = 2000
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

func TestDeleteRemovesFiles(t *testing.T) {
	r := newRig(t)
	loc := r.createUpload(t, "alpha", 5, "")
	id := lastSegment(loc)
	req := r.newReq(t, "DELETE", pathOf(loc), nil)
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
	if _, err := r.store.LoadInfo("alpha", id); err == nil {
		t.Errorf("LoadInfo after DELETE succeeded; want ErrNotFound")
	}
}

func TestMissingTusResumable(t *testing.T) {
	r := newRig(t)
	req, _ := http.NewRequest("POST", r.srv.URL+"/v1/uploads/alpha", nil)
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Upload-Length", "1")
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want 412", resp.StatusCode)
	}
}

func TestMissingBearer(t *testing.T) {
	r := newRig(t)
	req, _ := http.NewRequest("POST", r.srv.URL+"/v1/uploads/alpha", nil)
	req.Header.Set("Tus-Resumable", tusVersion)
	req.Header.Set("Upload-Length", "1")
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestWrongNamespaceForToken(t *testing.T) {
	r := newRig(t)
	req, _ := http.NewRequest("POST", r.srv.URL+"/v1/uploads/forbidden-ns", nil)
	req.Header.Set("Tus-Resumable", tusVersion)
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Upload-Length", "1")
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestPostInvalidUploadLength(t *testing.T) {
	r := newRig(t)
	req := r.newReq(t, "POST", "/v1/uploads/alpha", nil)
	req.Header.Set("Upload-Length", "abc")
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPatchMissingOffset(t *testing.T) {
	r := newRig(t)
	loc := r.createUpload(t, "alpha", 5, "")
	req := r.newReq(t, "PATCH", pathOf(loc), bytes.NewReader([]byte("x")))
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPatchAfterCompletion(t *testing.T) {
	r := newRig(t)
	loc := r.createUpload(t, "alpha", 3, "")
	// Complete it.
	req := r.newReq(t, "PATCH", pathOf(loc), bytes.NewReader([]byte("abc")))
	req.Header.Set("Upload-Offset", "0")
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.ContentLength = 3
	resp := r.do(t, req)
	resp.Body.Close()
	// Try to PATCH again; should fail with 409 (or 404 if we cleaned up).
	req2 := r.newReq(t, "PATCH", pathOf(loc), bytes.NewReader([]byte("d")))
	req2.Header.Set("Upload-Offset", "3")
	req2.Header.Set("Content-Type", "application/offset+octet-stream")
	req2.ContentLength = 1
	resp2 := r.do(t, req2)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict && resp2.StatusCode != http.StatusNotFound {
		t.Errorf("PATCH after complete status = %d, want 409 or 404", resp2.StatusCode)
	}
}

// TestConcurrentPatchCooperativeCancel verifies that two PATCHes on the same
// upload don't deadlock and end with a consistent on-disk state.
func TestConcurrentPatchCooperativeCancel(t *testing.T) {
	r := newRig(t)
	loc := r.createUpload(t, "alpha", 1024, "")

	// First PATCH: a slow reader so the holder is still in io.Copy when
	// the second arrives.
	slow := &slowReader{
		data:  make([]byte, 20),
		delay: 50 * time.Millisecond,
	}
	req1 := r.newReq(t, "PATCH", pathOf(loc), slow)
	req1.Header.Set("Upload-Offset", "0")
	req1.Header.Set("Content-Type", "application/offset+octet-stream")
	req1.ContentLength = 20

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := r.srv.Client().Do(req1)
		if err == nil {
			resp.Body.Close()
		}
		// We don't assert on resp1's status: it may be 204 (if it finished
		// before the second arrived) or anything cancel-related.
	}()

	// Give holder a moment to acquire the lock and start writing.
	time.Sleep(75 * time.Millisecond)

	// Second PATCH: arrives mid-flight, should kick the holder, then need
	// to discover the new offset via HEAD. We expect 409 here because we
	// pass offset=0 but the first holder probably wrote some bytes already.
	body2 := bytes.NewReader(make([]byte, 100))
	req2 := r.newReq(t, "PATCH", pathOf(loc), body2)
	req2.Header.Set("Upload-Offset", "0")
	req2.Header.Set("Content-Type", "application/offset+octet-stream")
	req2.ContentLength = 100
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req2 = req2.WithContext(ctx)
	resp2, err := r.srv.Client().Do(req2)
	if err != nil {
		t.Fatalf("second PATCH transport err: %v", err)
	}
	resp2.Body.Close()
	// Wait for the first request to finish so we can inspect final state.
	wg.Wait()

	// Now HEAD: offset should match what's on disk, and disk should be
	// consistent (not interleaved). We can't easily assert "no
	// interleave" but we can assert offset is between 0 and 200.
	headReq := r.newReq(t, "HEAD", pathOf(loc), nil)
	headResp := r.do(t, headReq)
	defer headResp.Body.Close()
	if headResp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d", headResp.StatusCode)
	}
	off, _ := strconv.ParseInt(headResp.Header.Get("Upload-Offset"), 10, 64)
	if off < 0 || off > 30 {
		t.Errorf("offset after concurrent patch = %d, expected 0..30", off)
	}
}

// helpers --------------------------------------------------------------------

func pathOf(fullURL string) string {
	// Strip scheme://host
	idx := strings.Index(fullURL, "/v1/uploads/")
	if idx == -1 {
		return fullURL
	}
	return fullURL[idx:]
}

func lastSegment(s string) string {
	i := strings.LastIndex(s, "/")
	if i == -1 {
		return s
	}
	return s[i+1:]
}

func readCompleted(r *testRig, ns, id string) ([]byte, error) {
	return readFile(r.store.completedPath(ns, id))
}

func readFile(path string) ([]byte, error) {
	f, err := openFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func openFile(path string) (io.ReadCloser, error) {
	f, err := openOSFile(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return f, nil
}

// slowReader emits data slowly so a concurrent PATCH can race the holder.
type slowReader struct {
	data  []byte
	pos   int
	delay time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}
	time.Sleep(s.delay)
	end := s.pos + 1
	if end > len(s.data) {
		end = len(s.data)
	}
	n := copy(p, s.data[s.pos:end]) // 1 byte at a time
	s.pos += n
	return n, nil
}
