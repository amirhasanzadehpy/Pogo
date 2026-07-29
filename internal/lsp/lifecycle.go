package lsp

import (
	"context"
	"errors"
	"sync"

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
	mu       sync.Mutex
	state    lifecycleState
	exited   bool
	exitCode int
	cancel   context.CancelFunc
	log      commonlog.Logger
	handler  protocol.Handler
}

func NewLifecycle(cancel context.CancelFunc, logger commonlog.Logger) *Lifecycle {
	lifecycle := &Lifecycle{
		cancel: cancel,
		log:    logger,
	}
	lifecycle.handler = protocol.Handler{
		Initialize:  lifecycle.initialize,
		Initialized: lifecycle.initialized,
		Shutdown:    lifecycle.shutdown,
	}
	return lifecycle
}

// Handle implements glsp.Handler.
func (lifecycle *Lifecycle) Handle(ctx *glsp.Context) (any, bool, bool, error) {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()

	if ctx.Method == protocol.MethodExit {
		lifecycle.exited = true
		if lifecycle.state == stateShutdown {
			lifecycle.exitCode = 0
		} else {
			lifecycle.exitCode = 1
		}
		lifecycle.info("exit received")
		if lifecycle.cancel != nil {
			lifecycle.cancel()
		}
		return nil, true, true, nil
	}

	switch lifecycle.state {
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
		lifecycle.state = stateInitialized
	case protocol.MethodInitialized:
		lifecycle.state = stateRunning
	case protocol.MethodShutdown:
		lifecycle.state = stateShutdown
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

func (lifecycle *Lifecycle) initialize(_ *glsp.Context, _ *protocol.InitializeParams) (any, error) {
	version := ServerVersion
	lifecycle.info("initialize received")
	return protocol.InitializeResult{
		Capabilities: protocol.ServerCapabilities{},
		ServerInfo: &protocol.InitializeResultServerInfo{
			Name:    ServerName,
			Version: &version,
		},
	}, nil
}

func (lifecycle *Lifecycle) initialized(_ *glsp.Context, _ *protocol.InitializedParams) error {
	lifecycle.info("initialized received")
	return nil
}

func (lifecycle *Lifecycle) shutdown(_ *glsp.Context) error {
	lifecycle.info("shutdown received")
	return nil
}

func (lifecycle *Lifecycle) info(message string) {
	if lifecycle.log != nil {
		lifecycle.log.Info(message)
	}
}
