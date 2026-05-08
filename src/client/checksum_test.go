package client

import (
	"bytes"
	"context"
	"encoding/hex"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClient_ChecksumDefaultIsCRC32C(t *testing.T) {
	ts, dataDir, token := startTestServer(t)
	src, want := writeRandom(t, 5*1024*1024) // 5 MiB, two PATCHes at 4 MiB chunks

	c := NewClient(ts.URL, token)
	// Default Checksum (empty) should resolve to crc32c. Server validates.
	res, err := c.Upload(context.Background(), src, UploadOptions{
		Namespace: "alpha",
		ChunkSize: 4 * 1024 * 1024,
		Progress:  NewProgress(int64(len(want)), ProgressSilent, nil, nil),
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dataDir, "alpha", res.UploadID))
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("uploaded bytes differ from source")
	}
}

func TestClient_ChecksumSHA256(t *testing.T) {
	ts, dataDir, token := startTestServer(t)
	src, want := writeRandom(t, 1*1024*1024)

	c := NewClient(ts.URL, token)
	res, err := c.Upload(context.Background(), src, UploadOptions{
		Namespace: "alpha",
		ChunkSize: 4 * 1024 * 1024,
		Checksum:  "sha256",
		Progress:  NewProgress(int64(len(want)), ProgressSilent, nil, nil),
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dataDir, "alpha", res.UploadID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("uploaded bytes differ from source")
	}
}

func TestClient_ChecksumNoneSkipsHeader(t *testing.T) {
	ts, dataDir, token := startTestServer(t)
	src, want := writeRandom(t, 1*1024*1024)

	c := NewClient(ts.URL, token)
	res, err := c.Upload(context.Background(), src, UploadOptions{
		Namespace: "alpha",
		ChunkSize: 4 * 1024 * 1024,
		Checksum:  "none",
		Progress:  NewProgress(int64(len(want)), ProgressSilent, nil, nil),
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dataDir, "alpha", res.UploadID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("uploaded bytes differ from source")
	}
}

func TestClient_ChecksumUnsupportedAlgoFailsClient(t *testing.T) {
	ts, _, token := startTestServer(t)
	src, _ := writeRandom(t, 1024)
	c := NewClient(ts.URL, token)
	_, err := c.Upload(context.Background(), src, UploadOptions{
		Namespace: "alpha",
		ChunkSize: 4 * 1024 * 1024,
		Checksum:  "md5",
		Progress:  NewProgress(1024, ProgressSilent, nil, nil),
	})
	if err == nil {
		t.Fatal("expected error for unsupported algo")
	}
	if !strings.Contains(err.Error(), "unsupported checksum algo") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestComputeChunkChecksumCRC32C(t *testing.T) {
	chunk := []byte("ferry chunk")
	got, err := computeChunkChecksum("crc32c", chunk)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(got, " ", 2)
	if parts[0] != "crc32c" {
		t.Fatalf("algo prefix = %q", parts[0])
	}
	expected := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	_, _ = expected.Write(chunk)
	if parts[1] != hex.EncodeToString(expected.Sum(nil)) {
		t.Fatalf("hash mismatch: got %q want %x", parts[1], expected.Sum(nil))
	}
}
