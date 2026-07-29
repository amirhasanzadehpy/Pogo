package harness

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	MaxFrameSize  = 8 << 20
	maxHeaderSize = 8 << 10
)

func WriteFrame(writer io.Writer, body []byte) error {
	header := []byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body)))
	if err := writeAll(writer, header); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	if err := writeAll(writer, body); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	return nil
}

func ReadFrame(reader *bufio.Reader, maximum int) ([]byte, error) {
	if maximum <= 0 {
		maximum = MaxFrameSize
	}

	contentLength := -1
	headerBytes := 0
	for {
		line, err := readHeaderLine(reader, maxHeaderSize-headerBytes)
		if err != nil {
			if errors.Is(err, io.EOF) && line == "" && headerBytes == 0 {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("read header: %w", err)
		}
		headerBytes += len(line)
		if headerBytes > maxHeaderSize {
			return nil, errors.New("header exceeds maximum size")
		}
		if !strings.HasSuffix(line, "\r\n") {
			return nil, errors.New("header line must end with CRLF")
		}
		if line == "\r\n" {
			break
		}

		name, value, found := strings.Cut(strings.TrimSuffix(line, "\r\n"), ":")
		if !found {
			return nil, errors.New("malformed header")
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "content-length":
			if contentLength >= 0 {
				return nil, errors.New("duplicate Content-Length header")
			}
			length, parseErr := strconv.Atoi(strings.TrimSpace(value))
			if parseErr != nil || length < 0 {
				return nil, errors.New("invalid Content-Length header")
			}
			if length > maximum {
				return nil, fmt.Errorf("frame length %d exceeds maximum %d", length, maximum)
			}
			contentLength = length
		case "content-type":
			contentType := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), " ", ""))
			if !strings.Contains(contentType, "charset=utf-8") && !strings.Contains(contentType, "charset=utf8") {
				return nil, errors.New("Content-Type charset must be UTF-8")
			}
		}
	}

	if contentLength < 0 {
		return nil, errors.New("missing Content-Length header")
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return body, nil
}

func readHeaderLine(reader *bufio.Reader, remaining int) (string, error) {
	var line strings.Builder
	for {
		fragment, err := reader.ReadSlice('\n')
		if line.Len()+len(fragment) > remaining {
			return "", errors.New("header exceeds maximum size")
		}
		line.Write(fragment)
		if err == nil {
			return line.String(), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line.String(), err
		}
	}
}

func writeAll(writer io.Writer, content []byte) error {
	for len(content) > 0 {
		written, err := writer.Write(content)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		content = content[written:]
	}
	return nil
}
