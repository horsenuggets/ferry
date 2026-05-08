package server

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
)

// parseMetadata parses a tus Upload-Metadata header value:
//
//	"key1 base64value1, key2 base64value2"
//
// Bad pairs are silently skipped; metadata is opaque to ferry.
func parseMetadata(header string) map[string]string {
	if header == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(header, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, " ", 2)
		key := strings.TrimSpace(parts[0])
		if key == "" {
			continue
		}
		if len(parts) == 1 {
			out[key] = ""
			continue
		}
		val, err := base64.StdEncoding.DecodeString(strings.TrimSpace(parts[1]))
		if err != nil {
			continue
		}
		out[key] = string(val)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// contextWithCause wraps context.WithCancelCause so callers don't have to
// import context directly. The returned cancel takes a cause; pass nil for
// "no cause" semantics.
func contextWithCause(parent context.Context) (context.Context, context.CancelCauseFunc) {
	return context.WithCancelCause(parent)
}

// ctxReader wraps an io.Reader with a context check before each Read call.
// When the context is cancelled (e.g. cooperative-cancel from the locker),
// Read returns ctx.Err() so the in-flight io.Copy unblocks promptly.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// isMaxBytesError reports whether err comes from http.MaxBytesReader's
// overrun path. The stdlib uses *http.MaxBytesError since Go 1.19.
func isMaxBytesError(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}
