package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestServerRunIntegration spins up a real Server.Run on an OS-assigned
// port (":0"), performs POST + PATCH + HEAD + DELETE end-to-end, verifies
// the file lands on disk, then cancels the context to shut down cleanly.
func TestServerRunIntegration(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	tokensPath := filepath.Join(dir, "tokens.json")

	token := "integration-token"
	tokensBody := `{"tokens":{"` + HashToken(token) + `":{"namespaces":["alpha"]}}}`
	if err := os.WriteFile(tokensPath, []byte(tokensBody), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		ListenAddr: "127.0.0.1:0", // ephemeral port
		DataDir:    dataDir,
		TokensPath: tokensPath,
	}
	cfg.ApplyDefaults()
	cfg.ListenAddr = "127.0.0.1:0"

	srv, err := New(cfg, "test-version", nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, "test-version") }()

	// Wait for the listener to become ready (Addr populated by Run).
	deadline := time.After(3 * time.Second)
	for srv.Addr() == "" {
		select {
		case <-deadline:
			t.Fatal("server didn't bind")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	base := "http://" + srv.Addr()

	// Wait for /health to respond (server.Run goroutine may still be
	// installing handlers).
	for {
		resp, err := http.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			break
		}
		select {
		case <-deadline:
			t.Fatalf("health never responded: %v", err)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// POST.
	req, _ := http.NewRequest("POST", base+"/v1/uploads/alpha", nil)
	req.Header.Set("Tus-Resumable", tusVersion)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Upload-Length", "5")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	id := loc[len("/v1/uploads/alpha/"):]

	// PATCH full body -> completes the upload.
	req2, _ := http.NewRequest("PATCH", base+loc, bytes.NewReader([]byte("hello")))
	req2.Header.Set("Tus-Resumable", tusVersion)
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set("Upload-Offset", "0")
	req2.Header.Set("Content-Type", "application/offset+octet-stream")
	req2.ContentLength = 5
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("PATCH status = %d", resp2.StatusCode)
	}
	if resp2.Header.Get("Upload-Offset") != "5" {
		t.Errorf("offset = %q", resp2.Header.Get("Upload-Offset"))
	}

	// Verify the completed file is on disk after atomic-rename.
	completed := filepath.Join(dataDir, "alpha", id)
	body, err := os.ReadFile(completed) //nolint:gosec // test-only path
	if err != nil {
		t.Fatalf("read completed file: %v", err)
	}
	if string(body) != "hello" {
		t.Errorf("on-disk = %q, want %q", body, "hello")
	}

	// HEAD - completed upload reports offset == size.
	req3, _ := http.NewRequest("HEAD", base+loc, nil)
	req3.Header.Set("Tus-Resumable", tusVersion)
	req3.Header.Set("Authorization", "Bearer "+token)
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("HEAD status = %d", resp3.StatusCode)
	}
	if resp3.Header.Get("Upload-Offset") != "5" {
		t.Errorf("HEAD offset = %q", resp3.Header.Get("Upload-Offset"))
	}

	// /health body shape.
	hresp, err := http.Get(base + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer hresp.Body.Close()
	hbody, _ := io.ReadAll(hresp.Body)
	var got map[string]any
	if err := json.Unmarshal(hbody, &got); err != nil {
		t.Fatal(err)
	}
	if got["ok"] != true || got["version"] != "test-version" {
		t.Errorf("health body = %v", got)
	}

	// DELETE removes the upload + sidecar.
	req4, _ := http.NewRequest("DELETE", base+loc, nil)
	req4.Header.Set("Tus-Resumable", tusVersion)
	req4.Header.Set("Authorization", "Bearer "+token)
	resp4, err := http.DefaultClient.Do(req4)
	if err != nil {
		t.Fatal(err)
	}
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE status = %d", resp4.StatusCode)
	}
	if _, err := os.Stat(completed); !os.IsNotExist(err) {
		t.Errorf("completed file still exists after DELETE: %v", err)
	}

	// Shutdown.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not exit after cancel")
	}
}

func TestServerNewMissingTokens(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		ListenAddr: "127.0.0.1:0",
		DataDir:    filepath.Join(dir, "data"),
		TokensPath: filepath.Join(dir, "tokens-missing.json"),
	}
	cfg.ApplyDefaults()
	if _, err := New(cfg, "v", nil); err == nil {
		t.Fatal("expected error from missing tokens file")
	}
}

func TestParseMetadataValidPair(t *testing.T) {
	// "filename" base64("hello.txt"=aGVsbG8udHh0)
	got := parseMetadata("filename aGVsbG8udHh0")
	if got["filename"] != "hello.txt" {
		t.Errorf("got %v", got)
	}
}

func TestParseMetadataMultiplePairs(t *testing.T) {
	got := parseMetadata("a YQ==, b YmI=")
	if got["a"] != "a" || got["b"] != "bb" {
		t.Errorf("got %v", got)
	}
}

