package client

import (
	"net"
	"net/http"
	"time"
)

// responseHeaderTimeoutOf returns the live ResponseHeaderTimeout setting on
// the client's transport, or ResponseHeaderTimeout (the package default) if
// the transport isn't an *http.Transport or has no timeout set. Used by
// error messages so they reflect what was actually configured rather than
// the package default — tests and callers can override the timeout.
func responseHeaderTimeoutOf(c *http.Client) time.Duration {
	if c == nil {
		return ResponseHeaderTimeout
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr == nil {
		return ResponseHeaderTimeout
	}
	if tr.ResponseHeaderTimeout > 0 {
		return tr.ResponseHeaderTimeout
	}
	return ResponseHeaderTimeout
}

// ResponseHeaderTimeout is the maximum time the client waits for the server
// to start sending response headers after the PATCH body has been fully
// written. Exposed as a package-level constant so error messages and tests
// can reference the same value.
//
// 5 minutes gives generous slack on slow residential uplinks: a 4 MiB chunk
// at 300 KiB/s spends ~13s on the wire, plus TCP slow-start, plus the
// server's per-PATCH disk + checksum work. The previous 60s value was
// calibrated for a fast LAN and tripped under real WAN conditions.
const ResponseHeaderTimeout = 5 * time.Minute

// NewHTTPClient returns an http.Client tuned for resumable uploads:
//
//   - No overall request timeout: a single PATCH may take an arbitrary amount
//     of time on a slow link or while the server is fsyncing.
//   - Bounded dial / TLS / response-header timeouts so a network black hole
//     doesn't pin a goroutine indefinitely.
//   - Compression off: PATCH bodies are opaque encrypted/random bytes for our
//     purposes; gzipping is wasted CPU.
//   - Modest connection pool sized for one peer per ferry CLI invocation.
func NewHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: ResponseHeaderTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}
	return &http.Client{
		Transport: tr,
		// Timeout=0: the upper bound is the per-chunk retry budget.
		Timeout: 0,
	}
}
