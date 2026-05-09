package client

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"time"

	"golang.org/x/net/http2"
)

// responseHeaderTimeoutOf returns the live ResponseHeaderTimeout setting on
// the client's transport, or ResponseHeaderTimeout (the package default) if
// the transport doesn't expose one. Used by error messages so they reflect
// what was actually configured rather than the package default - tests and
// callers can override the timeout.
//
// HTTP/2 (http2.Transport) does not have a per-request response-header
// timeout: it relies on PING frames + ReadIdleTimeout to detect dead
// connections. For h2c clients we therefore always return the package
// default for error-message purposes.
func responseHeaderTimeoutOf(c *http.Client) time.Duration {
	if c == nil {
		return ResponseHeaderTimeout
	}
	if tr, ok := c.Transport.(*http.Transport); ok && tr != nil {
		if tr.ResponseHeaderTimeout > 0 {
			return tr.ResponseHeaderTimeout
		}
	}
	return ResponseHeaderTimeout
}

// ResponseHeaderTimeout is the maximum time the client waits for the server
// to start sending response headers after the PATCH body has been fully
// written under the HTTP/1.1 transport. Exposed as a package-level constant
// so error messages and tests can reference the same value.
//
// HTTP/2 (h2c) has no equivalent knob; ReadIdleTimeout + PingTimeout below
// fill the same role at the connection level.
const ResponseHeaderTimeout = 5 * time.Minute

// h2cReadIdleTimeout drives HTTP/2 keepalive PINGs. If no frame is read for
// this long, the transport sends a PING and waits h2cPingTimeout for a PONG;
// failure to receive one tears the connection down so the next request
// re-dials instead of hanging on a black hole.
const h2cReadIdleTimeout = 30 * time.Second

// h2cPingTimeout bounds how long we wait for the server's PING response
// before declaring the connection dead.
const h2cPingTimeout = 15 * time.Second

// NewHTTPClient returns an http.Client tuned for resumable uploads.
//
// Default transport is HTTP/1.1. Set FERRY_HTTP2=1 in the environment to
// switch to HTTP/2 cleartext (h2c) prior-knowledge: a single TCP connection
// per peer, slow-start runs once for the whole upload instead of once per
// PATCH, and per-stream flow control replaces the per-PATCH RTT-stall
// pattern that crippled HTTP/1.1 ferry on high-RTT links. h2c requires the
// server to be wrapped in h2c.NewHandler (ferry server does this since
// PR #12).
//
//   - No overall request timeout: a single PATCH may take an arbitrary amount
//     of time on a slow link or while the server is fsyncing.
//   - Bounded dial / TLS / idle-PING timeouts so a network black hole doesn't
//     pin a goroutine indefinitely.
//   - Compression off: PATCH bodies are opaque encrypted/random bytes for our
//     purposes; gzipping is wasted CPU.
func NewHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	if os.Getenv("FERRY_HTTP2") != "" {
		// h2c prior-knowledge. AllowHTTP lets the transport accept
		// "http://" URLs; DialTLSContext returns a plain TCP connection
		// (the field is named for the typical TLS path but
		// http2.Transport calls it for cleartext too when AllowHTTP is
		// set).
		tr := &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return dialer.DialContext(ctx, network, addr)
			},
			ReadIdleTimeout:    h2cReadIdleTimeout,
			PingTimeout:        h2cPingTimeout,
			DisableCompression: true,
		}
		return &http.Client{Transport: tr, Timeout: 0}
	}

	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		MaxIdleConns:          10,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: ResponseHeaderTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}
	return &http.Client{Transport: tr, Timeout: 0}
}
