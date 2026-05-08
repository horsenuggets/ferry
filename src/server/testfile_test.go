package server

import (
	"io"
	"os"
)

// openOSFile is a tiny indirection to avoid importing os in handler_test.go
// which already pulls in many test helpers; keeps imports tidy.
func openOSFile(path string) (io.ReadCloser, error) {
	return os.Open(path)
}
