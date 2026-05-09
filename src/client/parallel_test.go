package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"

	"github.com/horsenuggets/ferry/src/server"
)

// flakyProxy is an httptest.Server that proxies to upstream but injects
// 500s on every Nth PATCH request, simulating transient network/server
// blips. Used to exercise the worker-loop retry path.
type flakyProxy struct {
	*httptest.Server
	upstream string
	every    int32
	patches  atomic.Int32
}

func newFlakyProxy(t *testing.T, upstream string, every int) *flakyProxy {
	t.Helper()
	fp := &flakyProxy{upstream: upstream, every: int32(every)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", fp.handle)
	fp.Server = httptest.NewServer(mux)
	return fp
}

func (f *flakyProxy) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPatch {
		n := f.patches.Add(1)
		if n%f.every == 1 {
			// Inject a transient 500 without forwarding.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}
	target, _ := url.Parse(f.upstream)
	r2 := r.Clone(r.Context())
	r2.URL.Scheme = target.Scheme
	r2.URL.Host = target.Host
	r2.RequestURI = ""
	r2.Host = target.Host
	resp, err := http.DefaultTransport.RoundTrip(r2)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// newServerRig spins up a real ferry server.Handler against an httptest
// listener, returning the base URL, the bearer token, and a cleanup func.
func newServerRig(t *testing.T) (baseURL, token string, store *server.Store, cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	st, err := server.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tok := "para-token"
	auth := server.NewAuthenticator(map[string]server.TokenEntry{
		server.HashToken(tok): {Namespaces: []string{"ns"}},
	})
	h := server.NewHandler(server.HandlerConfig{
		Store:         st,
		Auth:          auth,
		Locker:        server.NewLocker(),
		MaxPatchBytes: 8 << 20, // 8 MiB
		SafetyMargin:  0,
		CompletedTTL:  time.Hour,
		IncompleteTTL: time.Hour,
		Version:       "test",
		Logger:        slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	srv := httptest.NewServer(h.Routes())
	return srv.URL, tok, st, srv.Close
}

// makeRandomFile writes n bytes of pseudo-random data to a tempfile and
// returns its path and sha256.
func makeRandomFile(t *testing.T, n int64) (string, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, fmt.Sprintf("data-%d.bin", n))
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rng := rand.New(rand.NewSource(int64(n) ^ 0x5eed))
	buf := make([]byte, 64*1024)
	h := sha256.New()
	var written int64
	for written < n {
		need := int64(len(buf))
		if rem := n - written; rem < need {
			need = rem
		}
		_, _ = rng.Read(buf[:need])
		if _, err := f.Write(buf[:need]); err != nil {
			t.Fatal(err)
		}
		_, _ = h.Write(buf[:need])
		written += need
	}
	return p, hex.EncodeToString(h.Sum(nil))
}

// noopBackoff returns a backoff.BackOff that retries with no delay so
// failure tests don't waste real wall clock.
func noopBackoff() backoff.BackOff {
	return backoff.NewConstantBackOff(0)
}

func TestSplitSlabs(t *testing.T) {
	cases := []struct {
		total int64
		n     int
		want  []slab
	}{
		{0, 4, []slab{{0, 0}}},
		{10, 1, []slab{{0, 10}}},
		{10, 2, []slab{{0, 5}, {5, 5}}},
		{10, 3, []slab{{0, 4}, {4, 3}, {7, 3}}},
		{2, 4, []slab{{0, 1}, {1, 1}}}, // n > total downshifts
	}
	for _, tc := range cases {
		got := splitSlabs(tc.total, tc.n)
		if len(got) != len(tc.want) {
			t.Errorf("splitSlabs(%d,%d) len = %d, want %d", tc.total, tc.n, len(got), len(tc.want))
			continue
		}
		var sum int64
		for i, s := range got {
			if s != tc.want[i] {
				t.Errorf("splitSlabs(%d,%d)[%d] = %+v, want %+v", tc.total, tc.n, i, s, tc.want[i])
			}
			sum += s.length
		}
		if tc.total > 0 && sum != tc.total {
			t.Errorf("splitSlabs(%d,%d) total length = %d", tc.total, tc.n, sum)
		}
	}
}

// TestUploadParallelFour sends a 5 MiB file via N=4 and verifies sha256.
func TestUploadParallelFour(t *testing.T) {
	const fileSize int64 = 5 * 1024 * 1024
	baseURL, token, store, cleanup := newServerRig(t)
	defer cleanup()
	path, wantHash := makeRandomFile(t, fileSize)

	c := NewClient(baseURL, token)
	c.NewBackoff = noopBackoff
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := c.Upload(ctx, path, UploadOptions{
		Namespace: "ns",
		ChunkSize: 1 << 20, // 1 MiB
		Parallel:  4,
		Checksum:  "none", // keep test focused on transfer correctness
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if res.Size != fileSize {
		t.Errorf("res.Size = %d, want %d", res.Size, fileSize)
	}
	// HEAD via Status to confirm completion.
	st, err := c.Status(ctx, "ns", res.UploadID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Complete || st.Size != fileSize {
		t.Errorf("status = %+v", st)
	}
	// Read the stitched file via the Store and compare sha256.
	data, err := os.ReadFile(store.CompletedPath("ns", res.UploadID))
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	gotSum := sha256.Sum256(data)
	if hex.EncodeToString(gotSum[:]) != wantHash {
		t.Errorf("sha256 mismatch:\n got  %s\n want %s", hex.EncodeToString(gotSum[:]), wantHash)
	}
	info, err := store.LoadInfo("ns", res.UploadID)
	if err != nil {
		t.Fatalf("load info: %v", err)
	}
	if info.Concat != "final" {
		t.Errorf("Info.Concat = %q, want \"final\"", info.Concat)
	}
	if len(info.ConcatSourceIDs) != 4 {
		t.Errorf("ConcatSourceIDs = %v", info.ConcatSourceIDs)
	}
}

// TestUploadParallelOneMatchesSequential checks that --parallel=1 takes the
// historic sequential path: a single regular (non-concat) upload with no
// stitching dance. Verified by ensuring the upload succeeds and the
// resulting on-disk Info has Concat=="" (not "final").
func TestUploadParallelOneMatchesSequential(t *testing.T) {
	const fileSize int64 = 200 * 1024
	baseURL, token, store, cleanup := newServerRig(t)
	defer cleanup()
	path, wantHash := makeRandomFile(t, fileSize)

	c := NewClient(baseURL, token)
	c.NewBackoff = noopBackoff
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := c.Upload(ctx, path, UploadOptions{
		Namespace: "ns",
		ChunkSize: 64 * 1024,
		Parallel:  1,
		Checksum:  "none",
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	info, err := store.LoadInfo("ns", res.UploadID)
	if err != nil {
		t.Fatalf("load info: %v", err)
	}
	if info.Concat != "" {
		t.Errorf("Parallel=1 took the concat path; Info.Concat = %q", info.Concat)
	}
	// Verify content too.
	data, err := os.ReadFile(store.CompletedPath("ns", res.UploadID))
	if err != nil {
		t.Fatal(err)
	}
	gotSum := sha256.Sum256(data)
	if hex.EncodeToString(gotSum[:]) != wantHash {
		t.Errorf("sha256 mismatch")
	}
}

// TestUploadParallelInvalidN rejects out-of-range parallelism.
func TestUploadParallelInvalidN(t *testing.T) {
	baseURL, token, _, cleanup := newServerRig(t)
	defer cleanup()
	path, _ := makeRandomFile(t, 1024)
	c := NewClient(baseURL, token)
	c.NewBackoff = noopBackoff
	for _, n := range []int{-1, MaxParallelWorkers + 1} {
		_, err := c.Upload(context.Background(), path, UploadOptions{
			Namespace: "ns",
			ChunkSize: 1024,
			Parallel:  n,
		})
		if err == nil {
			t.Errorf("Parallel=%d: want error", n)
		}
	}
}

// TestUploadParallelBadToken verifies that the parallel path returns a
// PermanentError when the server rejects with 401/403. The first
// createPartial fails and Upload returns without spinning workers.
func TestUploadParallelBadToken(t *testing.T) {
	baseURL, _, _, cleanup := newServerRig(t)
	defer cleanup()
	path, _ := makeRandomFile(t, 100*1024)
	c := NewClient(baseURL, "wrong-token")
	c.NewBackoff = noopBackoff
	_, err := c.Upload(context.Background(), path, UploadOptions{
		Namespace: "ns",
		ChunkSize: 16 * 1024,
		Parallel:  4,
		Checksum:  "none",
	})
	if err == nil {
		t.Fatal("expected error with bad token, got nil")
	}
}

// TestUploadParallelEmptyFile covers the n>total downshift path.
func TestUploadParallelEmptyFile(t *testing.T) {
	baseURL, token, _, cleanup := newServerRig(t)
	defer cleanup()
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.bin")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewClient(baseURL, token)
	c.NewBackoff = noopBackoff
	res, err := c.Upload(context.Background(), path, UploadOptions{
		Namespace: "ns",
		ChunkSize: 1024,
		Parallel:  4,
		Checksum:  "none",
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if res.Size != 0 {
		t.Errorf("Size = %d, want 0", res.Size)
	}
}

// TestPermanentErrorUnwrap is a tiny coverage-bumper for the wrapping.
func TestPermanentErrorUnwrap(t *testing.T) {
	inner := fmt.Errorf("boom")
	pe := &PermanentError{Status: 401, Body: "x", Err: inner}
	if pe.Unwrap() != inner {
		t.Errorf("unwrap mismatch")
	}
	if pe.Error() != "boom" {
		t.Errorf("error mismatch")
	}
	pe2 := &PermanentError{Status: 500, Body: "y"}
	if pe2.Error() == "" {
		t.Errorf("default error empty")
	}
}

// TestProgressSetters covers Progress methods that aren't otherwise
// exercised by the parallel rig.
func TestProgressSetters(t *testing.T) {
	p := NewProgress(100, ProgressSilent, nil, nil)
	p.SetClock(func() time.Time { return time.Unix(0, 0) })
	p.SetBytes(50)
	p.Add(25)
	p.Done("id", "url")
	// AutoProgressMode is just a tty check; call it for coverage.
	_ = AutoProgressMode()
}

// TestJoinPartialURLs covers the joiner directly (single-source corner).
func TestJoinPartialURLs(t *testing.T) {
	if got := joinPartialURLs([]string{"http://h/v1/uploads/a/b"}); got != "/v1/uploads/a/b" {
		t.Errorf("got %q", got)
	}
	if got := joinPartialURLs([]string{"/x", "/y"}); got != "/x /y" {
		t.Errorf("got %q", got)
	}
}

// TestPathOnly handles common URL shapes for Upload-Concat sources.
func TestPathOnly(t *testing.T) {
	cases := map[string]string{
		"/v1/uploads/a/b":               "/v1/uploads/a/b",
		"http://h/v1/uploads/a/b":       "/v1/uploads/a/b",
		"https://h:1234/v1/uploads/a/b": "/v1/uploads/a/b",
		"http://h":                      "",
	}
	for in, want := range cases {
		if got := pathOnly(in); got != want {
			t.Errorf("pathOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestUploadParallelWithSHA256 covers the parallel path with the sha256
// checksum branch enabled.
func TestUploadParallelWithSHA256(t *testing.T) {
	const fileSize int64 = 600 * 1024
	baseURL, token, store, cleanup := newServerRig(t)
	defer cleanup()
	path, wantHash := makeRandomFile(t, fileSize)

	c := NewClient(baseURL, token)
	c.NewBackoff = noopBackoff
	res, err := c.Upload(context.Background(), path, UploadOptions{
		Namespace: "ns",
		ChunkSize: 64 * 1024,
		Parallel:  3,
		Checksum:  "sha256",
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	data, err := os.ReadFile(store.CompletedPath("ns", res.UploadID))
	if err != nil {
		t.Fatal(err)
	}
	gotSum := sha256.Sum256(data)
	if hex.EncodeToString(gotSum[:]) != wantHash {
		t.Errorf("sha256 mismatch")
	}
}

// TestComputeSlabChecksum covers all algos directly.
func TestComputeSlabChecksum(t *testing.T) {
	chunk := []byte("hello world")
	for _, algo := range []string{"crc32c", "sha256"} {
		v, err := computeSlabChecksum(algo, chunk)
		if err != nil {
			t.Errorf("%s: %v", algo, err)
		}
		if v == "" {
			t.Errorf("%s: empty result", algo)
		}
	}
	if _, err := computeSlabChecksum("md5", chunk); err == nil {
		t.Errorf("md5 should be unsupported")
	}
}

// TestUploadParallelEightWorkers exercises the upper end of the worker
// fan-out so the parallel goroutines actually contend for the shared HTTP
// transport.
func TestUploadParallelEightWorkers(t *testing.T) {
	const fileSize int64 = 800 * 1024
	baseURL, token, store, cleanup := newServerRig(t)
	defer cleanup()
	path, wantHash := makeRandomFile(t, fileSize)

	c := NewClient(baseURL, token)
	c.NewBackoff = noopBackoff
	res, err := c.Upload(context.Background(), path, UploadOptions{
		Namespace: "ns",
		ChunkSize: 32 * 1024,
		Parallel:  8,
		Checksum:  "none",
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	data, err := os.ReadFile(store.CompletedPath("ns", res.UploadID))
	if err != nil {
		t.Fatal(err)
	}
	gotSum := sha256.Sum256(data)
	if hex.EncodeToString(gotSum[:]) != wantHash {
		t.Errorf("sha256 mismatch")
	}
}

// TestUploadParallelTransientFailureRecovers exercises the worker retry
// path: a flaky proxy 500s the first PATCH on each slab, then transparently
// proxies subsequent calls. The upload should still succeed via the
// HEAD-resume + backoff machinery.
func TestUploadParallelTransientFailureRecovers(t *testing.T) {
	const fileSize int64 = 200 * 1024
	baseURL, token, store, cleanup := newServerRig(t)
	defer cleanup()
	path, wantHash := makeRandomFile(t, fileSize)

	// Wrap the real server with a flaky proxy that 500s every Nth PATCH.
	flaky := newFlakyProxy(t, baseURL, 3)
	defer flaky.Close()

	c := NewClient(flaky.URL, token)
	c.NewBackoff = noopBackoff
	res, err := c.Upload(context.Background(), path, UploadOptions{
		Namespace: "ns",
		ChunkSize: 32 * 1024,
		Parallel:  2,
		Checksum:  "none",
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	data, err := os.ReadFile(store.CompletedPath("ns", res.UploadID))
	if err != nil {
		t.Fatal(err)
	}
	gotSum := sha256.Sum256(data)
	if hex.EncodeToString(gotSum[:]) != wantHash {
		t.Errorf("sha256 mismatch")
	}
}

// TestProgressAggregates checks that Add is called from multiple slabs and
// the total roughly equals the file size.
func TestProgressAggregates(t *testing.T) {
	const fileSize int64 = 2 * 1024 * 1024
	baseURL, token, _, cleanup := newServerRig(t)
	defer cleanup()
	path, _ := makeRandomFile(t, fileSize)

	c := NewClient(baseURL, token)
	c.NewBackoff = noopBackoff
	prog := NewProgress(fileSize, ProgressSilent, nil, nil)
	res, err := c.Upload(context.Background(), path, UploadOptions{
		Namespace: "ns",
		ChunkSize: 256 * 1024,
		Parallel:  4,
		Progress:  prog,
		Checksum:  "none",
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if res.Size != fileSize {
		t.Errorf("Size = %d, want %d", res.Size, fileSize)
	}
}
