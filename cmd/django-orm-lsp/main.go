package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/amirhasanzadehpy/Pogo/internal/lsp"
	"github.com/tliron/commonlog"
	"github.com/tliron/commonlog/simple"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet(lsp.ServerName, flag.ContinueOnError)
	flags.SetOutput(stderr)
	logPath := flags.String("log-file", "", "write server logs to this file instead of stderr")
	showVersion := flags.Bool("version", false, "print the server version and exit")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected positional arguments")
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stderr, "%s %s\n", lsp.ServerName, lsp.ServerVersion)
		return 0
	}
	if err := configureLogging(*logPath); err != nil {
		fmt.Fprintf(stderr, "configure logging: %v\n", err)
		return 2
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	return lsp.RunStdio(ctx, cancel)
}

func configureLogging(path string) error {
	backend := simple.NewBackend()
	backend.Buffered = false
	commonlog.SetBackend(backend)

	if path == "" {
		commonlog.Configure(1, nil)
		return nil
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	commonlog.Configure(1, &path)
	return nil
}
