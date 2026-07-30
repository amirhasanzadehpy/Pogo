package python

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/amirhasanzadehpy/Pogo/internal/schema"
)

const ProtocolVersion = 1

type hello struct {
	ProtocolVersion int    `json:"protocol_version"`
	Type            string `json:"type"`
	Token           string `json:"token"`
}

type request struct {
	ProtocolVersion int             `json:"protocol_version"`
	ID              string          `json:"id"`
	Method          string          `json:"method"`
	Params          json.RawMessage `json:"params"`
}

type protocolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type RemoteError struct {
	Code    string
	Message string
}

func (err *RemoteError) Error() string {
	return fmt.Sprintf("worker error %s: %s", err.Code, err.Message)
}

func marshalStrict(value any) ([]byte, error) {
	buffer := &limitedBuffer{maximum: MaxFrameSize + 1}
	encoder := json.NewEncoder(buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("encode IPC JSON: %w", err)
	}
	payload := bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'})
	return payload, nil
}

func decodeStrict(payload []byte, destination any) error {
	if err := rejectDuplicateKeys(payload); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode IPC JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("decode IPC JSON: trailing value")
	}
	return nil
}

func decodeResponse(payload []byte, expectedID string, result any) error {
	if err := rejectDuplicateKeys(payload); err != nil {
		return err
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("decode worker response: %w", err)
	}
	if len(envelope) != 4 || envelope["protocol_version"] == nil || envelope["id"] == nil || envelope["result"] == nil || envelope["error"] == nil {
		return errors.New("worker response has invalid envelope")
	}
	var version int
	if err := json.Unmarshal(envelope["protocol_version"], &version); err != nil || version != ProtocolVersion {
		return fmt.Errorf("worker protocol version mismatch")
	}
	var id string
	if err := json.Unmarshal(envelope["id"], &id); err != nil || id != expectedID {
		return fmt.Errorf("worker response id mismatch")
	}
	resultNull := bytes.Equal(bytes.TrimSpace(envelope["result"]), []byte("null"))
	errorNull := bytes.Equal(bytes.TrimSpace(envelope["error"]), []byte("null"))
	if !errorNull {
		if !resultNull {
			return errors.New("worker error response also contains a result")
		}
		var remote protocolError
		if err := decodeStrict(envelope["error"], &remote); err != nil || remote.Code == "" || remote.Message == "" {
			return errors.New("worker response has invalid structured error")
		}
		return &RemoteError{Code: remote.Code, Message: remote.Message}
	}
	if result != nil && !resultNull {
		if _, isSnapshot := result.(*schema.Snapshot); isSnapshot {
			if err := schema.ValidateWire(envelope["result"]); err != nil {
				return fmt.Errorf("validate worker schema envelope: %w", err)
			}
		}
		if err := decodeStrict(envelope["result"], result); err != nil {
			return fmt.Errorf("decode worker result: %w", err)
		}
	}
	return nil
}

type limitedBuffer struct {
	bytes.Buffer
	maximum int
}

func (buffer *limitedBuffer) Write(payload []byte) (int, error) {
	if buffer.Len()+len(payload) > buffer.maximum {
		return 0, ErrFrameTooLarge
	}
	return buffer.Buffer.Write(payload)
}

func rejectDuplicateKeys(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode IPC JSON: %w", err)
	}
	if err := consumeJSONValue(decoder, token); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode IPC JSON: trailing value")
		}
		return fmt.Errorf("decode IPC JSON: %w", err)
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder, token json.Token) error {
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("decode IPC JSON: object key is not a string")
			}
			if _, exists := keys[key]; exists {
				return fmt.Errorf("decode IPC JSON: duplicate key %q", key)
			}
			keys[key] = struct{}{}
			valueToken, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := consumeJSONValue(decoder, valueToken); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			valueToken, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := consumeJSONValue(decoder, valueToken); err != nil {
				return err
			}
		}
	default:
		return errors.New("decode IPC JSON: unexpected delimiter")
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	closingDelimiter, ok := closing.(json.Delim)
	if !ok || (delimiter == '{' && closingDelimiter != '}') || (delimiter == '[' && closingDelimiter != ']') {
		return errors.New("decode IPC JSON: mismatched delimiter")
	}
	return nil
}
