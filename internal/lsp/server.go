package lsp

import (
	"context"

	"github.com/tliron/commonlog"
	"github.com/tliron/glsp/server"
)

func RunStdio(ctx context.Context, cancel context.CancelFunc) int {
	logger := commonlog.GetLogger(ServerName)
	lifecycle := NewLifecycle(cancel, logger)
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
