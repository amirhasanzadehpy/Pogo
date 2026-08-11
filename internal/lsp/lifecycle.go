package lsp

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/tliron/commonlog"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

const (
	ServerName    = "pogo"
	ServerVersion = "0.2.2"
)

type lifecycleState uint8

const (
	stateNew lifecycleState = iota
	stateInitialized
	stateRunning
	stateShutdown
)

// Lifecycle wraps glsp's generated handler with the one-way state transitions
// required by LSP. glsp's internal state alone permits duplicate initialize
// requests and prevents its exit callback from running after shutdown.
type Lifecycle struct {
	opMu                     sync.Mutex
	mu                       sync.Mutex
	state                    lifecycleState
	exited                   bool
	exitCode                 int
	cancel                   context.CancelFunc
	ctx                      context.Context
	log                      commonlog.Logger
	handler                  protocol.Handler
	worker                   Worker
	workerStop               sync.Once
	workerStopErr            error
	factory                  WorkerFactory
	workerConfigurationError error
	features                 *Features
	notifier                 glsp.NotifyFunc
}

type Worker interface {
	Start(context.Context, func(uint64, error))
	DidSave(string)
	Stop(context.Context) error
}

type WorkerFactory func(*protocol.InitializeParams) (Worker, error)

func NewLifecycle(cancel context.CancelFunc, logger commonlog.Logger, workers ...Worker) *Lifecycle {
	return NewLifecycleContext(context.Background(), cancel, logger, workers...)
}

func NewLifecycleContext(ctx context.Context, cancel context.CancelFunc, logger commonlog.Logger, workers ...Worker) *Lifecycle {
	lifecycle := &Lifecycle{
		cancel: cancel,
		ctx:    ctx,
		log:    logger,
	}
	if len(workers) > 0 {
		lifecycle.worker = workers[0]
	}
	lifecycle.handler = protocol.Handler{
		CancelRequest: lifecycle.cancelRequest,
		Initialize:    lifecycle.initialize,
		Initialized:   lifecycle.initialized,
		SetTrace:      lifecycle.setTrace,
		Shutdown:      lifecycle.shutdown,
	}
	return lifecycle
}

func (lifecycle *Lifecycle) cancelRequest(_ *glsp.Context, _ *protocol.CancelParams) error {
	// Feature handlers are synchronous and bounded. Accept cancellation
	// notifications so clients do not receive a method-not-supported error.
	return nil
}

func (lifecycle *Lifecycle) setTrace(_ *glsp.Context, params *protocol.SetTraceParams) error {
	protocol.SetTraceValue(params.Value)
	return nil
}

func NewLifecycleContextWithFactory(ctx context.Context, cancel context.CancelFunc, logger commonlog.Logger, factory WorkerFactory, featureSets ...*Features) *Lifecycle {
	lifecycle := NewLifecycleContext(ctx, cancel, logger)
	lifecycle.factory = factory
	if len(featureSets) > 0 && featureSets[0] != nil {
		lifecycle.features = featureSets[0]
		lifecycle.handler.TextDocumentDidOpen = lifecycle.features.didOpen
		lifecycle.handler.TextDocumentDidChange = lifecycle.features.didChange
		lifecycle.handler.TextDocumentDidClose = lifecycle.features.didClose
		lifecycle.handler.TextDocumentDidSave = lifecycle.didSave
		lifecycle.handler.TextDocumentCompletion = lifecycle.features.completion
		lifecycle.handler.TextDocumentHover = lifecycle.features.hover
		lifecycle.handler.TextDocumentDefinition = lifecycle.features.definition
		lifecycle.handler.TextDocumentSignatureHelp = lifecycle.features.signatureHelp
	}
	return lifecycle
}

// Handle implements glsp.Handler.
func (lifecycle *Lifecycle) Handle(ctx *glsp.Context) (any, bool, bool, error) {
	lifecycle.opMu.Lock()
	defer lifecycle.opMu.Unlock()

	if ctx.Method == protocol.MethodExit {
		lifecycle.mu.Lock()
		lifecycle.exited = true
		if lifecycle.state == stateShutdown {
			lifecycle.exitCode = 0
		} else {
			lifecycle.exitCode = 1
		}
		lifecycle.mu.Unlock()
		lifecycle.info("exit received")
		_ = lifecycle.stopWorker("exit")
		if lifecycle.cancel != nil {
			lifecycle.cancel()
		}
		return nil, true, true, nil
	}

	lifecycle.mu.Lock()
	state := lifecycle.state
	lifecycle.mu.Unlock()
	switch state {
	case stateNew:
		if ctx.Method != protocol.MethodInitialize {
			return nil, true, true, errors.New("server not initialized")
		}
	case stateInitialized:
		if ctx.Method == protocol.MethodInitialize {
			return nil, true, true, errors.New("initialize may only be sent once")
		}
		if ctx.Method != protocol.MethodInitialized && ctx.Method != protocol.MethodCancelRequest && ctx.Method != protocol.MethodSetTrace {
			return nil, true, true, errors.New("initialized notification not received")
		}
	case stateRunning:
		if ctx.Method == protocol.MethodInitialize {
			return nil, true, true, errors.New("initialize may only be sent once")
		}
		if ctx.Method == protocol.MethodInitialized {
			return nil, true, true, errors.New("initialized may only be sent once")
		}
	case stateShutdown:
		return nil, true, true, errors.New("server has shut down")
	}

	result, validMethod, validParams, err := lifecycle.handler.Handle(ctx)
	if err != nil || !validMethod || !validParams {
		return result, validMethod, validParams, err
	}

	switch ctx.Method {
	case protocol.MethodInitialize:
		lifecycle.mu.Lock()
		lifecycle.state = stateInitialized
		lifecycle.mu.Unlock()
	case protocol.MethodInitialized:
		lifecycle.mu.Lock()
		lifecycle.state = stateRunning
		lifecycle.mu.Unlock()
	case protocol.MethodShutdown:
		lifecycle.mu.Lock()
		lifecycle.state = stateShutdown
		lifecycle.mu.Unlock()
	}
	return result, validMethod, validParams, nil
}

