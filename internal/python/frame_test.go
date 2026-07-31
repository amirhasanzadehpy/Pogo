package python

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFrameRoundTripAndBounds(t *testing.T) {
	var buffer bytes.Buffer
	message := map[string]any{"protocol_version": 1, "value": "héllo"}
	if err := WriteFrame(&buffer, message); err != nil {
		t.Fatalf("WriteFrame() error = %v", err)
	}
	payload, err := ReadFrame(bufio.NewReader(&buffer))
	if err != nil {
		t.Fatalf("ReadFrame() error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil || decoded["value"] != "héllo" {
		t.Fatalf("decoded frame = %#v, %v", decoded, err)
	}
	if _, err := ReadFrame(bufio.NewReader(bytes.NewReader([]byte("partial")))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated frame error = %v", err)
	}
	oversized := append(bytes.Repeat([]byte{'x'}, MaxFrameSize+1), '\n')
	if _, err := ReadFrame(bufio.NewReaderSize(bytes.NewReader(oversized), 1024)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversized frame error = %v", err)
	}
}

func TestFrameSupportsProductionScaleSchemaResponse(t *testing.T) {
	const legacyMaximum = 8 * 1024 * 1024
	const measuredQueraSchemaSize = 14_081_193
	value := strings.Repeat("x", measuredQueraSchemaSize)
	var buffer bytes.Buffer
	if err := WriteFrame(&buffer, map[string]string{"schema": value}); err != nil {
		t.Fatalf("WriteFrame() production-scale error = %v", err)
	}
	payload, err := ReadFrame(bufio.NewReader(&buffer))
	if err != nil {
		t.Fatalf("ReadFrame() production-scale error = %v", err)
	}
	if len(payload) <= legacyMaximum {
		t.Fatalf("production-scale frame length = %d, want > %d", len(payload), legacyMaximum)
	}
	if len(payload) <= measuredQueraSchemaSize {
		t.Fatalf("production-scale frame length = %d, want > %d", len(payload), measuredQueraSchemaSize)
	}
}

func TestClientCorrelatesResponsesAndSerializesRequests(t *testing.T) {
	clientConnection, workerConnection := net.Pipe()
	client := newClient(clientConnection, time.Second)
	defer client.Close()
	var active atomic.Int32
	var maximum atomic.Int32
	workerDone := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(workerConnection)
		for range 8 {
			payload, err := ReadFrame(reader)
			if err != nil {
				workerDone <- err
				return
			}
			var request request
			if err := decodeStrict(payload, &request); err != nil {
				workerDone <- err
				return
			}
			current := active.Add(1)
			if current > maximum.Load() {
				maximum.Store(current)
			}
			time.Sleep(time.Millisecond)
			active.Add(-1)
			response := map[string]any{
				"protocol_version": 1,
				"id":               request.ID,
				"result":           map[string]bool{"pong": true},
				"error":            nil,
			}
			if err := WriteFrame(workerConnection, response); err != nil {
				workerDone <- err
				return
			}
		}
		workerDone <- nil
	}()

	errorsFound := make(chan error, 8)
	for range 8 {
		go func() {
			var result struct {
				Pong bool `json:"pong"`
			}
			errorsFound <- client.Request(context.Background(), "worker/ping", &result)
		}()
	}
	for range 8 {
		if err := <-errorsFound; err != nil {
			t.Errorf("Request() error = %v", err)
		}
	}
	if err := <-workerDone; err != nil {
		t.Fatalf("worker error = %v", err)
	}
	if maximum.Load() != 1 {
		t.Fatalf("maximum in-flight requests = %d, want 1", maximum.Load())
	}
	_ = workerConnection.Close()
}

