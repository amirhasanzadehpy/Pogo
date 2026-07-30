package lsp

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/tliron/commonlog"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

const (
	ServerName    = "django-orm-lsp"
	ServerVersion = "0.1.0"
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
	factory                  WorkerFactory
	workerConfigurationError error
	features                 *Features
}

type Worker interface {
	Start(context.Context, func(error))
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
		Initialize:  lifecycle.initialize,
		Initialized: lifecycle.initialized,
		Shutdown:    lifecycle.shutdown,
	}
	return lifecycle
}

func NewLifecycleContextWithFactory(ctx context.Context, cancel context.CancelFunc, logger commonlog.Logger, factory WorkerFactory, featureSets ...*Features) *Lifecycle {
	lifecycle := NewLifecycleContext(ctx, cancel, logger)
	lifecycle.factory = factory
	if len(featureSets) > 0 && featureSets[0] != nil {
		lifecycle.features = featureSets[0]
		lifecycle.handler.TextDocumentDidOpen = lifecycle.features.didOpen
		lifecycle.handler.TextDocumentDidChange = lifecycle.features.didChange
		lifecycle.handler.TextDocumentDidClose = lifecycle.features.didClose
		lifecycle.handler.TextDocumentCompletion = lifecycle.features.completion
		lifecycle.handler.TextDocumentHover = lifecycle.features.hover
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
		lifecycle.stopWorker("exit")
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
		if ctx.Method != protocol.MethodInitialized {
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
	notifyFailure := func() {
		if ctx.Notify != nil {
			ctx.Notify(string(protocol.ServerWindowShowMessage), protocol.ShowMessageParams{
				Type:    protocol.MessageTypeWarning,
				Message: "Django schema loading failed; ORM data is unavailable or stale. See the language server log for details.",
			})
		}
	}
	if lifecycle.workerConfigurationError != nil {
		notifyFailure()
		return nil
	}
	if lifecycle.worker != nil {
		lifecycle.worker.Start(lifecycle.ctx, func(_ error) {
			notifyFailure()
		})
	}
	return nil
}

func (lifecycle *Lifecycle) shutdown(_ *glsp.Context) error {
	lifecycle.info("shutdown received")
	if lifecycle.features != nil {
		lifecycle.features.Close()
	}
	lifecycle.stopWorker("shutdown")
	return nil
}

func (lifecycle *Lifecycle) StopWorker() {
	lifecycle.stopWorker("connection close")
}

func (lifecycle *Lifecycle) stopWorker(reason string) {
	if lifecycle.worker == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lifecycle.worker.Stop(ctx); err != nil && lifecycle.log != nil {
		lifecycle.log.Errorf("stop Python worker during %s: %s", reason, err)
	}
}

func (lifecycle *Lifecycle) info(message string) {
	if lifecycle.log != nil {
		lifecycle.log.Info(message)
	}
}
