package server

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// GCConfig captures the inputs the GC sweeper needs.
type GCConfig struct {
	Store         *Store
	Locker        *Locker
	Interval      time.Duration
	CompletedTTL  time.Duration // retain completed uploads this long after completed_at
	IncompleteTTL time.Duration // retain incomplete uploads this long after created_at (mirrored as expires_at)
	Logger        *slog.Logger
}

// GC is a periodic sweeper that removes:
//
//   - Completed uploads whose completed_at + CompletedTTL is in the past:
//     deletes the binary, .info, and any idem mapping that pointed at it.
//   - Incomplete uploads whose expires_at is in the past: deletes the
//     .partial, .info, and any idem mapping.
//   - Orphans: .info files whose binary doesn't exist (no .partial AND no
//     completed file) - the partial was likely interrupted between the
//     create-partial and create-info steps. Logged as a warning.
//   - Idem keys whose target upload no longer exists.
//
// Skips uploads currently locked by an in-flight PATCH (the lock is held
// by the request handler; we do a TryAcquire-equivalent by attempting a
// short-deadline acquire and bailing if we don't get it).
type GC struct {
	cfg GCConfig
}

// minGCInterval is the smallest tick we accept; anything below produces
// CPU thrash with no useful churn-rate. Misconfigurations get clamped here
// instead of panicking time.NewTicker.
const minGCInterval = time.Second

// NewGC constructs a GC sweeper. Interval is clamped to a minimum so a
// misconfigured (or zero/negative) gc_interval_seconds doesn't panic the
// server at runtime.
func NewGC(cfg GCConfig) *GC {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Interval < minGCInterval {
		cfg.Logger.Warn("gc: interval too small, clamping",
			"requested", cfg.Interval, "clamped_to", minGCInterval)
		cfg.Interval = minGCInterval
	}
	return &GC{cfg: cfg}
}

// Run blocks until ctx is canceled, sweeping every Interval. The first
// sweep happens at Interval (not immediately) so a flapping server doesn't
// thrash the disk on every restart.
func (g *GC) Run(ctx context.Context) {
	ticker := time.NewTicker(g.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.SweepOnce(ctx)
		}
	}
}

// SweepOnce performs a single pass over all namespaces. Exposed for tests.
func (g *GC) SweepOnce(ctx context.Context) {
	now := time.Now().UTC()
	namespaces, err := g.cfg.Store.ListNamespaces()
	if err != nil {
		g.cfg.Logger.Error("gc: list namespaces", "err", err)
		return
	}
	for _, ns := range namespaces {
		if err := ctx.Err(); err != nil {
			return
		}
		g.sweepNamespace(ctx, ns, now)
	}
}

func (g *GC) sweepNamespace(ctx context.Context, ns string, now time.Time) {
	ids, err := g.cfg.Store.ListUploads(ns)
	if err != nil {
		g.cfg.Logger.Error("gc: list uploads", "namespace", ns, "err", err)
		return
	}
	// First pass: remove uploads that have aged out or are orphans.
	keptIDs := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return
		}
		removed := g.maybeSweepUpload(ctx, ns, id, now)
		if !removed {
			keptIDs[id] = struct{}{}
		}
	}
	// Second pass: remove idem keys that point at uploads no longer present.
	keys, err := g.cfg.Store.ListIdemKeys(ns)
	if err != nil {
		g.cfg.Logger.Error("gc: list idem keys", "namespace", ns, "err", err)
		return
	}
	for _, key := range keys {
		target, err := g.cfg.Store.LookupIdem(ns, key)
		if err != nil {
			g.cfg.Logger.Warn("gc: read idem mapping", "namespace", ns, "key", key, "err", err)
			continue
		}
		if target == "" {
			continue
		}
		if _, alive := keptIDs[target]; alive {
			continue
		}
		if err := g.cfg.Store.RemoveIdem(ns, key); err != nil {
			g.cfg.Logger.Warn("gc: remove orphan idem", "namespace", ns, "key", key, "err", err)
		} else {
			g.cfg.Logger.Info("gc: removed orphan idem", "namespace", ns, "key", key, "target", target)
		}
	}
}

// maybeSweepUpload returns true when the upload was removed and false when
// it was kept (still in retention window or currently locked).
func (g *GC) maybeSweepUpload(ctx context.Context, ns, id string, now time.Time) bool {
	info, err := g.cfg.Store.LoadInfo(ns, id)
	if err != nil {
		// .info was racing-removed by Delete; nothing to do.
		if errors.Is(err, ErrNotFound) {
			return true
		}
		g.cfg.Logger.Warn("gc: load info", "namespace", ns, "id", id, "err", err)
		return false
	}

	hasPartial, errP := g.cfg.Store.HasPartial(ns, id)
	if errP != nil {
		g.cfg.Logger.Warn("gc: stat partial; keeping upload",
			"namespace", ns, "id", id, "err", errP)
		return false
	}
	hasCompleted, errC := g.cfg.Store.HasCompleted(ns, id)
	if errC != nil {
		g.cfg.Logger.Warn("gc: stat completed; keeping upload",
			"namespace", ns, "id", id, "err", errC)
		return false
	}

	// Decide whether this upload should be reaped.
	var reason string
	switch {
	case info.CompletedAt != nil:
		if now.Sub(*info.CompletedAt) > g.cfg.CompletedTTL {
			reason = "completed retention exceeded"
		}
	case !hasPartial && !hasCompleted:
		// Sidecar but no data file at all: orphan.
		reason = "orphan: .info without binary"
	case now.After(info.ExpiresAt):
		reason = "incomplete expired"
	}
	if reason == "" {
		return false
	}

	// Take the upload's lock without disturbing any current holder. If a
	// PATCH (or another GC pass) is holding it, skip this round and try
	// again next sweep. Using TryAcquire instead of Acquire is important:
	// Acquire would call the holder's requestRelease, which in production
	// cancels the in-flight PATCH. We never want background reaping to
	// abort active uploads.
	release, ok := g.cfg.Locker.TryAcquire(ns + "/" + id)
	if !ok {
		g.cfg.Logger.Info("gc: skipped (locked)", "namespace", ns, "id", id)
		return false
	}
	defer release()

	// Re-stat under the lock: caller may have just completed/deleted.
	freshInfo, err := g.cfg.Store.LoadInfo(ns, id)
	if err != nil {
		// Already gone, treat as removed.
		return true
	}
	freshCompleted := freshInfo.CompletedAt
	if freshCompleted != nil && now.Sub(*freshCompleted) <= g.cfg.CompletedTTL {
		// Completion happened during our sweep; not stale yet.
		return false
	}

	if err := g.cfg.Store.Delete(ns, id); err != nil {
		g.cfg.Logger.Error("gc: delete failed",
			"namespace", ns, "id", id, "reason", reason, "err", err)
		return false
	}
	g.cfg.Logger.Info("gc: removed", "namespace", ns, "id", id, "reason", reason)
	return true
}
