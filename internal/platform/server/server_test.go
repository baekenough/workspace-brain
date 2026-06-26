package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestNewAppliesTimeouts(t *testing.T) {
	t.Parallel()
	cfg := Config{Addr: ":0", ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: 4 * time.Second}
	srv := New(cfg, http.NotFoundHandler())
	if srv.Addr != cfg.Addr || srv.ReadHeaderTimeout != cfg.ReadHeaderTimeout || srv.ReadTimeout != cfg.ReadTimeout || srv.WriteTimeout != cfg.WriteTimeout || srv.IdleTimeout != cfg.IdleTimeout || srv.Handler == nil {
		t.Fatalf("server = %+v", srv)
	}
}

func TestServeShutsDownGracefullyOnContextCancel(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	srv := New(Config{Addr: ln.Addr().String()}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, srv, time.Second) }()

	deadline := time.After(2 * time.Second)
	for {
		resp, err := http.Get("http://" + ln.Addr().String())
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		select {
		case <-deadline:
			t.Fatalf("server did not start: %v", err)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Serve did not return after cancel")
	}
}

func TestServeReturnsListenError(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	defer ln.Close()
	srv := New(Config{Addr: addr}, http.NotFoundHandler())
	if err := Serve(context.Background(), srv, time.Second); err == nil || errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("err=%v", err)
	}
}

func TestServeUsesBackgroundWhenContextIsNil(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	srv := New(Config{Addr: addr}, http.NotFoundHandler())
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(nil, srv, time.Second) }()
	deadline := time.After(2 * time.Second)
	for {
		resp, err := http.Get("http://" + addr)
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		select {
		case <-deadline:
			_ = srv.Close()
			t.Fatalf("server did not start: %v", err)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Serve did not return after Close")
	}
}

func TestServeReturnsShutdownError(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	handlerStarted := make(chan struct{})
	block := make(chan struct{})
	defer close(block)
	srv := New(Config{Addr: addr}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-handlerStarted:
		default:
			close(handlerStarted)
		}
		<-block
	}))
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, srv, time.Nanosecond) }()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		deadline := time.Now().Add(2 * time.Second)
		for {
			resp, err := http.Get("http://" + addr)
			if err == nil {
				_ = resp.Body.Close()
				return
			}
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	select {
	case <-handlerStarted:
	case <-requestDone:
		t.Fatalf("request finished before handler could block")
	case <-time.After(2 * time.Second):
		t.Fatalf("handler was not reached before deadline")
	}
	cancel()
	select {
	case err := <-errCh:
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Serve did not return shutdown error")
	}
}
