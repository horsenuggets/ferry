package client

import (
	"strings"
	"time"
)

// Adaptive chunk sizing
//
// Ferry uploads a file in fixed-size PATCH chunks. On a fast link a 4 MiB
// chunk is a good compromise: low protocol overhead, manageable retry cost.
// On a slow / lossy / high-RTT residential uplink, a 4 MiB chunk can spend
// 30+ seconds on the wire and any transient interruption forces the client
// to re-send the whole thing.
//
// We adapt the chunk size in response to observed throughput:
//
//   - Each successful PATCH records its observed throughput (bytes/elapsed).
//   - If observed throughput is below SlowThroughputBytesPerSec (1 MiB/s),
//     the next chunk size is halved (down to MinAdaptiveChunkBytes, 256 KiB).
//   - If observed throughput is above FastThroughputBytesPerSec (4 MiB/s),
//     the next chunk size is doubled (up to the configured ceiling).
//   - Otherwise the chunk size stays put.
//
// The ceiling is whatever the caller configured (CLI --chunk-size or default
// 4 MiB), capped by the server's max_patch_bytes. The floor is 256 KiB.
//
// Adaptation is enabled by default. The CLI flag --no-adaptive-chunks
// disables it (leaving the size pinned to the configured value).
const (
	// SlowThroughputBytesPerSec is the threshold below which we shrink the
	// next chunk. 1 MiB/s.
	SlowThroughputBytesPerSec = 1 * 1024 * 1024

	// FastThroughputBytesPerSec is the threshold above which we grow the
	// next chunk. 4 MiB/s.
	FastThroughputBytesPerSec = 4 * 1024 * 1024

	// MinAdaptiveChunkBytes is the floor on the adaptive chunk size.
	// 256 KiB keeps per-PATCH protocol overhead acceptable while still
	// limiting the cost of a retried chunk on a slow link.
	MinAdaptiveChunkBytes = 256 * 1024
)

// adaptiveSizer tracks the current chunk size and adjusts it after each
// successful PATCH based on observed throughput.
//
// Not safe for concurrent use; the sequential upload loop owns one of these.
type adaptiveSizer struct {
	enabled bool
	current int64
	ceiling int64 // configured / caller-supplied max
	floor   int64
}

// newAdaptiveSizer returns a sizer initialized to ceiling (the configured
// chunk size). When enabled is false, observe is a no-op and size always
// returns the ceiling.
func newAdaptiveSizer(ceiling int64, enabled bool) *adaptiveSizer {
	floor := int64(MinAdaptiveChunkBytes)
	if ceiling < floor {
		floor = ceiling
	}
	return &adaptiveSizer{
		enabled: enabled,
		current: ceiling,
		ceiling: ceiling,
		floor:   floor,
	}
}

// size returns the chunk size to use for the next PATCH.
func (a *adaptiveSizer) size() int64 {
	if a == nil {
		return 0
	}
	return a.current
}

// observe updates the current chunk size based on the throughput of the
// PATCH that just completed. bytes is the chunk length, elapsed is wall time
// from sending the first byte to receiving the 204 status.
func (a *adaptiveSizer) observe(bytes int64, elapsed time.Duration) {
	if a == nil || !a.enabled {
		return
	}
	if bytes <= 0 || elapsed <= 0 {
		return
	}
	bps := float64(bytes) / elapsed.Seconds()
	switch {
	case bps < SlowThroughputBytesPerSec:
		next := a.current / 2
		if next < a.floor {
			next = a.floor
		}
		a.current = next
	case bps > FastThroughputBytesPerSec:
		next := a.current * 2
		if next > a.ceiling {
			next = a.ceiling
		}
		a.current = next
	}
}

// isResponseHeaderTimeout reports whether err looks like the
// http.Transport.ResponseHeaderTimeout firing. Go's net/http surfaces this
// without a sentinel error; we string-match. Returning a false positive is
// harmless: it just produces a slightly more verbose error.
func isResponseHeaderTimeout(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "timeout awaiting response headers")
}

