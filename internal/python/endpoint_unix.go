//go:build !windows

package python

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"time"
)

type unixEndpoint struct {
	listener *net.UnixListener
	path     string
}

func newEndpoint(runtimeDirectory string) (endpoint, error) {
	path := filepath.Join(runtimeDirectory, "worker.sock")
	address, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return &unixEndpoint{listener: listener, path: path}, nil
}

func (endpoint *unixEndpoint) Network() string { return "unix" }
func (endpoint *unixEndpoint) Address() string { return endpoint.path }

func (endpoint *unixEndpoint) Accept(ctx context.Context) (net.Conn, error) {
	deadline := time.Now().Add(100 * time.Millisecond)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	for {
		if err := endpoint.listener.SetDeadline(deadline); err != nil {
			return nil, err
		}
		connection, err := endpoint.listener.AcceptUnix()
		if err == nil {
			return connection, nil
		}
		var netError net.Error
		if !errors.As(err, &netError) || !netError.Timeout() {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		deadline = time.Now().Add(100 * time.Millisecond)
		if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
			deadline = contextDeadline
		}
	}
}

func (endpoint *unixEndpoint) Seal() error {
	return endpoint.close()
}

func (endpoint *unixEndpoint) Close() error {
	return endpoint.close()
}

func (endpoint *unixEndpoint) close() error {
	err := endpoint.listener.Close()
	if errors.Is(err, net.ErrClosed) {
		err = nil
	}
	removeErr := os.Remove(endpoint.path)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return errors.Join(err, removeErr)
	}
	return errors.Join(err, removeErr)
}
