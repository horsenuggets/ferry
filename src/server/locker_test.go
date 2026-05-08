package server

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestLockerBasic(t *testing.T) {
	l := NewLocker()
	rel, err := l.Acquire(context.Background(), "k", func() {})
	if err != nil {
		t.Fatal(err)
	}
	rel()
	// Should be re-acquirable.
	rel2, err := l.Acquire(context.Background(), "k", func() {})
	if err != nil {
		t.Fatal(err)
	}
	rel2()
}

func TestLockerCooperativeCancel(t *testing.T) {
	l := NewLocker()

	var firstReleaseRequested atomic.Bool
	holderDone := make(chan struct{})
	holderRel, err := l.Acquire(context.Background(), "k", func() {
		firstReleaseRequested.Store(true)
	})
	if err != nil {
		t.Fatal(err)
	}

	// Second acquirer should fire holder's requestRelease and then wait
	// until the holder releases.
	secondAcquired := make(chan struct{})
	go func() {
		rel, err := l.Acquire(context.Background(), "k", func() {})
		if err != nil {
			t.Errorf("second acquire: %v", err)
			return
		}
		close(secondAcquired)
		rel()
	}()

	// Wait until the holder is asked to release.
	deadline := time.After(2 * time.Second)
	for !firstReleaseRequested.Load() {
		select {
		case <-deadline:
			t.Fatal("requestRelease never invoked on holder")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Holder hasn't released yet; second must still be blocked.
	select {
	case <-secondAcquired:
		t.Fatal("second acquired before holder released")
	default:
	}

	// Now release.
	holderRel()
	close(holderDone)

	select {
	case <-secondAcquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second never acquired after holder released")
	}
}

func TestLockerAcquireTimeout(t *testing.T) {
	l := NewLocker()
	// Hold the lock and never release.
	_, err := l.Acquire(context.Background(), "k", func() {})
	if err != nil {
		t.Fatal(err)
	}
	// Use a context that times out quickly so we don't wait the full 20s.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := l.Acquire(ctx, "k", func() {}); !errors.Is(err, ErrFileLocked) && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Acquire on held lock = %v, want ErrFileLocked or DeadlineExceeded", err)
	}
}

func TestLockerTryAcquire(t *testing.T) {
	l := NewLocker()
	rel, ok := l.TryAcquire("ns/upload-1")
	if !ok {
		t.Fatalf("TryAcquire on empty should succeed")
	}
	// Second TryAcquire while held: must fail without disturbing the holder.
	if _, ok := l.TryAcquire("ns/upload-1"); ok {
		t.Fatalf("TryAcquire on held lock should return false")
	}
	rel()
	// After release, TryAcquire works again.
	rel2, ok := l.TryAcquire("ns/upload-1")
	if !ok {
		t.Fatalf("TryAcquire after release should succeed")
	}
	rel2()
}
