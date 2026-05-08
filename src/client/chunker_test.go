package client

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func writeRandomFile(t *testing.T, size int64) (string, []byte) {
	t.Helper()
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "data.bin")
	if err := os.WriteFile(p, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	return p, buf
}

func TestChunker_RoundTrip17MiBIn4MiBChunks(t *testing.T) {
	const size = 17 * 1024 * 1024
	const chunk = 4 * 1024 * 1024
	path, want := writeRandomFile(t, size)

	ch, err := NewChunker(path, chunk)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	if ch.Size() != size {
		t.Fatalf("Size = %d, want %d", ch.Size(), size)
	}

	var got bytes.Buffer
	expectChunks := []int64{chunk, chunk, chunk, chunk, size - 4*chunk}
	idx := 0
	for {
		r, off, n, err := ch.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if idx >= len(expectChunks) {
			t.Fatalf("more chunks than expected at idx %d", idx)
		}
		if n != expectChunks[idx] {
			t.Fatalf("chunk %d: len %d, want %d", idx, n, expectChunks[idx])
		}
		_ = off
		copied, err := io.Copy(&got, r)
		if err != nil {
			t.Fatal(err)
		}
		if copied != n {
			t.Fatalf("chunk %d: copied %d, want %d", idx, copied, n)
		}
		idx++
	}
	if !ch.Done() {
		t.Fatal("Done = false after EOF")
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatal("round-trip bytes differ")
	}
	// Last chunk size is 17 MiB - 16 MiB = 1 MiB.
	if expectChunks[4] != 1024*1024 {
		t.Fatalf("last expected chunk size %d", expectChunks[4])
	}
}

func TestChunker_SeekToResume(t *testing.T) {
	path, want := writeRandomFile(t, 10*1024*1024)

	ch, err := NewChunker(path, 4*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	// Read first chunk.
	r, _, n, err := ch.Next()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, r)
	if n != 4*1024*1024 {
		t.Fatalf("first chunk size %d", n)
	}

	// Seek back to byte 1024 and read 4 KiB. Verify it matches the source.
	if err := ch.SeekTo(1024); err != nil {
		t.Fatal(err)
	}
	r2, err := ch.ReaderAt(1024, 4096)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want[1024:1024+4096]) {
		t.Fatal("ReaderAt returned wrong bytes after seek")
	}
	if ch.Offset() != 1024 {
		t.Fatalf("Offset = %d, want 1024", ch.Offset())
	}
}

func TestChunker_BadInputs(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.bin")
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewChunker(p, 0); err == nil {
		t.Fatal("expected error for chunk size 0")
	}
	if _, err := NewChunker(p, MaxChunkSizeBytes+1); err == nil {
		t.Fatal("expected error for chunk size > max")
	}
	if _, err := NewChunker(filepath.Join(dir, "missing"), 1024); err == nil {
		t.Fatal("expected error for missing file")
	}

	ch, err := NewChunker(p, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	if err := ch.SeekTo(-1); err == nil {
		t.Fatal("expected error for negative seek")
	}
	if err := ch.SeekTo(ch.Size() + 1); err == nil {
		t.Fatal("expected error for seek past size")
	}
	if _, err := ch.ReaderAt(0, ch.Size()+1); err == nil {
		t.Fatal("expected error for reader past size")
	}
}

func TestChunker_ZeroByteFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.bin")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ch, err := NewChunker(p, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	if !ch.Done() {
		t.Fatal("expected Done=true on empty file")
	}
	if _, _, _, err := ch.Next(); err != io.EOF {
		t.Fatalf("expected EOF on empty Next, got %v", err)
	}
}
