package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

type Scenario struct {
	Name           string `json:"name"`
	TimeoutMS      int    `json:"timeout_ms"`
	ExpectExitCode int    `json:"expect_exit_code"`
	Steps          []Step `json:"steps"`
}

type Step struct {
	Send       json.RawMessage  `json:"send,omitempty"`
	SendRaw    *string          `json:"send_raw,omitempty"`
	Expect     *ExpectedMessage `json:"expect,omitempty"`
	CloseStdin bool             `json:"close_stdin,omitempty"`
	ExpectEOF  bool             `json:"expect_eof,omitempty"`
	SampleRSS  bool             `json:"sample_rss,omitempty"`
}

type ExpectedMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *ExpectedError  `json:"error,omitempty"`
}

type ExpectedError struct {
	Code    int64  `json:"code"`
	Message string `json:"message,omitempty"`
}

func LoadScenario(path string) (Scenario, error) {
	file, err := os.Open(path)
	if err != nil {
		return Scenario{}, err
	}
	defer file.Close()

	decoder := json.NewDecoder(io.LimitReader(file, MaxFrameSize))
	decoder.DisallowUnknownFields()
	var scenario Scenario
	if err := decoder.Decode(&scenario); err != nil {
		return Scenario{}, fmt.Errorf("decode scenario: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Scenario{}, err
	}
	if err := scenario.Validate(); err != nil {
		return Scenario{}, err
	}
	return scenario, nil
}

func (scenario Scenario) Validate() error {
	if strings.TrimSpace(scenario.Name) == "" {
		return errors.New("scenario name is required")
	}
	if scenario.TimeoutMS <= 0 {
		return errors.New("scenario timeout_ms must be positive")
	}
	if len(scenario.Steps) == 0 {
		return errors.New("scenario must contain at least one step")
	}
	for index, step := range scenario.Steps {
		actions := 0
		if len(step.Send) > 0 {
			actions++
			if err := validateSend(step.Send); err != nil {
				return fmt.Errorf("step %d: %w", index+1, err)
			}
		}
		if step.SendRaw != nil {
			actions++
		}
		if step.Expect != nil {
			actions++
			if err := step.Expect.validate(); err != nil {
				return fmt.Errorf("step %d: %w", index+1, err)
			}
		}
		if step.CloseStdin {
			actions++
		}
		if step.ExpectEOF {
			actions++
		}
		if step.SampleRSS {
			actions++
		}
		if actions != 1 {
			return fmt.Errorf("step %d must contain exactly one action", index+1)
		}
	}
	return nil
}

func validateSend(message json.RawMessage) error {
	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		Method  string          `json:"method"`
		ID      json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(message, &envelope); err != nil {
		return fmt.Errorf("send must contain valid JSON: %w", err)
	}
	if envelope.JSONRPC != "2.0" {
		return errors.New("send jsonrpc must be 2.0")
	}
	if envelope.Method == "" {
		return errors.New("send method is required")
	}
	return nil
}

func (expected ExpectedMessage) validate() error {
	if len(expected.ID) == 0 && expected.Method == "" {
		return errors.New("expect requires id or method")
	}
	if len(expected.Result) > 0 && expected.Error != nil {
		return errors.New("expect cannot contain both result and error")
	}
	if expected.Method != "" && len(expected.ID) > 0 {
		return errors.New("expect cannot contain both method and id")
	}
	if len(expected.ID) > 0 && len(expected.Result) == 0 && expected.Error == nil {
		return errors.New("response expect requires result or error")
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("scenario contains multiple JSON values")
		}
		return fmt.Errorf("decode scenario trailer: %w", err)
	}
	return nil
}

func canonicalID(id json.RawMessage) string {
	return string(bytes.TrimSpace(id))
}

func formatID(id json.RawMessage) string {
	if len(id) == 0 {
		return ""
	}
	return canonicalID(id)
}

func parseRSS(output []byte) (uint64, error) {
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return 0, errors.New("ps returned no RSS value")
	}
	kilobytes, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse RSS: %w", err)
	}
	return kilobytes * 1024, nil
}
