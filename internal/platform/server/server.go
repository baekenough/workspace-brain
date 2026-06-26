// Package server provides hardened HTTP server helpers.
package server

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Config contains HTTP server runtime settings.
type Config struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

// New creates an http.Server with production-safe timeouts.
func New(cfg Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}
}

// Serve runs srv and gracefully shuts it down when ctx is canceled.
func Serve(ctx context.Context, srv *http.Server, shutdownTimeout time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	errCh := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return <-errCh
}
