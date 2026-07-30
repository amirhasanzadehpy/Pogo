//go:build windows

package python

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestWindowsEndpointUsesAuthenticatedLoopbackTransport(t *testing.T) {
	created, err := newEndpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer created.Close()
	if created.Network() != "tcp" {
		t.Fatalf("Network() = %q", created.Network())
	}
	host, _, err := net.SplitHostPort(created.Address())
	if err != nil || !net.ParseIP(host).IsLoopback() {
		t.Fatalf("Address() = %q, %v", created.Address(), err)
	}
	dialDone := make(chan error, 1)
	go func() {
		connection, dialErr := net.Dial(created.Network(), created.Address())
		if dialErr == nil {
			_ = connection.Close()
		}
		dialDone <- dialErr
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	connection, err := created.Accept(ctx)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	if err := <-dialDone; err != nil {
		t.Fatal(err)
	}
	if err := created.Seal(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsEndpointAcceptCancellation(t *testing.T) {
	created, err := newEndpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer created.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := created.Accept(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Accept() error = %v, want context deadline", err)
	}
}
