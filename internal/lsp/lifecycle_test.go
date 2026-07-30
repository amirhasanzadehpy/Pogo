package lsp

import (
	"context"
	"encoding/json"
	"sync"
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

func TestWorkerStartsAfterInitializedAndStopsOnShutdown(t *testing.T) {
	worker := &fakeWorker{}
	lifecycle := NewLifecycleContext(context.Background(), func() {}, nil, worker)
	if _, _, _, err := lifecycle.Handle(initializeContext(t)); err != nil {
		t.Fatalf("initialize error = %v", err)
	}
	if starts, _ := worker.counts(); starts != 0 {
		t.Fatalf("worker starts after initialize = %d, want 0", starts)
	}
	initialized := &glsp.Context{Method: protocol.MethodInitialized, Params: json.RawMessage(`{}`)}
	if _, _, _, err := lifecycle.Handle(initialized); err != nil {
		t.Fatalf("initialized error = %v", err)
	}
	if starts, _ := worker.counts(); starts != 1 {
		t.Fatalf("worker starts after initialized = %d, want 1", starts)
	}
	if _, _, _, err := lifecycle.Handle(&glsp.Context{Method: protocol.MethodShutdown}); err != nil {
		t.Fatalf("shutdown error = %v", err)
	}
	if _, stops := worker.counts(); stops != 1 {
		t.Fatalf("worker stops after shutdown = %d, want 1", stops)
	}
}

func TestWorkerFailureNotifiesAndExitStopsWorker(t *testing.T) {
	worker := &fakeWorker{fail: true}
	lifecycle := NewLifecycleContext(context.Background(), func() {}, nil, worker)
	if _, _, _, err := lifecycle.Handle(initializeContext(t)); err != nil {
		t.Fatalf("initialize error = %v", err)
	}
	notification := make(chan protocol.ShowMessageParams, 1)
	initialized := &glsp.Context{
		Method: protocol.MethodInitialized,
		Params: json.RawMessage(`{}`),
		Notify: func(method string, params any) {
			if method != string(protocol.ServerWindowShowMessage) {
				t.Errorf("notification method = %q", method)
				return
			}
			message, ok := params.(protocol.ShowMessageParams)
			if !ok {
				t.Errorf("notification params type = %T", params)
				return
			}
			notification <- message
		},
	}
	if _, _, _, err := lifecycle.Handle(initialized); err != nil {
		t.Fatalf("initialized error = %v", err)
	}
	select {
	case message := <-notification:
		if message.Type != protocol.MessageTypeWarning || message.Message == "" {
			t.Fatalf("warning notification = %#v", message)
		}
	default:
		t.Fatal("worker failure did not send warning notification")
	}
	if _, _, _, err := lifecycle.Handle(&glsp.Context{Method: protocol.MethodExit}); err != nil {
		t.Fatalf("exit error = %v", err)
	}
	if _, stops := worker.counts(); stops != 1 {
		t.Fatalf("worker stops after exit = %d, want 1", stops)
	}
}

type fakeWorker struct {
	mu     sync.Mutex
	starts int
	stops  int
	fail   bool
}

func (worker *fakeWorker) Start(_ context.Context, notify func(error)) {
	worker.mu.Lock()
	worker.starts++
	fail := worker.fail
	worker.mu.Unlock()
	if fail && notify != nil {
		notify(context.DeadlineExceeded)
	}
}

func (worker *fakeWorker) Stop(context.Context) error {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	worker.stops++
	return nil
}

func (worker *fakeWorker) counts() (int, int) {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	return worker.starts, worker.stops
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
