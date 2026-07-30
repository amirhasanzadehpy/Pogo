//go:build !windows

package python

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUnixEndpointIsPrivateAndSealsSocket(t *testing.T) {
	runtimeDirectory, err := os.MkdirTemp("", "pogo-endpoint-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDirectory) })
	if err := os.Chmod(runtimeDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	created, err := newEndpoint(runtimeDirectory)
	if err != nil {
		t.Fatalf("newEndpoint() error = %v", err)
	}
	defer created.Close()
	info, err := os.Stat(filepath.Join(runtimeDirectory, "worker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %o, want 600", info.Mode().Perm())
	}
	dialDone := make(chan error, 1)
	go func() {
		connection, dialErr := net.Dial("unix", created.Address())
		if dialErr == nil {
			_ = connection.Close()
		}
		dialDone <- dialErr
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	connection, err := created.Accept(ctx)
	cancel()
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	_ = connection.Close()
	if err := <-dialDone; err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	if err := created.Seal(); err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	if _, err := os.Stat(created.Address()); !os.IsNotExist(err) {
		t.Fatalf("sealed socket stat error = %v, want not exist", err)
	}
}

func TestUnixEndpointAcceptCancellation(t *testing.T) {
	runtimeDirectory, err := os.MkdirTemp("", "pogo-endpoint-cancel-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDirectory) })
	created, err := newEndpoint(runtimeDirectory)
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