func TestClientPoisonsConnectionAfterProtocolAndTimeoutErrors(t *testing.T) {
	t.Run("id mismatch", func(t *testing.T) {
		clientConnection, workerConnection := net.Pipe()
		client := newClient(clientConnection, time.Second)
		go func() {
			_, _ = ReadFrame(bufio.NewReader(workerConnection))
			_ = WriteFrame(workerConnection, map[string]any{
				"protocol_version": 1,
				"id":               "wrong",
				"result":           map[string]any{},
				"error":            nil,
			})
			_ = workerConnection.Close()
		}()
		if err := client.Request(context.Background(), "worker/ping", nil); err == nil {
			t.Fatal("Request() id mismatch error = nil")
		}
		if err := client.Request(context.Background(), "worker/ping", nil); err == nil {
			t.Fatal("Request() on poisoned connection error = nil")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		clientConnection, workerConnection := net.Pipe()
		defer workerConnection.Close()
		client := newClient(clientConnection, 20*time.Millisecond)
		if err := client.Request(context.Background(), "worker/ping", nil); err == nil {
			t.Fatal("Request() timeout error = nil")
		}
	})
}

func TestClientKeepsConnectionAfterStructuredRemoteError(t *testing.T) {
	clientConnection, workerConnection := net.Pipe()
	client := newClient(clientConnection, time.Second)
	defer client.Close()
	go func() {
		reader := bufio.NewReader(workerConnection)
		first, _ := ReadFrame(reader)
		var firstRequest request
		_ = decodeStrict(first, &firstRequest)
		_ = WriteFrame(workerConnection, map[string]any{
			"protocol_version": 1,
			"id":               firstRequest.ID,
			"result":           nil,
			"error":            map[string]string{"code": "method_not_found", "message": "unknown worker method"},
		})
		second, _ := ReadFrame(reader)
		var secondRequest request
		_ = decodeStrict(second, &secondRequest)
		_ = WriteFrame(workerConnection, map[string]any{
			"protocol_version": 1,
			"id":               secondRequest.ID,
			"result":           map[string]bool{"pong": true},
			"error":            nil,
		})
		_ = workerConnection.Close()
	}()
	var remoteError *RemoteError
	if err := client.Request(context.Background(), "unknown", nil); !errors.As(err, &remoteError) {
		t.Fatalf("first Request() error = %T %v", err, err)
	}
	var pong struct {
		Pong bool `json:"pong"`
	}
	if err := client.Request(context.Background(), "worker/ping", &pong); err != nil || !pong.Pong {
		t.Fatalf("second Request() = %#v, %v", pong, err)
	}
}

func TestStrictJSONRejectsDuplicateKeysAndOversizedWrites(t *testing.T) {
	duplicate := []byte(`{"protocol_version":1,"protocol_version":1,"id":"1","result":{},"error":null}`)
	if err := decodeResponse(duplicate, "1", nil); err == nil {
		t.Fatal("decodeResponse() duplicate key error = nil")
	}
	var buffer bytes.Buffer
	if err := WriteFrame(&buffer, map[string]string{"value": string(bytes.Repeat([]byte{'x'}, MaxFrameSize))}); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("WriteFrame() oversized error = %v", err)
	}
	if buffer.Len() != 0 {
		t.Fatalf("oversized write emitted %d bytes", buffer.Len())
	}
}

func FuzzIPCReadFrame(f *testing.F) {
	for _, seed := range []string{"{}\n", "\n", "partial", "{}\n{}\n", "{\"value\":\"héllo 😀\"}\n", " \r\n"} {
		f.Add(seed, uint8(8))
	}
	f.Fuzz(func(t *testing.T, wire string, bufferSize uint8) {
		if len(wire) > 64*1024 {
			t.Skip()
		}
		payload, err := ReadFrame(bufio.NewReaderSize(strings.NewReader(wire), int(bufferSize)+1))
		if err == nil && (len(payload) == 0 || len(payload) > MaxFrameSize || bytes.Contains(payload, []byte{'\n'})) {
			t.Fatalf("invalid successful frame length=%d payload=%q", len(payload), payload)
		}
	})
}
