package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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
	n, err := s.AppendChunk(context.Background(), "alpha", "u1", body, 100, true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 11 {
		t.Errorf("wrote %d, want 11", n)
	}
	if err := s.Complete(context.Background(), "alpha", "u1"); err != nil {
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

// captureSlog returns a fresh slog.Logger that writes JSONL to buf, plus the
// buf for read-back. Used by the structured-timing tests below.
func captureSlog(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), &buf
}

// readLogEvents parses each JSON line in buf and returns the parsed maps,
// keyed by the "msg" field for cheap lookup.
func readLogEvents(t *testing.T, buf *bytes.Buffer) map[string]map[string]any {
	t.Helper()
	out := make(map[string]map[string]any)
	for {
		line, err := buf.ReadBytes('\n')
		if len(line) > 0 {
			var ev map[string]any
			if jerr := json.Unmarshal(line, &ev); jerr != nil {
				t.Fatalf("unmarshal log line %q: %v", line, jerr)
			}
			if msg, _ := ev["msg"].(string); msg != "" {
				out[msg] = ev
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func newTestStoreWithLogger(t *testing.T, logger *slog.Logger) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStoreWithLogger(dir, logger)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreCompleteEmitsTimingLogs(t *testing.T) {
	logger, buf := captureSlog(t)
	s := newTestStoreWithLogger(t, logger)

	info := sampleInfo("u1")
	if err := s.Create(info); err != nil {
		t.Fatal(err)
	}
	body := strings.NewReader("hello world") // 11 bytes (matches sampleInfo.Size)
	if _, err := s.AppendChunk(context.Background(), "alpha", "u1", body, 100, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(context.Background(), "alpha", "u1"); err != nil {
		t.Fatal(err)
	}

	events := readLogEvents(t, buf)

	want := []string{
		"ferry.chunk_write",
		"ferry.chunk_fsync",
		"ferry.partial_open",
		"ferry.partial_fsync",
		"ferry.partial_rename",
		"ferry.parent_dir_fsync",
		"ferry.info_load",
		"ferry.info_completed_write",
		"ferry.info_completed_fsync",
		"ferry.info_completed_rename",
		"ferry.info_completed_dir_fsync",
		"ferry.upload_complete",
	}
	for _, name := range want {
		ev, ok := events[name]
		if !ok {
			t.Errorf("missing log event %q", name)
			continue
		}
		// elapsed is rendered as a number (nanoseconds) by JSONHandler.
		elapsed, ok := ev["elapsed"].(float64)
		if !ok {
			t.Errorf("event %q has no numeric elapsed: %#v", name, ev["elapsed"])
			continue
		}
		if elapsed <= 0 {
			t.Errorf("event %q elapsed = %v, want > 0", name, elapsed)
		}
		if got, _ := ev["upload_id"].(string); got != "u1" {
			t.Errorf("event %q upload_id = %q, want u1", name, got)
		}
		if got, _ := ev["namespace"].(string); got != "alpha" {
			t.Errorf("event %q namespace = %q, want alpha", name, got)
		}
		// happy-path: every entry should be at INFO level.
		if got, _ := ev["level"].(string); got != "INFO" {
			t.Errorf("event %q level = %q, want INFO", name, got)
		}
	}

	// chunk_write is_final=true (this PATCH finishes the upload) and
	// carries the byte count alongside elapsed in a single event.
	if ev := events["ferry.chunk_write"]; ev != nil {
		if got, _ := ev["is_final"].(bool); !got {
			t.Errorf("chunk_write is_final = %v, want true", got)
		}
		if got, _ := ev["bytes"].(float64); got != 11 {
			t.Errorf("chunk_write bytes = %v, want 11", got)
		}
	}
	// upload_complete carries the byte count.
	if ev := events["ferry.upload_complete"]; ev != nil {
		if got, _ := ev["bytes"].(float64); got != 11 {
			t.Errorf("upload_complete bytes = %v, want 11", got)
		}
	}
}

func TestStoreCompleteLogsWarnOnError(t *testing.T) {
	logger, buf := captureSlog(t)
	s := newTestStoreWithLogger(t, logger)

	// Complete on an upload that has no .partial - the partial_open step
	// must fail, emit a WARN entry with an error attribute, and the
	// upload_complete summary must also be at WARN.
	err := s.Complete(context.Background(), "alpha", "ghost")
	if err == nil {
		t.Fatalf("Complete on missing partial should error")
	}

	events := readLogEvents(t, buf)
	open, ok := events["ferry.partial_open"]
	if !ok {
		t.Fatalf("missing ferry.partial_open event")
	}
	if got, _ := open["level"].(string); got != "WARN" {
		t.Errorf("partial_open level = %q, want WARN", got)
	}
	if got, _ := open["error"].(string); got == "" {
		t.Errorf("partial_open missing error attribute")
	}
	summary, ok := events["ferry.upload_complete"]
	if !ok {
		t.Fatalf("missing ferry.upload_complete event")
	}
	if got, _ := summary["level"].(string); got != "WARN" {
		t.Errorf("upload_complete level = %q, want WARN", got)
	}
}
