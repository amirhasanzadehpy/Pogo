package python

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

const MaxFrameSize = 8 * 1024 * 1024

var ErrFrameTooLarge = errors.New("IPC frame exceeds maximum size")

func ReadFrame(reader *bufio.Reader) ([]byte, error) {
	var frame []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(frame)+len(chunk) > MaxFrameSize+1 {
			return nil, ErrFrameTooLarge
		}
		frame = append(frame, chunk...)
		if err == nil {
			frame = bytes.TrimSuffix(frame, []byte{'\n'})
			if len(frame) == 0 {
				return nil, errors.New("empty IPC frame")
			}
			return frame, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(frame) != 0 {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
}

func WriteFrame(writer io.Writer, value any) error {
	payload, err := marshalStrict(value)
	if err != nil {
		return err
	}
	if len(payload) == 0 || len(payload) > MaxFrameSize {
		return ErrFrameTooLarge
	}
	payload = append(payload, '\n')
	for len(payload) > 0 {
		written, writeErr := writer.Write(payload)
		if writeErr != nil {
			return writeErr
		}
		if written == 0 {
			return fmt.Errorf("write IPC frame: %w", io.ErrShortWrite)
		}
		payload = payload[written:]
	}
	return nil
}
