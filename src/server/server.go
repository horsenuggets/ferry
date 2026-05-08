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
	store, err := NewStore(cfg.DataDir)
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
// in-flight requests up to 10s before returning.
//
// TODO(phase 4): GC sweeper for completed/incomplete uploads beyond their
// retention windows. Stubbed here so the lifecycle is in place.
func (s *Server) Run(ctx context.Context, version string) error {
	h := NewHandler(HandlerConfig{
		Store:         s.store,
		Auth:          s.auth,
		Locker:        NewLocker(),
		MaxPatchBytes: s.cfg.MaxPatchBytes,
		SafetyMargin:  s.cfg.DiskSafetyMarginBytes,
		CompletedTTL:  time.Duration(s.cfg.CompletedRetentionSeconds) * time.Second,
		IncompleteTTL: time.Duration(s.cfg.IncompleteRetentionSeconds) * time.Second,
		Version:       version,
		Logger:        s.logger,
	})
	listener, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.ListenAddr, err)
	}
	s.mu.Lock()
	s.addr = listener.Addr().String()
	s.mu.Unlock()

	srv := &http.Server{
		Handler:           h.Routes(),
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
