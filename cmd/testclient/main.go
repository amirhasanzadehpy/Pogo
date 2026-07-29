package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/amirhasanzadehpy/Pogo/internal/harness"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("testclient", flag.ContinueOnError)
	flags.SetOutput(stderr)
	scenarioPath := flags.String("scenario", "", "path to a request scenario JSON file")
	traceMethods := flags.Bool("trace-methods", false, "print sent and received protocol methods")
	timeout := flags.Duration("timeout", 0, "override the scenario timeout")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *scenarioPath == "" {
		fmt.Fprintln(stderr, "-scenario is required")
		return 2
	}
	command := flags.Args()
	if len(command) == 0 {
		fmt.Fprintln(stderr, "server command is required after --")
		return 2
	}

	scenario, err := harness.LoadScenario(*scenarioPath)
	if err != nil {
		fmt.Fprintf(stderr, "load scenario: %v\n", err)
		return 1
	}
	if *timeout > 0 {
		scenario.TimeoutMS = int((*timeout) / time.Millisecond)
	}
	trace := io.Discard
	if *traceMethods {
		trace = stdout
	}
	result, err := harness.RunScenario(context.Background(), scenario, command, trace)
	if err != nil {
		fmt.Fprintf(stderr, "FAIL %s: %v\n", scenario.Name, err)
		return 1
	}
	if result.RSSBytes > 0 {
		fmt.Fprintf(stdout, "Go idle RSS: %.2f MB\n", float64(result.RSSBytes)/(1024*1024))
	}
	fmt.Fprintf(stdout, "PASS %s\n", scenario.Name)
	return 0
}
