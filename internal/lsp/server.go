package lsp

import (
	"context"

	"github.com/tliron/commonlog"
	"github.com/tliron/glsp/server"
)

func RunStdio(ctx context.Context, cancel context.CancelFunc, workers ...Worker) int {
	logger := commonlog.GetLogger(ServerName)
	lifecycle := NewLifecycleContext(ctx, cancel, logger, workers...)
	defer lifecycle.StopWorker()
	stdioServer := server.NewServer(lifecycle, ServerName, false)
	stdioServer.Context = ctx

	if err := stdioServer.RunStdio(); err != nil {
		logger.Errorf("stdio server failed: %s", err)
		return 1
	}

	if cancel != nil {
		cancel()
	}
	return lifecycle.ExitCode()
}

func RunStdioWithFactory(ctx context.Context, cancel context.CancelFunc, factory WorkerFactory) int {
	logger := commonlog.GetLogger(ServerName)
	lifecycle := NewLifecycleContextWithFactory(ctx, cancel, logger, factory)
	defer lifecycle.StopWorker()
	stdioServer := server.NewServer(lifecycle, ServerName, false)
	stdioServer.Context = ctx
	if err := stdioServer.RunStdio(); err != nil {
		logger.Errorf("stdio server failed: %s", err)
		return 1
	}
	if cancel != nil {
		cancel()
	}
	return lifecycle.ExitCode()
}
