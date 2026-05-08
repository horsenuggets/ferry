package server

import (
	"context"
	"sync"
	"time"
)

// acquireTimeout matches tusd's default. If we can't grab the lock within
// this window, the request is failed with ErrFileLocked.
const acquireTimeout = 20 * time.Second

// lockEntry tracks a held lock. When a competing request tries to acquire,
// we call requestRelease (typically cancels the holder's context) and wait
// for the holder to actually release by closing released.
type lockEntry struct {
	released       chan struct{}
	requestRelease func()
}

// Locker is an in-memory cooperative-cancel locker keyed by upload id (we
// use namespace/id to avoid collisions across namespaces). Acquiring a held
// lock signals the holder to release; the new holder then waits for the
// release. This is a direct port of tusd's MemoryLocker algorithm.
type Locker struct {
	mu    sync.Mutex
	locks map[string]lockEntry
}

// NewLocker constructs an empty Locker.
func NewLocker() *Locker {
	return &Locker{locks: map[string]lockEntry{}}
}

// Acquire takes the lock for key. requestRelease is invoked if a future
// acquirer asks the current holder to release. The returned release
// function must be called exactly once (typically via defer) when the
// holder is done.
func (l *Locker) Acquire(ctx context.Context, key string, requestRelease func()) (func(), error) {
	// Bound the acquire wait independently of the caller's ctx so a long
	// caller deadline doesn't let lock starvation block forever silently.
	acqCtx, cancel := context.WithTimeout(ctx, acquireTimeout)
	defer cancel()

	for {
		l.mu.Lock()
		entry, held := l.locks[key]
		if !held {
			released := make(chan struct{})
			l.locks[key] = lockEntry{
				released:       released,
				requestRelease: requestRelease,
			}
			l.mu.Unlock()
			return func() { l.release(key, released) }, nil
		}
		l.mu.Unlock()

		// Ask the holder to release, then wait.
		entry.requestRelease()
		select {
		case <-acqCtx.Done():
			if acqCtx.Err() == context.DeadlineExceeded {
				return nil, ErrFileLocked
			}
			return nil, acqCtx.Err()
		case <-entry.released:
			// Holder released; loop and try to acquire.
		}
	}
}

func (l *Locker) release(key string, released chan struct{}) {
	l.mu.Lock()
	delete(l.locks, key)
	l.mu.Unlock()
	close(released)
}
