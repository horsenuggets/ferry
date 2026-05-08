package client

import (
	"net"
	"net/http"
	"time"
)

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
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}
	return &http.Client{
		Transport: tr,
		// Timeout=0: the upper bound is the per-chunk retry budget.
		Timeout: 0,
	}
}
