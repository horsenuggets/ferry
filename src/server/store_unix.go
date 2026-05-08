//go:build !windows

package server

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// availableBytes reports free disk bytes at root using statfs(2).
func availableBytes(root string) (int64, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil {
		return 0, fmt.Errorf("statfs: %w", err)
	}
	// Bavail/Bsize are unsigned. Cap the product at maxInt64 to avoid an
	// uint64 -> int64 overflow on absurdly-large filesystems (gosec G115).
	const maxInt64 = int64(^uint64(0) >> 1)
	avail := uint64(fs.Bavail) * uint64(fs.Bsize)
	if avail > uint64(maxInt64) {
		return maxInt64, nil
	}
	return int64(avail), nil
}