func TestParseMetadataEmpty(t *testing.T) {
	if got := parseMetadata(""); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestParseMetadataKeyOnly(t *testing.T) {
	got := parseMetadata("flag")
	if _, ok := got["flag"]; !ok {
		t.Errorf("got %v", got)
	}
}

func TestParseMetadataBadBase64Skipped(t *testing.T) {
	// Bad base64 silently skipped; key entirely absent.
	got := parseMetadata("k !!!notbase64")
	if _, ok := got["k"]; ok {
		t.Errorf("bad base64 not skipped, got %v", got)
	}
}

func TestStatusForUnknown(t *testing.T) {
	if got := statusFor(io.EOF); got != http.StatusInternalServerError {
		t.Errorf("statusFor(unknown) = %d, want 500", got)
	}
}

func TestParseUploadPathVariants(t *testing.T) {
	cases := []struct {
		path string
		ns   string
		id   string
		ok   bool
	}{
		{"/v1/uploads/alpha", "alpha", "", true},
		{"/v1/uploads/alpha/", "alpha", "", true},
		{"/v1/uploads/alpha/abc123", "alpha", "abc123", true},
		{"/v1/uploads/alpha/abc/extra", "", "", false},
		{"/v1/uploads/", "", "", false},
		{"/v2/uploads/alpha", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		ns, id, ok := parseUploadPath(tc.path)
		if ns != tc.ns || id != tc.id || ok != tc.ok {
			t.Errorf("parseUploadPath(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.path, ns, id, ok, tc.ns, tc.id, tc.ok)
		}
	}
}

func TestUploadLocationFormat(t *testing.T) {
	got := uploadLocation("alpha", "01HXYZ")
	want := "/v1/uploads/alpha/01HXYZ"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNewIDIsULIDLength(t *testing.T) {
	id := newID()
	if len(id) != 26 {
		t.Errorf("ULID length = %d, want 26", len(id))
	}
}

// TestPatchResumesAfterPartial verifies that a second PATCH after a partial
// first PATCH resumes from the on-disk size (the canonical offset).
func TestPatchResumesAfterPartial(t *testing.T) {
	r := newRig(t)
	loc := r.createUpload(t, "alpha", 10, "")

	// First PATCH: 4 bytes.
	req1 := r.newReq(t, "PATCH", pathOf(loc), bytes.NewReader([]byte("abcd")))
	req1.Header.Set("Upload-Offset", "0")
	req1.Header.Set("Content-Type", "application/offset+octet-stream")
	req1.ContentLength = 4
	resp1 := r.do(t, req1)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusNoContent {
		t.Fatalf("first PATCH = %d", resp1.StatusCode)
	}
	if resp1.Header.Get("Upload-Offset") != "4" {
		t.Errorf("offset = %q", resp1.Header.Get("Upload-Offset"))
	}

	// Second PATCH at offset 4 with 6 bytes -> completes.
	req2 := r.newReq(t, "PATCH", pathOf(loc), bytes.NewReader([]byte("efghij")))
	req2.Header.Set("Upload-Offset", "4")
	req2.Header.Set("Content-Type", "application/offset+octet-stream")
	req2.ContentLength = 6
	resp2 := r.do(t, req2)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("second PATCH = %d", resp2.StatusCode)
	}
	if resp2.Header.Get("Upload-Offset") != "10" {
		t.Errorf("offset = %q", resp2.Header.Get("Upload-Offset"))
	}
}

func TestParseUploadLengthMissing(t *testing.T) {
	r := newRig(t)
	req := r.newReq(t, "POST", "/v1/uploads/alpha", nil)
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPostNegativeUploadLength(t *testing.T) {
	r := newRig(t)
	req := r.newReq(t, "POST", "/v1/uploads/alpha", nil)
	req.Header.Set("Upload-Length", "-1")
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestDeleteMissing(t *testing.T) {
	r := newRig(t)
	req := r.newReq(t, "DELETE", "/v1/uploads/alpha/ghost", nil)
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	r := newRig(t)
	loc := r.createUpload(t, "alpha", 5, "")
	req := r.newReq(t, "PUT", pathOf(loc), nil)
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestValidNamespace(t *testing.T) {
	cases := map[string]bool{
		"alpha":                        true,
		"alpha-1":                      true,
		"alpha_beta":                   true,
		"":                             false,
		"..":                           false,
		"a/b":                          false,
		"a b":                          false,
		"with.dot":                     false,
		"x" + string(make([]byte, 64)): false, // > 64
	}
	for s, want := range cases {
		if got := validNamespace(s); got != want {
			t.Errorf("validNamespace(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestValidIdemKey(t *testing.T) {
	if !validIdemKey("post.user.123") {
		t.Errorf("dotted idem key rejected")
	}
	if validIdemKey("../escape") {
		t.Errorf("traversal idem key accepted")
	}
	if validIdemKey("") {
		t.Errorf("empty idem key accepted")
	}
}

func TestPostRejectsTraversalNamespace(t *testing.T) {
	r := newRig(t)
	req, _ := http.NewRequest("POST", r.srv.URL+"/v1/uploads/..", nil)
	req.Header.Set("Tus-Resumable", tusVersion)
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Upload-Length", "1")
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("traversal namespace status = %d, want 404", resp.StatusCode)
	}
}

func TestPostRejectsBadIdempotencyKey(t *testing.T) {
	r := newRig(t)
	req := r.newReq(t, "POST", "/v1/uploads/alpha", nil)
	req.Header.Set("Upload-Length", "1")
	req.Header.Set("Idempotency-Key", "../escape")
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad idem status = %d, want 400", resp.StatusCode)
	}
}

// keep imports alive
var _ = strconv.Itoa
