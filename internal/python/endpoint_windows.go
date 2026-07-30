//go:build windows

package python

import (
	"context"
	"net"
	"time"
)

type tcpEndpoint struct {
	listener *net.TCPListener
	address  string
}

func newEndpoint(_ string) (endpoint, error) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		return nil, err
	}
	return &tcpEndpoint{listener: listener, address: listener.Addr().String()}, nil
}

func (endpoint *tcpEndpoint) Network() string { return "tcp" }
func (endpoint *tcpEndpoint) Address() string { return endpoint.address }

func (endpoint *tcpEndpoint) Accept(ctx context.Context) (net.Conn, error) {
	deadline := time.Now().Add(100 * time.Millisecond)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	for {
		_ = endpoint.listener.SetDeadline(deadline)
		connection, err := endpoint.listener.AcceptTCP()
		if err == nil {
			return connection, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		deadline = time.Now().Add(100 * time.Millisecond)
	}
}

func (endpoint *tcpEndpoint) Seal() error  { return endpoint.listener.Close() }
func (endpoint *tcpEndpoint) Close() error { return endpoint.listener.Close() }
