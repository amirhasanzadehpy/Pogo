package lsp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

func TestLifecycleTransitions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	lifecycle := NewLifecycle(cancel, nil)

	_, validMethod, validParams, err := lifecycle.Handle(&glsp.Context{Method: protocol.MethodShutdown})
	if !validMethod || !validParams || err == nil {
		t.Fatalf("pre-initialize shutdown = (%v, %v, %v), want lifecycle error", validMethod, validParams, err)
	}

	result, validMethod, validParams, err := lifecycle.Handle(initializeContext(t))
	if err != nil || !validMethod || !validParams {
		t.Fatalf("initialize = (%T, %v, %v, %v)", result, validMethod, validParams, err)
	}
	initializeResult, ok := result.(protocol.InitializeResult)
	if !ok {
		t.Fatalf("initialize result type = %T", result)
	}
	if initializeResult.ServerInfo == nil || initializeResult.ServerInfo.Name != ServerName {
		t.Fatalf("server info = %#v", initializeResult.ServerInfo)
	}

	if _, _, _, err := lifecycle.Handle(initializeContext(t)); err == nil {
		t.Fatal("duplicate initialize error = nil")
	}
	if _, _, _, err := lifecycle.Handle(&glsp.Context{Method: "pogo/beforeInitialized"}); err == nil {
		t.Fatal("request before initialized notification error = nil")
	}

	initialized := &glsp.Context{Method: protocol.MethodInitialized, Params: json.RawMessage(`{}`)}
	if _, _, _, err := lifecycle.Handle(initialized); err != nil {
		t.Fatalf("initialized error = %v", err)
	}
	if _, validMethod, _, err := lifecycle.Handle(&glsp.Context{Method: "pogo/unknown", Params: json.RawMessage(`{}`)}); err != nil || validMethod {
		t.Fatalf("unknown method = (validMethod %v, error %v), want framework method error", validMethod, err)
	}
	if _, _, _, err := lifecycle.Handle(&glsp.Context{Method: protocol.MethodShutdown}); err != nil {
		t.Fatalf("shutdown error = %v", err)
	}
	if _, _, _, err := lifecycle.Handle(&glsp.Context{Method: "pogo/afterShutdown"}); err == nil {
		t.Fatal("post-shutdown request error = nil")
	}
	if _, _, _, err := lifecycle.Handle(&glsp.Context{Method: protocol.MethodExit}); err != nil {
		t.Fatalf("exit error = %v", err)
	}
	if !lifecycle.Exited() || lifecycle.ExitCode() != 0 {
		t.Fatalf("exit state = (%v, %d), want (true, 0)", lifecycle.Exited(), lifecycle.ExitCode())
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("exit did not cancel lifecycle context")
	}
}

func TestExitWithoutShutdown(t *testing.T) {
	lifecycle := NewLifecycle(func() {}, nil)
	if _, _, _, err := lifecycle.Handle(&glsp.Context{Method: protocol.MethodExit}); err != nil {
		t.Fatalf("exit error = %v", err)
	}
	if lifecycle.ExitCode() != 1 {
		t.Fatalf("ExitCode() = %d, want 1", lifecycle.ExitCode())
	}
}

func TestInitializeInvalidParams(t *testing.T) {
	lifecycle := NewLifecycle(func() {}, nil)
	_, validMethod, validParams, err := lifecycle.Handle(&glsp.Context{
		Method: protocol.MethodInitialize,
		Params: json.RawMessage(`[]`),
	})
	if !validMethod || validParams || err == nil {
		t.Fatalf("invalid params = (%v, %v, %v), want framework parameter error", validMethod, validParams, err)
	}
}

func initializeContext(t *testing.T) *glsp.Context {
	t.Helper()
	params := json.RawMessage(`{"processId":null,"rootUri":null,"capabilities":{}}`)
	return &glsp.Context{Method: protocol.MethodInitialize, Params: params}
}
