package harness

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestScenarioValidation(t *testing.T) {
	validSend := json.RawMessage(`{"jsonrpc":"2.0","method":"exit"}`)
	tests := []struct {
		name     string
		scenario Scenario
		wantErr  bool
	}{
		{
			name: "valid",
			scenario: Scenario{
				Name:      "exit",
				TimeoutMS: 1000,
				Steps:     []Step{{Send: validSend}, {ExpectEOF: true}},
			},
		},
		{name: "missing name", scenario: Scenario{TimeoutMS: 1000, Steps: []Step{{Send: validSend}}}, wantErr: true},
		{name: "missing timeout", scenario: Scenario{Name: "invalid", Steps: []Step{{Send: validSend}}}, wantErr: true},
		{name: "multiple actions", scenario: Scenario{Name: "invalid", TimeoutMS: 1000, Steps: []Step{{Send: validSend, ExpectEOF: true}}}, wantErr: true},
		{name: "invalid send", scenario: Scenario{Name: "invalid", TimeoutMS: 1000, Steps: []Step{{Send: json.RawMessage(`[]`)}}}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.scenario.Validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestParseRSS(t *testing.T) {
	got, err := parseRSS([]byte("  10240\n"))
	if err != nil {
		t.Fatalf("parseRSS() error = %v", err)
	}
	if got != 10*1024*1024 {
		t.Fatalf("parseRSS() = %d, want %d", got, 10*1024*1024)
	}
}

func TestRunnerTimeoutReapsProcess(t *testing.T) {
	t.Setenv("POGO_HARNESS_HELPER", "hang")
	scenario := Scenario{
		Name:           "timeout",
		TimeoutMS:      50,
		ExpectExitCode: 0,
		Steps:          []Step{{ExpectEOF: true}},
	}
	_, err := RunScenario(context.Background(), scenario, []string{os.Args[0], "-test.run=^TestHarnessHelperProcess$"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("RunScenario() error = %v, want deadline error", err)
	}
	for _, cleanupFailure := range []string{"kill server", "timed out reaping", "timed out stopping stdout reader"} {
		if strings.Contains(err.Error(), cleanupFailure) {
			t.Fatalf("RunScenario() cleanup failed: %v", err)
		}
	}
}

func TestHarnessHelperProcess(t *testing.T) {
	if os.Getenv("POGO_HARNESS_HELPER") != "hang" {
		return
	}
	time.Sleep(time.Hour)
}

func TestAssertMessageEnvelopeValidation(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		expected ExpectedMessage
		pending  map[string]string
		wantErr  bool
	}{
		{
			name:     "valid null result",
			body:     `{"jsonrpc":"2.0","id":1,"result":null}`,
			expected: ExpectedMessage{ID: json.RawMessage(`1`), Result: json.RawMessage(`null`)},
			pending:  map[string]string{"1": "shutdown"},
		},
		{
			name:     "mixed request response",
			body:     `{"jsonrpc":"2.0","id":1,"method":"wrong","result":null}`,
			expected: ExpectedMessage{ID: json.RawMessage(`1`), Result: json.RawMessage(`null`)},
			pending:  map[string]string{"1": "shutdown"},
			wantErr:  true,
		},
		{
			name:     "missing id",
			body:     `{"jsonrpc":"2.0","result":null}`,
			expected: ExpectedMessage{ID: json.RawMessage(`1`), Result: json.RawMessage(`null`)},
			pending:  map[string]string{"1": "shutdown"},
			wantErr:  true,
		},
		{
			name:     "result and error",
			body:     `{"jsonrpc":"2.0","id":1,"result":null,"error":{"code":-32600}}`,
			expected: ExpectedMessage{ID: json.RawMessage(`1`), Result: json.RawMessage(`null`)},
			pending:  map[string]string{"1": "shutdown"},
			wantErr:  true,
		},
		{
			name:     "neither result nor error",
			body:     `{"jsonrpc":"2.0","id":1}`,
			expected: ExpectedMessage{ID: json.RawMessage(`1`), Result: json.RawMessage(`null`)},
			pending:  map[string]string{"1": "shutdown"},
			wantErr:  true,
		},
		{
			name:     "notification with id",
			body:     `{"jsonrpc":"2.0","id":1,"method":"window/logMessage"}`,
			expected: ExpectedMessage{Method: "window/logMessage"},
			pending:  map[string]string{},
			wantErr:  true,
		},
		{
			name:     "unknown response id",
			body:     `{"jsonrpc":"2.0","id":2,"result":null}`,
			expected: ExpectedMessage{ID: json.RawMessage(`2`), Result: json.RawMessage(`null`)},
			pending:  map[string]string{"1": "shutdown"},
			wantErr:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := assertMessage([]byte(test.body), test.expected, test.pending)
			if (err != nil) != test.wantErr {
				t.Fatalf("assertMessage() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}
