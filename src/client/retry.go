package client

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/cenkalti/backoff/v4"
)

// MaxChunkRetries is the per-chunk retry budget. After this many failed
// attempts (counting both transport errors and 5xx responses), the upload
// gives up and surfaces the last error.
const MaxChunkRetries uint64 = 5

// NewChunkBackoff builds the exponential-backoff schedule used for a single
// chunk. The schedule is per-chunk: a fresh one is created at the start of
// every PATCH so that a slow chunk doesn't poison later chunks' budgets.
//
// Settings mirror the spec in the task brief:
//   - InitialInterval: 500ms
//   - MaxInterval:     30s
//   - RandomizationFactor: 0.3 (jitter)
//   - MaxElapsedTime: 0 (unbounded; we cap by attempt count)
func NewChunkBackoff() backoff.BackOff {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = 500 * time.Millisecond
	b.MaxInterval = 30 * time.Second
	b.RandomizationFactor = 0.3
	b.Multiplier = 2.0
	b.MaxElapsedTime = 0
	b.Reset()
	return backoff.WithMaxRetries(b, MaxChunkRetries)
}

// IsRetryable classifies an error as worth retrying. The rules:
//
//   - context cancellation/deadline: NEVER retry. The user (or a higher-level
//     timeout) explicitly asked us to stop.
//   - net errors that are timeouts or temporary: retry.
//   - syscall errors that suggest transport flakiness (ECONNREFUSED,
//     ECONNRESET, EPIPE, ETIMEDOUT, ENETUNREACH, EHOSTUNREACH): retry.
//   - io.EOF / io.ErrUnexpectedEOF mid-request: retry (server hung up).
//   - DNS errors: retry.
//   - everything else: do not retry.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		// Even "no such host" is worth at least one retry: stub DNS or a
		// resolver hiccup can produce it. The retry budget caps the cost.
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return true
		}
	}

	var sysErr syscall.Errno
	if errors.As(err, &sysErr) {
		switch sysErr {
		case syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EPIPE,
			syscall.ETIMEDOUT, syscall.ENETUNREACH, syscall.EHOSTUNREACH:
			return true
		}
	}

	// Last resort: substring matching for errors that don't unwrap cleanly.
	// HTTP/1.1 connection-reset surfaces as "connection reset by peer" via
	// the transport in some Go versions without a wrapped syscall.Errno.
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "unexpected EOF"),
		strings.Contains(msg, "no such host"):
		return true
	}
	return false
}

// ClassifyStatus returns whether an HTTP status code triggers (a) a retry
// (true, false) or (b) a HEAD-then-resume (false, true). 4xx statuses other
// than 409 short-circuit upload with a permanent error (false, false).
//
// 2xx is the success path and never reaches this function in callers.
func ClassifyStatus(status int) (retry, headThenResume bool) {
	switch {
	case status == 409:
		return false, true
	case status >= 500 && status <= 599:
		return true, false
	case status == 408 || status == 425 || status == 429:
		// 408 Request Timeout, 425 Too Early, 429 Too Many Requests are
		// transient by design; safe to retry with backoff.
		return true, false
	default:
		return false, false
	}
}
