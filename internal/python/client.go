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
	onRequest      func()
}

func newClient(connection net.Conn, requestTimeout time.Duration, hooks ...func()) *client {
	client := &client{
		connection:     connection,
		reader:         bufio.NewReader(connection),
		requestTimeout: requestTimeout,
	}
	if len(hooks) > 0 {
		client.onRequest = hooks[0]
	}
	return client
}

func (client *client) Request(ctx context.Context, method string, result any) error {
	_, err := client.request(ctx, method, result)
	return err
}

func (client *client) request(ctx context.Context, method string, result any) ([]byte, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.poisoned {
		return nil, errors.New("worker connection is unavailable")
	}
	if client.onRequest != nil {
		client.onRequest()
	}
	client.nextID++
	id := strconv.FormatUint(client.nextID, 10)
	deadline := time.Now().Add(client.requestTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := client.connection.SetDeadline(deadline); err != nil {
		return nil, err
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
		return nil, fmt.Errorf("write worker request: %w", err)
	}
	payload, err := ReadFrame(client.reader)
	if err != nil {
		client.poison(err)
		return nil, fmt.Errorf("read worker response: %w", err)
	}
	resultPayload, err := decodeResponsePayload(payload, id, result)
	if err != nil {
		var remoteError *RemoteError
		if errors.As(err, &remoteError) {
			return nil, err
		}
		client.poison(err)
		return nil, err
	}
	return resultPayload, nil
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
