package python

import (
	"context"
	"net"
)

type endpoint interface {
	Network() string
	Address() string
	Accept(context.Context) (net.Conn, error)
	Seal() error
	Close() error
}
