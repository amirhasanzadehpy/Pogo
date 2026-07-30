package lsp

import (
	"context"
	"encoding/json"
	"strings"
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

func TestFeatureCapabilitiesAndDocumentNotifications(t *testing.T) {
	features := testFeatures(t)
	defer features.Close()
	lifecycle := NewLifecycleContextWithFactory(context.Background(), func() {}, nil, nil, features)
	result, validMethod, validParams, err := lifecycle.Handle(initializeContext(t))
	if err != nil || !validMethod || !validParams {
		t.Fatalf("initialize = (%T, %v, %v, %v)", result, validMethod, validParams, err)
	}
	initialized := result.(protocol.InitializeResult)
	syncOptions, ok := initialized.Capabilities.TextDocumentSync.(protocol.TextDocumentSyncOptions)
	if !ok || syncOptions.OpenClose == nil || !*syncOptions.OpenClose || syncOptions.Change == nil || *syncOptions.Change != protocol.TextDocumentSyncKindIncremental {
		t.Fatalf("text synchronization capability = %#v", initialized.Capabilities.TextDocumentSync)
	}
	if initialized.Capabilities.CompletionProvider == nil || initialized.Capabilities.HoverProvider != true || initialized.Capabilities.SignatureHelpProvider == nil {
		t.Fatalf("feature capabilities = %#v", initialized.Capabilities)
	}
	if got := initialized.Capabilities.CompletionProvider.TriggerCharacters; strings.Join(got, "") != "._\"'" {
		t.Fatalf("completion triggers = %v", got)
	}
	if _, _, _, err := lifecycle.Handle(&glsp.Context{Method: protocol.MethodInitialized, Params: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	open := json.RawMessage(`{"textDocument":{"uri":"file:///feature.py","languageId":"python","version":1,"text":"from myapp.models import Book\nBook.objects.filter(au)"}}`)
	if _, validMethod, validParams, err := lifecycle.Handle(&glsp.Context{Method: protocol.MethodTextDocumentDidOpen, Params: open}); err != nil || !validMethod || !validParams {
		t.Fatalf("didOpen = (%v, %v, %v)", validMethod, validParams, err)
	}
	if _, ok := features.documents.Snapshot("file:///feature.py"); !ok {
		t.Fatal("didOpen did not publish document")
	}
	change := json.RawMessage(`{"textDocument":{"uri":"file:///feature.py","version":2},"contentChanges":[{"range":{"start":{"line":1,"character":20},"end":{"line":1,"character":22}},"text":"auth"},{"range":{"start":{"line":1,"character":24},"end":{"line":1,"character":24}},"text":"or"}]}`)
	if _, validMethod, validParams, err := lifecycle.Handle(&glsp.Context{Method: protocol.MethodTextDocumentDidChange, Params: change}); err != nil || !validMethod || !validParams {
		t.Fatalf("didChange = (%v, %v, %v)", validMethod, validParams, err)
	}
	completionParams := json.RawMessage(`{"textDocument":{"uri":"file:///feature.py"},"position":{"line":1,"character":23}}`)
	result, validMethod, validParams, err = lifecycle.Handle(&glsp.Context{Method: protocol.MethodTextDocumentCompletion, Params: completionParams})
	completion, ok := result.(*protocol.CompletionList)
	if err != nil || !validMethod || !validParams || !ok || len(completion.Items) != 2 {
		t.Fatalf("completion = (%T, %v, %v, %v)", result, validMethod, validParams, err)
	}
	hoverParams := json.RawMessage(`{"textDocument":{"uri":"file:///feature.py"},"position":{"line":1,"character":23}}`)
	result, validMethod, validParams, err = lifecycle.Handle(&glsp.Context{Method: protocol.MethodTextDocumentHover, Params: hoverParams})
	if _, ok := result.(*protocol.Hover); err != nil || !validMethod || !validParams || !ok {
		t.Fatalf("hover = (%T, %v, %v, %v)", result, validMethod, validParams, err)
	}
	signatureOpen := json.RawMessage(`{"textDocument":{"uri":"file:///signature.py","languageId":"python","version":1,"text":"from myapp.models import Book\nBook.objects.active(limit=)"}}`)
	if _, validMethod, validParams, err := lifecycle.Handle(&glsp.Context{Method: protocol.MethodTextDocumentDidOpen, Params: signatureOpen}); err != nil || !validMethod || !validParams {
		t.Fatalf("signature didOpen = (%v, %v, %v)", validMethod, validParams, err)
	}
	signatureParams := json.RawMessage(`{"textDocument":{"uri":"file:///signature.py"},"position":{"line":1,"character":26}}`)
	result, validMethod, validParams, err = lifecycle.Handle(&glsp.Context{Method: protocol.MethodTextDocumentSignatureHelp, Params: signatureParams})
	help, ok := result.(*protocol.SignatureHelp)
	if err != nil || !validMethod || !validParams || !ok || help.ActiveParameter == nil || *help.ActiveParameter != 1 {
		t.Fatalf("signature help = (%#v, %v, %v, %v)", result, validMethod, validParams, err)
	}
	closeParams := json.RawMessage(`{"textDocument":{"uri":"file:///feature.py"}}`)
	if _, _, _, err := lifecycle.Handle(&glsp.Context{Method: protocol.MethodTextDocumentDidClose, Params: closeParams}); err != nil {
		t.Fatal(err)
	}
	if _, ok := features.documents.Snapshot("file:///feature.py"); ok {
		t.Fatal("didClose did not remove document")
	}
}

func initializeContext(t *testing.T) *glsp.Context {
	t.Helper()
	params := json.RawMessage(`{"processId":null,"rootUri":null,"capabilities":{}}`)
	return &glsp.Context{Method: protocol.MethodInitialize, Params: params}
}
