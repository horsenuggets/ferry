//go:build windows

package server

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// availableBytes reports free disk bytes at root using GetDiskFreeSpaceExW.
func availableBytes(root string) (int64, error) {
	pathPtr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return 0, fmt.Errorf("path utf16: %w", err)
	}
	var freeAvail, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(pathPtr, &freeAvail, &totalBytes, &totalFree); err != nil {
		return 0, fmt.Errorf("GetDiskFreeSpaceEx: %w", err)
	}
	if freeAvail > 1<<62 {
		return 1 << 62, nil
	}
	return int64(freeAvail), nil
}
