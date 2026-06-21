package runtimehttp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServerShutdownStopsServeGracefully(t *testing.T) {
	server := NewServer()
	server.Handle(http.MethodGet, "/healthz", func(*http.Request) (any, error) {
		return map[string]string{"status": "ok"}, nil
	})
	listener := mustListenLocal(t)
	errc := make(chan error, 1)

	go func() {
		errc <- server.Serve(context.Background(), listener)
	}()
	waitForHTTP(t, "http://"+listener.Addr().String()+"/healthz")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown returned error: %v", err)
	}

	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("expected Serve to exit cleanly after Shutdown, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("expected Serve to exit after Shutdown")
	}
}

func TestServerRunStopsWhenContextIsCancelled(t *testing.T) {
	server := NewServer()
	server.Handle(http.MethodGet, "/healthz", func(*http.Request) (any, error) {
		return map[string]string{"status": "ok"}, nil
	})
	addr := reserveLocalAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)

	go func() {
		errc <- server.Run(ctx, addr)
	}()
	waitForHTTP(t, "http://"+addr+"/healthz")
	cancel()

	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("expected Run to exit cleanly after context cancel, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("expected Run to exit after context cancellation")
	}
}

func mustListenLocal(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return listener
}

func reserveLocalAddr(t *testing.T) string {
	t.Helper()
	listener := mustListenLocal(t)
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

func waitForHTTP(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(url)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		} else if !errors.Is(err, net.ErrClosed) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", url)
}
