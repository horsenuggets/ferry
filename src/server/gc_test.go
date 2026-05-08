package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// gcFixture creates a Store rooted in a temp dir and returns it along with the
// usual GC handle. CompletedTTL/IncompleteTTL default to 1h/24h; tests
// override `now` instead of fiddling with TTLs.
func gcFixture(t *testing.T) (*Store, *GC) {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	gc := NewGC(GCConfig{
		Store:         store,
		Locker:        NewLocker(),
		Interval:      time.Hour,
		CompletedTTL:  time.Hour,
		IncompleteTTL: 24 * time.Hour,
	})
	return store, gc
}

func writeBytes(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestGC_RemovesOldCompletedUpload(t *testing.T) {
	store, gc := gcFixture(t)

	completedAt := time.Now().Add(-2 * time.Hour).UTC()
	info := Info{
		ID:          "old-completed",
		Namespace:   "ns",
		Size:        4,
		CreatedAt:   completedAt.Add(-time.Minute),
		ExpiresAt:   completedAt.Add(time.Hour),
		CompletedAt: &completedAt,
	}
	if err := store.Create(info); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Promote it to "completed" by renaming the partial.
	if err := os.Rename(store.partialPath("ns", "old-completed"), store.completedPath("ns", "old-completed")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	// Re-write info with completed_at set.
	if err := store.writeInfo(info); err != nil {
		t.Fatalf("writeInfo: %v", err)
	}
	writeBytes(t, store.completedPath("ns", "old-completed"), []byte("data"))

	gc.SweepOnce(context.Background())

	if store.HasCompleted("ns", "old-completed") {
		t.Fatalf("expected old completed file to be gone")
	}
	if _, err := store.LoadInfo("ns", "old-completed"); err == nil {
		t.Fatalf("expected info to be gone")
	}
}

func TestGC_KeepsFreshCompletedUpload(t *testing.T) {
	store, gc := gcFixture(t)

	completedAt := time.Now().Add(-1 * time.Minute).UTC()
	info := Info{
		ID:          "fresh-completed",
		Namespace:   "ns",
		Size:        4,
		CreatedAt:   completedAt.Add(-time.Minute),
		ExpiresAt:   completedAt.Add(time.Hour),
		CompletedAt: &completedAt,
	}
	if err := store.Create(info); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.Rename(store.partialPath("ns", "fresh-completed"), store.completedPath("ns", "fresh-completed")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := store.writeInfo(info); err != nil {
		t.Fatalf("writeInfo: %v", err)
	}
	writeBytes(t, store.completedPath("ns", "fresh-completed"), []byte("data"))

	gc.SweepOnce(context.Background())

	if !store.HasCompleted("ns", "fresh-completed") {
		t.Fatalf("expected fresh completed file to remain")
	}
}

func TestGC_RemovesIncompleteUploadPastExpiry(t *testing.T) {
	store, gc := gcFixture(t)

	// expires_at is in the past; no completed_at.
	info := Info{
		ID:        "stale-incomplete",
		Namespace: "ns",
		Size:      100,
		CreatedAt: time.Now().Add(-48 * time.Hour).UTC(),
		ExpiresAt: time.Now().Add(-1 * time.Hour).UTC(),
	}
	if err := store.Create(info); err != nil {
		t.Fatalf("Create: %v", err)
	}

	gc.SweepOnce(context.Background())

	if store.HasPartial("ns", "stale-incomplete") {
		t.Fatalf("expected stale partial to be gone")
	}
}

func TestGC_KeepsInProgressUpload(t *testing.T) {
	store, gc := gcFixture(t)

	info := Info{
		ID:        "fresh-incomplete",
		Namespace: "ns",
		Size:      100,
		CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().Add(7 * 24 * time.Hour).UTC(),
	}
	if err := store.Create(info); err != nil {
		t.Fatalf("Create: %v", err)
	}

	gc.SweepOnce(context.Background())

	if !store.HasPartial("ns", "fresh-incomplete") {
		t.Fatalf("expected fresh partial to remain")
	}
}

func TestGC_SkipsLockedUpload(t *testing.T) {
	store, gc := gcFixture(t)

	// Stale incomplete that would normally be reaped.
	info := Info{
		ID:        "locked-incomplete",
		Namespace: "ns",
		Size:      100,
		CreatedAt: time.Now().Add(-48 * time.Hour).UTC(),
		ExpiresAt: time.Now().Add(-1 * time.Hour).UTC(),
	}
	if err := store.Create(info); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Hold the lock externally to simulate an in-flight PATCH.
	releaseHandler, err := gc.cfg.Locker.Acquire(context.Background(), "ns/locked-incomplete", func() {})
	if err != nil {
		t.Fatalf("Acquire (handler): %v", err)
	}
	// Run sweep with the lock held: should skip.
	done := make(chan struct{})
	go func() {
		gc.SweepOnce(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("SweepOnce should have returned promptly when lock is held")
	}
	if !store.HasPartial("ns", "locked-incomplete") {
		t.Fatalf("expected locked partial to remain")
	}
	releaseHandler()

	// After releasing, a fresh sweep should now reap it.
	gc.SweepOnce(context.Background())
	if store.HasPartial("ns", "locked-incomplete") {
		t.Fatalf("expected locked-incomplete to be reaped after lock release")
	}
}

func TestGC_RemovesOrphanInfo(t *testing.T) {
	store, gc := gcFixture(t)

	// Write an info file directly without a corresponding .partial or
	// completed file - simulating a crash between mkdir and create-partial.
	info := Info{
		ID:        "orphan",
		Namespace: "ns",
		Size:      100,
		CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().Add(7 * 24 * time.Hour).UTC(),
	}
	if err := os.MkdirAll(store.nsDir("ns"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := store.writeInfo(info); err != nil {
		t.Fatalf("writeInfo: %v", err)
	}

	gc.SweepOnce(context.Background())

	if _, err := store.LoadInfo("ns", "orphan"); err == nil {
		t.Fatalf("expected orphan .info to be removed")
	}
}

func TestGC_RemovesOrphanIdemMapping(t *testing.T) {
	store, gc := gcFixture(t)

	// Write an idem mapping pointing at an upload that doesn't exist.
	if err := os.MkdirAll(filepath.Join(store.nsDir("ns"), ".idem"), 0o755); err != nil {
		t.Fatalf("mkdir idem: %v", err)
	}
	if err := store.writeIdem("ns", "orphan-key", "ghost-id"); err != nil {
		t.Fatalf("writeIdem: %v", err)
	}

	gc.SweepOnce(context.Background())

	got, err := store.LookupIdem("ns", "orphan-key")
	if err != nil {
		t.Fatalf("LookupIdem: %v", err)
	}
	if got != "" {
		t.Fatalf("expected orphan idem mapping to be removed, got %q", got)
	}
}

func TestGC_KeepsLiveIdemMapping(t *testing.T) {
	store, gc := gcFixture(t)

	info := Info{
		ID:             "live-id",
		Namespace:      "ns",
		Size:           100,
		CreatedAt:      time.Now().UTC(),
		ExpiresAt:      time.Now().Add(7 * 24 * time.Hour).UTC(),
		IdempotencyKey: "live-key",
	}
	if err := store.Create(info); err != nil {
		t.Fatalf("Create: %v", err)
	}

	gc.SweepOnce(context.Background())

	got, err := store.LookupIdem("ns", "live-key")
	if err != nil {
		t.Fatalf("LookupIdem: %v", err)
	}
	if got != "live-id" {
		t.Fatalf("expected live idem mapping kept, got %q", got)
	}
}
