package server

import (
	"crypto/rand"
	"time"

	"github.com/oklog/ulid/v2"
)

// newID returns a fresh ULID string. ULIDs are time-prefixed and lexically
// sortable, which is convenient for `ls` and log scans.
func newID() string {
	return ulid.MustNew(ulid.Timestamp(time.Now().UTC()), rand.Reader).String()
}
