package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
)

func TestAdaptiveSizer_Disabled(t *testing.T) {
	s := newAdaptiveSizer(4*1024*1024, false)
	if got := s.size(); got != 4*1024*1024 {
		t.Fatalf("initial size: got %d want %d", got, 4*1024*1024)
	}
	// Even with extremely slow throughput, disabled sizer should not shrink.
	s.observe(1024, time.Second)
	if got := s.size(); got != 4*1024*1024 {
		t.Fatalf("disabled sizer changed: got %d", got)
	}
}

func TestAdaptiveSizer_HalveOnSlow(t *testing.T) {
	s := newAdaptiveSizer(4*1024*1024, true)
	// 100 KB / 1s = 100 KB/s, well below SlowThroughputBytesPerSec.
	s.observe(100*1024, time.Second)
	if got, want := s.size(), int64(2*1024*1024); got != want {
		t.Fatalf("after slow observe: got %d want %d", got, want)
	}
	s.observe(100*1024, time.Second)
	if got, want := s.size(), int64(1*1024*1024); got != want {
		t.Fatalf("after second slow observe: got %d want %d", got, want)
	}
}

func TestAdaptiveSizer_FloorAt256KiB(t *testing.T) {
	s := newAdaptiveSizer(4*1024*1024, true)
	// Hammer slow observations; should bottom out at MinAdaptiveChunkBytes.
	for i := 0; i < 20; i++ {
		s.observe(1024, time.Second)
	}
	if got, want := s.size(), int64(MinAdaptiveChunkBytes); got != want {
		t.Fatalf("floor: got %d want %d", got, want)
	}
}

func TestAdaptiveSizer_DoubleOnFast(t *testing.T) {
	s := newAdaptiveSizer(4*1024*1024, true)
	// Shrink first.
	s.observe(100*1024, time.Second) // -> 2 MiB
	s.observe(100*1024, time.Second) // -> 1 MiB
	if s.size() != 1*1024*1024 {
		t.Fatalf("setup: expected 1 MiB, got %d", s.size())
	}
	// Now simulate a fast PATCH: 8 MiB / 1s.
	s.observe(8*1024*1024, time.Second)
	if got, want := s.size(), int64(2*1024*1024); got != want {
		t.Fatalf("after fast observe: got %d want %d", got, want)
	}
	s.observe(8*1024*1024, time.Second)
	if got, want := s.size(), int64(4*1024*1024); got != want {
		t.Fatalf("after second fast observe: got %d want %d", got, want)
	}
}

func TestAdaptiveSizer_CeilingAtConfigured(t *testing.T) {
	const ceiling = int64(2 * 1024 * 1024)
	s := newAdaptiveSizer(ceiling, true)
	// Many fast observations should never exceed the ceiling.
	for i := 0; i < 20; i++ {
		s.observe(100*1024*1024, time.Second)
	}
	if got := s.size(); got != ceiling {
		t.Fatalf("ceiling: got %d want %d", got, ceiling)
	}
}

func TestAdaptiveSizer_MidRangeUnchanged(t *testing.T) {
	s := newAdaptiveSizer(4*1024*1024, true)
	// 2 MiB/s -> between SlowThroughputBytesPerSec (1 MiB/s) and
	// FastThroughputBytesPerSec (4 MiB/s); size should not change.
	s.observe(2*1024*1024, time.Second)
	if got, want := s.size(), int64(4*1024*1024); got != want {
		t.Fatalf("mid-range: got %d want %d", got, want)
	}
}

func TestAdaptiveSizer_ZeroOrNegativeIgnored(t *testing.T) {
	s := newAdaptiveSizer(4*1024*1024, true)
	s.observe(0, time.Second)
	s.observe(1024, 0)
	s.observe(-1, time.Second)
	if got, want := s.size(), int64(4*1024*1024); got != want {
		t.Fatalf("zero/neg observe should be no-op: got %d", got)
	}
}

func TestAdaptiveSizer_Nil(t *testing.T) {
	var s *adaptiveSizer
	if got := s.size(); got != 0 {
		t.Fatalf("nil sizer size: got %d want 0", got)
	}
	// Should not panic.
	s.observe(1024, time.Second)
}

func TestAdaptiveSizer_FloorAboveCeiling(t *testing.T) {
	// If the configured ceiling is below the default floor, the floor
	// drops to match so the sizer never returns 0.
	s := newAdaptiveSizer(64*1024, true)
	for i := 0; i < 10; i++ {
		s.observe(1, time.Second)
	}
	if got, want := s.size(), int64(64*1024); got != want {
		t.Fatalf("floor below ceiling: got %d want %d", got, want)
	}
}

func TestIsResponseHeaderTimeout(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("timeout awaiting response headers"), true},
		{errors.New("net/http: timeout awaiting response headers"), true},
		{errors.New("connection refused"), false},
	}
	for _, c := range cases {
		if got := isResponseHeaderTimeout(c.err); got != c.want {
			t.Errorf("isResponseHeaderTimeout(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// TestPatchOne_ResponseHeaderTimeoutErrorMessage uses a server that accepts
// the PATCH body but never sends response headers, then verifies the error
// surfaced by Upload mentions the timeout duration and chunk-size hint.
func TestPatchOne_ResponseHeaderTimeoutErrorMessage(t *testing.T) {
	// Build a handler that hangs forever after reading the body. Close the
	// hang channel BEFORE httptest.Close so the in-flight handler returns
	// and the server can shut down cleanly.
	hang := make(chan struct{})
	defer close(hang)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/uploads/alpha", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Tus-Resumable", "1.0.0")
		w.Header().Set("Location", "/v1/uploads/alpha/test-id")
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/v1/uploads/alpha/test-id", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("Tus-Resumable", "1.0.0")
			w.Header().Set("Upload-Offset", "0")
			w.Header().Set("Upload-Length", "1024")
			w.WriteHeader(http.StatusOK)
			return
		case http.MethodPatch:
			// Drain body, then hang.
			_, _ = readAll(r.Body)
			<-hang
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	src, _ := writeRandom(t, 1024)
	c := NewClient(ts.URL, "any")
	// Override transport timeout to something tiny so the test runs fast.
	c.HTTP.Transport.(*http.Transport).ResponseHeaderTimeout = 50 * time.Millisecond
	// Tiny backoff so the retry loop exhausts quickly.
	c.NewBackoff = func() backoff.BackOff {
		return backoff.WithMaxRetries(backoff.NewConstantBackOff(0), 2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := c.Upload(ctx, src, UploadOptions{
		Namespace: "alpha",
		ChunkSize: 1024,
	})
	if err == nil {
		t.Fatal("expected error from hung server")
	}
	// Expect the timeout wrapper to have fired and produced the actionable
	// hint at least once during the retry loop. The retry budget exhausts
	// before the outer context deadline given the tiny transport timeout.
	if !strings.Contains(err.Error(), "consider --chunk-size") {
		t.Fatalf("expected timeout error message to include --chunk-size hint, got: %v", err)
	}
}

// readAll is a tiny helper to read+discard a body without pulling io into
// every test. Returns total bytes consumed.
func readAll(r interface{ Read(p []byte) (int, error) }) (int64, error) {
	var total int64
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		total += int64(n)
		if err != nil {
			if err.Error() == "EOF" {
				return total, nil
			}
			return total, err
		}
	}
}
