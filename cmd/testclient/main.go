package main

import (
	"context"
	"encoding/json"
	"errors"
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
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: testclient -scenario FILE [options] -- SERVER [SERVER_ARGS...]")
		flags.PrintDefaults()
	}
	scenarioPath := flags.String("scenario", "", "path to a required request scenario JSON file")
	traceMethods := flags.Bool("trace-methods", false, "print sent and received protocol methods")
	timeout := flags.Duration("timeout", 0, "override the scenario timeout")
	format := flags.String("format", "human", "output format: human or json")
	maxGoRSS := flags.Float64("max-go-rss-mib", 0, "fail if maximum Go RSS exceeds this MiB limit")
	maxCombinedRSS := flags.Float64("max-combined-rss-mib", 0, "fail if maximum combined RSS exceeds this MiB limit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *format != "human" && *format != "json" {
		fmt.Fprintln(stderr, "-format must be human or json")
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
	combinedMax := maximum(result.CombinedSamples)
	if *maxGoRSS > 0 && result.RSSBytes == 0 {
		fmt.Fprintln(stderr, "FAIL RSS gate: Go RSS unavailable")
		return 1
	}
	if *maxCombinedRSS > 0 && combinedMax == 0 {
		fmt.Fprintln(stderr, "FAIL RSS gate: combined RSS unavailable")
		return 1
	}
	if limit := uint64(*maxGoRSS * 1024 * 1024); limit > 0 && result.RSSBytes > limit {
		fmt.Fprintf(stderr, "FAIL RSS gate: Go RSS %.2f MiB exceeds %.2f MiB\n", float64(result.RSSBytes)/(1024*1024), *maxGoRSS)
		return 1
	}
	if limit := uint64(*maxCombinedRSS * 1024 * 1024); limit > 0 && combinedMax > limit {
		fmt.Fprintf(stderr, "FAIL RSS gate: combined RSS %.2f MiB exceeds %.2f MiB\n", float64(combinedMax)/(1024*1024), *maxCombinedRSS)
		return 1
	}
	if *format == "json" {
		payload := struct {
			Scenario string         `json:"scenario"`
			Passed   bool           `json:"passed"`
			Result   harness.Result `json:"result"`
		}{Scenario: scenario.Name, Passed: true, Result: result}
		if err := json.NewEncoder(stdout).Encode(payload); err != nil {
			fmt.Fprintf(stderr, "encode result: %v\n", err)
			return 1
		}
		return 0
	}
	if result.RSSBytes > 0 {
		fmt.Fprintf(stdout, "Go idle RSS: %.2f MB\n", float64(result.RSSBytes)/(1024*1024))
	}
	if result.WorkerRSSBytes > 0 {
		fmt.Fprintf(stdout, "Python idle RSS: %.2f MB\n", float64(result.WorkerRSSBytes)/(1024*1024))
	}
	fmt.Fprintf(stdout, "PASS %s\n", scenario.Name)
	return 0
}

func maximum(values []uint64) uint64 {
	var result uint64
	for _, value := range values {
		if value > result {
			result = value
		}
	}
	return result
}