func (lifecycle *Lifecycle) ExitCode() int {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	return lifecycle.exitCode
}

func (lifecycle *Lifecycle) Exited() bool {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	return lifecycle.exited
}

func (lifecycle *Lifecycle) initialize(_ *glsp.Context, params *protocol.InitializeParams) (any, error) {
	version := ServerVersion
	lifecycle.info("initialize received")
	if lifecycle.factory != nil && lifecycle.worker == nil {
		worker, err := lifecycle.factory(params)
		if err != nil {
			lifecycle.workerConfigurationError = err
			if lifecycle.log != nil {
				lifecycle.log.Errorf("configure Django worker: %s", err)
			}
		} else {
			lifecycle.worker = worker
		}
	}
	capabilities := protocol.ServerCapabilities{}
	if lifecycle.features != nil {
		capabilities = lifecycle.features.Capabilities()
	}
	return protocol.InitializeResult{
		Capabilities: capabilities,
		ServerInfo: &protocol.InitializeResultServerInfo{
			Name:    ServerName,
			Version: &version,
		},
	}, nil
}

func (lifecycle *Lifecycle) initialized(ctx *glsp.Context, _ *protocol.InitializedParams) error {
	lifecycle.info("initialized received")
	lifecycle.mu.Lock()
	lifecycle.notifier = ctx.Notify
	lifecycle.mu.Unlock()
	if lifecycle.features != nil {
		lifecycle.features.SetNotifier(lifecycle.notify)
	}
	notifyFailure := func() {
		lifecycle.notify(string(protocol.ServerWindowShowMessage), protocol.ShowMessageParams{
			Type:    protocol.MessageTypeWarning,
			Message: "Django schema loading failed; ORM data is unavailable or stale. See the language server log for details.",
		})
	}
	if lifecycle.workerConfigurationError != nil {
		notifyFailure()
		return nil
	}
	if lifecycle.worker != nil {
		lifecycle.worker.Start(lifecycle.ctx, func(generation uint64, err error) {
			if err != nil {
				go notifyFailure()
				return
			}
			if generation > 0 && lifecycle.features != nil {
				go lifecycle.features.RevalidateAll(generation)
			}
		})
	}
	return nil
}

func (lifecycle *Lifecycle) didSave(ctx *glsp.Context, params *protocol.DidSaveTextDocumentParams) error {
	if lifecycle.features == nil || params == nil {
		return nil
	}
	lifecycle.features.setNotifier(ctx)
	uri := string(params.TextDocument.URI)
	if err := lifecycle.features.save(uri, params.Text); err != nil {
		return err
	}
	if lifecycle.worker != nil {
		if path, ok := localFilePath(uri); ok {
			lifecycle.worker.DidSave(path)
		}
	}
	return nil
}

func localFilePath(uri string) (string, bool) {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme != "file" {
		return "", false
	}
	if runtime.GOOS == "windows" {
		if parsed.Host != "" && parsed.Host != "localhost" {
			return filepath.Clean(`\\` + parsed.Host + filepath.FromSlash(parsed.Path)), true
		}
		path := parsed.Path
		if len(path) >= 3 && path[0] == '/' && path[2] == ':' {
			path = path[1:]
		}
		path = filepath.FromSlash(path)
		if path == "" {
			return "", false
		}
		return filepath.Clean(path), true
	}
	if parsed.Host != "" && parsed.Host != "localhost" {
		return "", false
	}
	path := filepath.FromSlash(parsed.Path)
	if path == "" {
		return "", false
	}
	return filepath.Clean(path), true
}

func (lifecycle *Lifecycle) notify(method string, params any) {
	lifecycle.mu.Lock()
	notifier := lifecycle.notifier
	allowed := !lifecycle.exited && lifecycle.state != stateShutdown
	lifecycle.mu.Unlock()
	if allowed && notifier != nil {
		notifier(method, params)
	}
}

func (lifecycle *Lifecycle) shutdown(_ *glsp.Context) error {
	lifecycle.info("shutdown received")
	lifecycle.mu.Lock()
	lifecycle.state = stateShutdown
	lifecycle.notifier = nil
	lifecycle.mu.Unlock()
	stopErr := lifecycle.stopWorker("shutdown")
	if lifecycle.features != nil {
		lifecycle.features.Close()
	}
	return stopErr
}

func (lifecycle *Lifecycle) StopWorker() {
	_ = lifecycle.stopWorker("connection close")
}

func (lifecycle *Lifecycle) stopWorker(reason string) error {
	if lifecycle.worker == nil {
		return nil
	}
	lifecycle.workerStop.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		lifecycle.workerStopErr = lifecycle.worker.Stop(ctx)
		if lifecycle.workerStopErr != nil && lifecycle.log != nil {
			lifecycle.log.Errorf("stop Python worker during %s: %s", reason, lifecycle.workerStopErr)
		}
	})
	return lifecycle.workerStopErr
}

func (lifecycle *Lifecycle) info(message string) {
	if lifecycle.log != nil {
		lifecycle.log.Info(message)
	}
}
