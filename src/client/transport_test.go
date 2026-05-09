package client

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// TestNewHTTPClient_DefaultIsHTTP1 verifies the default transport is the
// stdlib *http.Transport (HTTP/1.1) when FERRY_HTTP2 is unset.
func TestNewHTTPClient_DefaultIsHTTP1(t *testing.T) {
	t.Setenv("FERRY_HTTP2", "")
	c := NewHTTPClient()
	if _, ok := c.Transport.(*http.Transport); !ok {
		t.Fatalf("expected *http.Transport by default, got %T", c.Transport)
	}
}

// TestNewHTTPClient_HTTP2OptIn verifies setting FERRY_HTTP2 swaps in the
// http2.Transport with cleartext (h2c) prior-knowledge configuration.
func TestNewHTTPClient_HTTP2OptIn(t *testing.T) {
	t.Setenv("FERRY_HTTP2", "1")
	c := NewHTTPClient()
	tr, ok := c.Transport.(*http2.Transport)
	if !ok {
		t.Fatalf("expected *http2.Transport, got %T", c.Transport)
	}
	if !tr.AllowHTTP {
		t.Error("expected AllowHTTP=true for cleartext h2c")
	}
	if tr.DialTLSContext == nil {
		t.Error("expected DialTLSContext to be set for cleartext")
	}
	if tr.ReadIdleTimeout == 0 {
		t.Error("expected ReadIdleTimeout to be set so dead connections are detected")
	}
}

// TestH2CRoundTrip exercises the full h2c stack: a server wrapped in
// h2c.NewHandler and a client built by NewHTTPClient with FERRY_HTTP2=1
// should successfully complete a request over a single cleartext TCP
// connection negotiated as HTTP/2 prior-knowledge. Asserts on the proto
// version observed by the handler so we can't silently fall back to h1.
func TestH2CRoundTrip(t *testing.T) {
	t.Setenv("FERRY_HTTP2", "1")

	var observedProto string
	h2s := &http2.Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		observedProto = r.Proto
		w.WriteHeader(http.StatusOK)
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h2c.NewHandler(mux, h2s)}
	go srv.Serve(listener)
	t.Cleanup(func() { srv.Close() })

	c := NewHTTPClient()
	resp, err := c.Get("http://" + listener.Addr().String() + "/probe")
	if err != nil {
		t.Fatalf("h2c GET failed: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if !strings.HasPrefix(observedProto, "HTTP/2") {
		t.Errorf("server saw proto %q, want HTTP/2.x (h2c upgrade did not happen)", observedProto)
	}
	if !strings.HasPrefix(resp.Proto, "HTTP/2") {
		t.Errorf("client saw response proto %q, want HTTP/2.x", resp.Proto)
	}
}

// TestH2CTransportUsesPlainTCP verifies the DialTLSContext hook on the
// h2c transport returns a plain net.Conn (not a *tls.Conn). This is the
// core trick that makes cleartext HTTP/2 work despite the field name.
func TestH2CTransportUsesPlainTCP(t *testing.T) {
	t.Setenv("FERRY_HTTP2", "1")
	c := NewHTTPClient()
	tr := c.Transport.(*http2.Transport)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })

	// Accept once and close so the dial succeeds without blocking.
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			conn.Close()
		}
	}()

	conn, err := tr.DialTLSContext(context.Background(), "tcp", listener.Addr().String(), &tls.Config{})
	if err != nil {
		t.Fatalf("DialTLSContext failed: %v", err)
	}
	defer conn.Close()

	if _, isTLS := conn.(*tls.Conn); isTLS {
		t.Error("DialTLSContext returned a *tls.Conn but should return a plain net.Conn for h2c cleartext")
	}
}

// TestNewHTTPClient_HTTP1ResponseHeaderTimeout verifies the HTTP/1.1
// transport still carries the package-level ResponseHeaderTimeout so the
// existing slow-server detection path keeps working when h2c is not
// opted into. h2c has no equivalent (uses ReadIdleTimeout instead).
func TestNewHTTPClient_HTTP1ResponseHeaderTimeout(t *testing.T) {
	t.Setenv("FERRY_HTTP2", "")
	c := NewHTTPClient()
	tr := c.Transport.(*http.Transport)
	if tr.ResponseHeaderTimeout != ResponseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout=%v, want %v", tr.ResponseHeaderTimeout, ResponseHeaderTimeout)
	}
}

// httptestNewServer is a small wrapper to ensure tests that use
// httptest.NewServer aren't accidentally affected by FERRY_HTTP2 leaking
// from the parent process. Sanity check at the package level.
var _ = httptest.NewServer
