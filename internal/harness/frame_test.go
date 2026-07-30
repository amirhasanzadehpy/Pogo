package harness

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","method":"example/astral","params":"😀"}`)
	var wire bytes.Buffer
	if err := WriteFrame(&wire, body); err != nil {
		t.Fatalf("WriteFrame() error = %v", err)
	}

	got, err := ReadFrame(bufio.NewReader(&wire), MaxFrameSize)
	if err != nil {
		t.Fatalf("ReadFrame() error = %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("ReadFrame() = %q, want %q", got, body)
	}
}

func TestReadFrameRejectsInvalidFraming(t *testing.T) {
	tests := []struct {
		name string
		wire string
		max  int
	}{
		{name: "missing length", wire: "Content-Type: application/vscode-jsonrpc; charset=utf-8\r\n\r\n{}", max: 10},
		{name: "duplicate length", wire: "Content-Length: 2\r\nContent-Length: 2\r\n\r\n{}", max: 10},
		{name: "LF header", wire: "Content-Length: 2\n\n{}", max: 10},
		{name: "oversized", wire: "Content-Length: 3\r\n\r\n{}", max: 2},
		{name: "truncated", wire: "Content-Length: 3\r\n\r\n{}", max: 10},
		{name: "invalid charset", wire: "Content-Length: 2\r\nContent-Type: application/vscode-jsonrpc; charset=latin1\r\n\r\n{}", max: 10},
		{name: "long header", wire: strings.Repeat("X", maxHeaderSize+1) + "\n", max: 10},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ReadFrame(bufio.NewReader(strings.NewReader(test.wire)), test.max); err == nil {
				t.Fatal("ReadFrame() error = nil, want framing error")
			}
		})
	}
}

func TestReadFrameCleanEOF(t *testing.T) {
	_, err := ReadFrame(bufio.NewReader(strings.NewReader("")), MaxFrameSize)
	if err != io.EOF {
		t.Fatalf("ReadFrame() error = %v, want io.EOF", err)
	}
}

func FuzzLSPReadFrame(f *testing.F) {
	for _, seed := range []string{
		"Content-Length: 2\r\n\r\n{}",
		"Content-Length: 4\r\nContent-Type: application/vscode-jsonrpc; charset=utf-8\r\n\r\nnull",
		"Content-Length: -1\r\n\r\n",
		"Content-Length: 2\n\n{}",
	} {
		f.Add(seed, uint16(1024))
	}
	f.Fuzz(func(t *testing.T, wire string, maximum uint16) {
		if len(wire) > 64*1024 {
			t.Skip()
		}
		limit := int(maximum)
		body, err := ReadFrame(bufio.NewReaderSize(strings.NewReader(wire), 17), limit)
		if err == nil {
			if limit <= 0 {
				limit = MaxFrameSize
			}
			if len(body) > limit {
				t.Fatalf("successful frame length %d exceeds %d", len(body), limit)
			}
		}
	})
}
