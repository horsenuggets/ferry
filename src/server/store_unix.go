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
	// Bavail/Bsize are unsigned; the product fits in int64 for any sane
	// filesystem.
	return int64(fs.Bavail) * int64(fs.Bsize), nil
}
