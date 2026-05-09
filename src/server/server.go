package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// Server is a thin lifecycle wrapper around an http.Server with a Handler.
type Server struct {
	cfg    *Config
	auth   *Authenticator
	store  *Store
	logger *slog.Logger

	mu   sync.Mutex
	addr string // resolved listen addr (set on first Run)
}

// New constructs a Server from cfg + tokens path. Logger defaults to a JSON
// slog handler on stderr if nil.
func New(cfg *Config, version string, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	auth, err := LoadAuthenticator(cfg.TokensPath)
	if err != nil {
		return nil, fmt.Errorf("load tokens: %w", err)
	}
	// Route the store's structured timing logs through the same logger
	// the rest of the server uses (JSON to stderr in production).
	store, err := NewStoreWithLogger(cfg.DataDir, logger)
	if err != nil {
		return nil, fmt.Errorf("init store: %w", err)
	}
	_ = version // currently surfaced via NewHandler below
	return &Server{
		cfg:    cfg,
		auth:   auth,
		store:  store,
		logger: logger,
	}, nil
}

// Run starts the HTTP server and blocks until ctx is canceled, then drains
// in-flight requests up to 10s before returning. A GC sweeper runs in a
// background goroutine, sharing the locker with the request handler so
// uploads can never be reaped mid-PATCH.
func (s *Server) Run(ctx context.Context, version string) error {
	locker := NewLocker()
	completedTTL := time.Duration(s.cfg.CompletedRetentionSeconds) * time.Second
	incompleteTTL := time.Duration(s.cfg.IncompleteRetentionSeconds) * time.Second

	h := NewHandler(HandlerConfig{
		Store:         s.store,
		Auth:          s.auth,
		Locker:        locker,
		MaxPatchBytes: s.cfg.MaxPatchBytes,
		SafetyMargin:  s.cfg.DiskSafetyMarginBytes,
		CompletedTTL:  completedTTL,
		IncompleteTTL: incompleteTTL,
		Version:       version,
		Logger:        s.logger,
	})

	gc := NewGC(GCConfig{
		Store:         s.store,
		Locker:        locker,
		Interval:      time.Duration(s.cfg.GCIntervalSeconds) * time.Second,
		CompletedTTL:  completedTTL,
		IncompleteTTL: incompleteTTL,
		Logger:        s.logger,
	})

	listener, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.ListenAddr, err)
	}
	s.mu.Lock()
	s.addr = listener.Addr().String()
	s.mu.Unlock()

	// Only start the GC goroutine once Listen has succeeded - otherwise
	// an early-return error path here leaks the goroutine until the
	// caller's ctx is canceled (which in tests may never happen).
	gcCtx, cancelGC := context.WithCancel(ctx)
	defer cancelGC()
	go gc.Run(gcCtx)

	// Wrap the routes in h2c so plain-HTTP clients can speak HTTP/2 over
	// cleartext. ferry runs over private links (WireGuard / loopback /
	// LAN) so TLS would be wasted overhead; HTTP/2's connection-level
	// flow control + single-slow-start dwarf any framing overhead and
	// matter most on high-RTT paths. HTTP/1.1 clients still work because
	// h2c.NewHandler intercepts only h2c-prefixed traffic and forwards
	// everything else to the inner handler unchanged.
	//
	// MaxUploadBuffer{PerStream,PerConnection} advertise the initial
	// flow-control window to the client. Default is 1 MiB / 1 MiB which
	// throttles uploads on high-BDP links: e.g. on a 70ms RTT path a
	// stream window of 1 MiB caps single-stream throughput at ~14 MiB/s
	// regardless of bandwidth. 16 MiB matches the largest chunk size
	// ferry uses today and is well above realistic single-link BDPs.
	h2s := &http2.Server{
		MaxUploadBufferPerStream:     16 * 1024 * 1024,
		MaxUploadBufferPerConnection: 64 * 1024 * 1024,
	}
	srv := &http.Server{
		Handler:           h2c.NewHandler(h.Routes(), h2s),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("ferry listen", "addr", listener.Addr().String())
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		return err
	}
}

// Addr returns the resolved listening address. Empty until Run starts.
// Useful for tests that bind ":0" to get an ephemeral port.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}
