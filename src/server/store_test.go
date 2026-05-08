package server

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sampleInfo(id string) Info {
	now := time.Now().UTC().Truncate(time.Second)
	return Info{
		ID:        id,
		Namespace: "alpha",
		Size:      11,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}
}

func TestStoreCreateAndLoadInfo(t *testing.T) {
	s := newTestStore(t)
	info := sampleInfo("u1")
	if err := s.Create(info); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadInfo("alpha", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != "u1" || loaded.Size != 11 {
		t.Errorf("round-trip mismatch: %+v", loaded)
	}
	// .partial exists, empty.
	st, err := os.Stat(s.partialPath("alpha", "u1"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 0 {
		t.Errorf("partial size = %d, want 0", st.Size())
	}
}

func TestStoreCurrentOffset(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create(sampleInfo("u1")); err != nil {
		t.Fatal(err)
	}
	off, err := s.CurrentOffset("alpha", "u1")
	if err != nil || off != 0 {
		t.Errorf("offset = %d, err = %v", off, err)
	}
	if _, err := s.CurrentOffset("alpha", "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing offset err = %v, want ErrNotFound", err)
	}
}

func TestStoreAppendAndComplete(t *testing.T) {
	s := newTestStore(t)
	info := sampleInfo("u1")
	if err := s.Create(info); err != nil {
		t.Fatal(err)
	}
	body := strings.NewReader("hello world") // 11 bytes
	n, err := s.AppendChunk("alpha", "u1", body, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 11 {
		t.Errorf("wrote %d, want 11", n)
	}
	if err := s.Complete("alpha", "u1"); err != nil {
		t.Fatal(err)
	}
	// Atomic-rename: .partial gone, completed exists.
	if _, err := os.Stat(s.partialPath("alpha", "u1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".partial still exists after complete: %v", err)
	}
	b, err := os.ReadFile(s.completedPath("alpha", "u1"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, []byte("hello world")) {
		t.Errorf("completed body = %q", b)
	}
	// Sidecar has completed_at.
	loaded, err := s.LoadInfo("alpha", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CompletedAt == nil {
		t.Errorf("CompletedAt not set")
	}
}

func TestStoreDelete(t *testing.T) {
	s := newTestStore(t)
	if err := s.Create(sampleInfo("u1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("alpha", "u1"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		s.partialPath("alpha", "u1"),
		s.infoPath("alpha", "u1"),
	} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after delete: %v", p, err)
		}
	}
}

func TestStoreIdempotency(t *testing.T) {
	s := newTestStore(t)
	info := sampleInfo("u1")
	info.IdempotencyKey = "dedupe-1"
	if err := s.Create(info); err != nil {
		t.Fatal(err)
	}
	got, err := s.LookupIdem("alpha", "dedupe-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "u1" {
		t.Errorf("LookupIdem = %q, want u1", got)
	}
	got2, err := s.LookupIdem("alpha", "missing")
	if err != nil || got2 != "" {
		t.Errorf("LookupIdem missing = %q, err=%v", got2, err)
	}
}

func TestStoreLoadInfoMissing(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.LoadInfo("alpha", "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("LoadInfo missing = %v, want ErrNotFound", err)
	}
}

func TestStoreAvailableBytes(t *testing.T) {
	s := newTestStore(t)
	avail, err := s.AvailableBytes()
	if err != nil {
		t.Fatal(err)
	}
	if avail <= 0 {
		t.Errorf("AvailableBytes = %d, want > 0", avail)
	}
}

func TestStoreInfoAtomicOnDisk(t *testing.T) {
	// Verify the sidecar JSON parses to the same struct on round-trip.
	s := newTestStore(t)
	info := sampleInfo("u1")
	info.Metadata = map[string]string{"filename": "test.bin"}
	if err := s.Create(info); err != nil {
		t.Fatal(err)
	}
	// .info.tmp must be gone after Create.
	tmp := filepath.Join(s.nsDir("alpha"), "u1.info.tmp")
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".info.tmp still exists: %v", err)
	}
	loaded, err := s.LoadInfo("alpha", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Metadata["filename"] != "test.bin" {
		t.Errorf("metadata round-trip lost: %+v", loaded.Metadata)
	}
}
