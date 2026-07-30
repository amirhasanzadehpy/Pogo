package python

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
)

type client struct {
	mu             sync.Mutex
	connection     net.Conn
	reader         *bufio.Reader
	nextID         uint64
	requestTimeout time.Duration
	poisoned       bool
}

func newClient(connection net.Conn, requestTimeout time.Duration) *client {
	return &client{
		connection:     connection,
		reader:         bufio.NewReader(connection),
		requestTimeout: requestTimeout,
	}
}

func (client *client) Request(ctx context.Context, method string, result any) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.poisoned {
		return errors.New("worker connection is unavailable")
	}
	client.nextID++
	id := strconv.FormatUint(client.nextID, 10)
	deadline := time.Now().Add(client.requestTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := client.connection.SetDeadline(deadline); err != nil {
		return err
	}
	cancelDone := make(chan struct{})
	stopCancellation := context.AfterFunc(ctx, func() {
		_ = client.connection.SetDeadline(time.Now())
		close(cancelDone)
	})
	defer func() {
		if !stopCancellation() {
			<-cancelDone
		}
		_ = client.connection.SetDeadline(time.Time{})
	}()

	params := json.RawMessage(`{}`)
	if err := WriteFrame(client.connection, request{
		ProtocolVersion: ProtocolVersion,
		ID:              id,
		Method:          method,
		Params:          params,
	}); err != nil {
		client.poison(err)
		return fmt.Errorf("write worker request: %w", err)
	}
	payload, err := ReadFrame(client.reader)
	if err != nil {
		client.poison(err)
		return fmt.Errorf("read worker response: %w", err)
	}
	if err := decodeResponse(payload, id, result); err != nil {
		var remoteError *RemoteError
		if errors.As(err, &remoteError) {
			return err
		}
		client.poison(err)
		return err
	}
	return nil
}

func (client *client) Close() error {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.poisoned = true
	return client.connection.Close()
}

func (client *client) poison(_ error) {
	client.poisoned = true
	_ = client.connection.Close()
}
