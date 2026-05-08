//go:build !windows

package server

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

// availableBytes reports free disk bytes at root using statfs(2).
func availableBytes(root string) (int64, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil {
		return 0, fmt.Errorf("statfs: %w", err)
	}
	// Bavail/Bsize are unsigned on Linux and signed on Darwin/BSD. Coerce
	// each to uint64 first, then cap the product at math.MaxInt64 before
	// the final int64 conversion to dodge overflow.
	avail := uint64(fs.Bavail) * uint64(fs.Bsize) //nolint:gosec // bounded below
	if avail > math.MaxInt64 {
		return math.MaxInt64, nil
	}
	return int64(avail), nil //nolint:gosec // bounded above by MaxInt64 check
}
