package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestServeUntilShutdownDrainsAndLeaves(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})
	ctx, cancel := context.WithCancel(context.Background())
	var stderr bytes.Buffer
	left := false
	done := make(chan error, 1)
	go func() {
		done <- serveUntilShutdown(ctx, listener, handler, func(context.Context) error {
			left = true
			return nil
		}, &stderr)
	}()

	address := "http://" + listener.Addr().String()
	response, err := http.Get(address + "/anything")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	response.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveUntilShutdown() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("serveUntilShutdown() did not return after cancellation")
	}
	if !left {
		t.Fatalf("membership leave was not invoked during shutdown")
	}
	if !strings.Contains(stderr.String(), "shutting down") {
		t.Fatalf("stderr = %q, want shutdown notice", stderr.String())
	}
	if _, err := net.DialTimeout("tcp", listener.Addr().String(), 500*time.Millisecond); err == nil {
		t.Fatalf("listener still accepting connections after shutdown")
	}
}
