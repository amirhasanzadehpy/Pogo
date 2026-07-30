package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Result struct {
	ExitCode         int      `json:"exit_code"`
	RSSBytes         uint64   `json:"go_rss_bytes,omitempty"`
	WorkerRSSBytes   uint64   `json:"worker_rss_bytes,omitempty"`
	RSSSamples       []uint64 `json:"go_rss_samples,omitempty"`
	WorkerRSSSamples []uint64 `json:"worker_rss_samples,omitempty"`
	CombinedSamples  []uint64 `json:"combined_rss_samples,omitempty"`
	Stderr           string   `json:"stderr,omitempty"`
}

type readEvent struct {
	body []byte
	err  error
}

func RunScenario(parent context.Context, scenario Scenario, command []string, trace io.Writer) (result Result, runErr error) {
	if err := scenario.Validate(); err != nil {
		return Result{}, err
	}
	if len(command) == 0 {
		return Result{}, errors.New("server command is required")
	}
	if trace == nil {
		trace = io.Discard
	}

	ctx, cancel := context.WithTimeout(parent, time.Duration(scenario.TimeoutMS)*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Result{}, fmt.Errorf("server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("server stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, fmt.Errorf("server stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start server: %w", err)
	}

	var stderrBuffer bytes.Buffer
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&stderrBuffer, stderr)
		close(stderrDone)
	}()

	readEvents := make(chan readEvent, 32)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		reader := bufio.NewReader(stdout)
		for {
			body, readErr := ReadFrame(reader, MaxFrameSize)
			select {
			case readEvents <- readEvent{body: body, err: readErr}:
			case <-ctx.Done():
				return
			}
			if readErr != nil {
				return
			}
		}
	}()

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	waited := false
	defer func() {
		joinCleanupError := func(cleanupErr error) {
			if runErr == nil {
				runErr = cleanupErr
			} else {
				runErr = errors.Join(runErr, cleanupErr)
			}
		}
		cancel()
		if !waited {
			_ = stdin.Close()
			if cmd.Process != nil {
				if killErr := cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
					joinCleanupError(fmt.Errorf("kill server: %w", killErr))
				}
			}
			select {
			case <-waitDone:
				waited = true
			case <-time.After(time.Second):
				joinCleanupError(errors.New("timed out reaping server process"))
			}
		}
		select {
		case <-readerDone:
		case <-time.After(time.Second):
			joinCleanupError(errors.New("timed out stopping stdout reader"))
		}
		stderrFinished := false
		select {
		case <-stderrDone:
			stderrFinished = true
		case <-time.After(time.Second):
		}
		if stderrFinished {
			result.Stderr = stderrBuffer.String()
		}
		if runErr != nil && result.Stderr != "" {
			runErr = fmt.Errorf("%w\nserver stderr:\n%s", runErr, result.Stderr)
		}
	}()

	pending := make(map[string]string)
	stdinClosed := false
	for index, step := range scenario.Steps {
		switch {
		case len(step.Send) > 0:
			method, id, describeErr := describeSend(step.Send)
			if describeErr != nil {
				return result, fmt.Errorf("step %d: %w", index+1, describeErr)
			}
			if len(id) > 0 {
				key := canonicalID(id)
				if _, exists := pending[key]; exists {
					return result, fmt.Errorf("step %d: duplicate pending request id %s", index+1, key)
				}
				pending[key] = method
				fmt.Fprintf(trace, "SEND request %s id=%s\n", method, formatID(id))
			} else {
				fmt.Fprintf(trace, "SEND notification %s\n", method)
			}
			if err := WriteFrame(stdin, step.Send); err != nil {
				return result, fmt.Errorf("step %d: %w", index+1, err)
			}
		case step.SendRaw != nil:
			fmt.Fprintln(trace, "SEND raw payload")
			if err := WriteFrame(stdin, []byte(*step.SendRaw)); err != nil {
				return result, fmt.Errorf("step %d: %w", index+1, err)
			}
		case step.Expect != nil:
			event, err := receiveEvent(ctx, readEvents)
			if err != nil {
				return result, fmt.Errorf("step %d: %w", index+1, err)
			}
			if errors.Is(event.err, io.EOF) {
				return result, fmt.Errorf("step %d: expected protocol message, received stdout EOF", index+1)
			}
			method, kind, err := assertMessage(event.body, *step.Expect, pending)
			if err != nil {
				return result, fmt.Errorf("step %d: %w", index+1, err)
			}
			if len(step.Expect.ID) > 0 {
				delete(pending, canonicalID(step.Expect.ID))
				fmt.Fprintf(trace, "RECV response %s id=%s %s\n", method, formatID(step.Expect.ID), kind)
			} else {
				fmt.Fprintf(trace, "RECV notification %s\n", method)
			}
		case step.CloseStdin:
			fmt.Fprintln(trace, "SEND eof")
			if err := stdin.Close(); err != nil {
				return result, fmt.Errorf("step %d: close server stdin: %w", index+1, err)
			}
			stdinClosed = true
		case step.ExpectEOF:
			event, err := receiveEvent(ctx, readEvents)
			if err != nil {
				return result, fmt.Errorf("step %d: %w", index+1, err)
			}
			if !errors.Is(event.err, io.EOF) {
				if event.err != nil {
					return result, fmt.Errorf("step %d: expected clean stdout EOF: %w", index+1, event.err)
				}
				return result, fmt.Errorf("step %d: expected stdout EOF, received protocol message %s", index+1, event.body)
			}
			fmt.Fprintln(trace, "RECV eof")
		case step.SampleRSS:
			rss, supported, err := sampleProcessRSS(ctx, cmd.Process.Pid)
			if err != nil {
				return result, fmt.Errorf("step %d: %w", index+1, err)
			}
			if supported {
				result.RSSBytes = max(result.RSSBytes, rss)
				result.RSSSamples = append(result.RSSSamples, rss)
				fmt.Fprintf(trace, "RSS %.2f MB\n", float64(rss)/(1024*1024))
			} else {
				fmt.Fprintln(trace, "RSS unavailable on this platform")
			}
		case step.SampleWorkerRSS:
			rss, supported, err := sampleChildProcessRSS(ctx, cmd.Process.Pid)
			if err != nil {
				return result, fmt.Errorf("step %d: %w", index+1, err)
			}
			if supported {
				result.WorkerRSSBytes = max(result.WorkerRSSBytes, rss)
				result.WorkerRSSSamples = append(result.WorkerRSSSamples, rss)
				fmt.Fprintf(trace, "WORKER RSS %.2f MB\n", float64(rss)/(1024*1024))
			} else {
				fmt.Fprintln(trace, "WORKER RSS unavailable on this platform")
			}
		case step.SampleAllRSS != nil:
			if err := sampleAllRSS(ctx, cmd.Process.Pid, *step.SampleAllRSS, &result, trace); err != nil {
				return result, fmt.Errorf("step %d: %w", index+1, err)
			}
		}
	}

	if !stdinClosed {
		_ = stdin.Close()
	}
	if len(pending) != 0 {
		return result, fmt.Errorf("scenario completed with %d pending request(s)", len(pending))
	}
	var waitErr error
	select {
	case waitErr = <-waitDone:
		waited = true
	case <-ctx.Done():
		return result, fmt.Errorf("wait for server: %w", ctx.Err())
	}
	if ctx.Err() != nil {
		return result, fmt.Errorf("scenario deadline: %w", ctx.Err())
	}
	result.ExitCode, err = processExitCode(waitErr)
	if err != nil {
		return result, err
	}
	fmt.Fprintf(trace, "EXIT code=%d expected=%d\n", result.ExitCode, scenario.ExpectExitCode)
	if result.ExitCode != scenario.ExpectExitCode {
		return result, fmt.Errorf("server exit code %d, expected %d", result.ExitCode, scenario.ExpectExitCode)
	}
	return result, nil
}

func sampleAllRSS(ctx context.Context, pid int, spec RSSSampleSpec, result *Result, trace io.Writer) error {
	if spec.SettleMS > 0 {
		select {
		case <-time.After(time.Duration(spec.SettleMS) * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for index := 0; index < spec.Count; index++ {
		goRSS, workerRSS, supported, err := sampleProcessTreeRSS(ctx, pid)
		if err != nil {
			return err
		}
		if supported {
			result.RSSSamples = append(result.RSSSamples, goRSS)
			result.WorkerRSSSamples = append(result.WorkerRSSSamples, workerRSS)
			result.CombinedSamples = append(result.CombinedSamples, goRSS+workerRSS)
			result.RSSBytes = max(result.RSSBytes, goRSS)
			result.WorkerRSSBytes = max(result.WorkerRSSBytes, workerRSS)
		}
		if index+1 < spec.Count && spec.IntervalMS > 0 {
			select {
			case <-time.After(time.Duration(spec.IntervalMS) * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if len(result.CombinedSamples) == 0 {
		fmt.Fprintln(trace, "RSS unavailable on this platform")
		return nil
	}
	fmt.Fprintf(trace, "RSS samples=%d Go-max=%.2f MiB worker-max=%.2f MiB combined-max=%.2f MiB\n",
		len(result.CombinedSamples), float64(result.RSSBytes)/(1024*1024), float64(result.WorkerRSSBytes)/(1024*1024), float64(maximum(result.CombinedSamples))/(1024*1024))
	return nil
}

func sampleProcessTreeRSS(ctx context.Context, parentPID int) (uint64, uint64, bool, error) {
	if runtime.GOOS == "windows" {
		return 0, 0, false, nil
	}
	output, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,rss=").Output()
	if err != nil {
		return 0, 0, true, fmt.Errorf("sample process tree RSS: %w", err)
	}
	var parentRSS uint64
	var workerRSS uint64
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, ppidErr := strconv.Atoi(fields[1])
		kilobytes, rssErr := strconv.ParseUint(fields[2], 10, 64)
		if pidErr != nil || ppidErr != nil || rssErr != nil {
			continue
		}
		if pid == parentPID {
			parentRSS = kilobytes * 1024
		}
		if ppid == parentPID {
			workerRSS += kilobytes * 1024
		}
	}
	if parentRSS == 0 {
		return 0, 0, true, errors.New("server process missing from RSS snapshot")
	}
	if workerRSS == 0 {
		return 0, 0, true, errors.New("worker process missing from RSS snapshot")
	}
	return parentRSS, workerRSS, true, nil
}

func maximum(values []uint64) uint64 {
	var value uint64
	for _, candidate := range values {
		value = max(value, candidate)
	}
	return value
}

func receiveEvent(ctx context.Context, events <-chan readEvent) (readEvent, error) {
	select {
	case event := <-events:
		if event.err != nil && !errors.Is(event.err, io.EOF) {
			return event, event.err
		}
		return event, nil
	case <-ctx.Done():
		return readEvent{}, ctx.Err()
	}
}

func describeSend(message json.RawMessage) (string, json.RawMessage, error) {
	var envelope struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(message, &envelope); err != nil {
		return "", nil, err
	}
	return envelope.Method, envelope.ID, nil
}

func assertMessage(body []byte, expected ExpectedMessage, pending map[string]string) (string, string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return "", "", fmt.Errorf("decode protocol message: %w", err)
	}
	var message struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Result  json.RawMessage `json:"result"`
		Error   *struct {
			Code    int64  `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &message); err != nil {
		return "", "", fmt.Errorf("decode protocol message: %w", err)
	}
	if message.JSONRPC != "2.0" {
		return "", "", fmt.Errorf("jsonrpc is %q, expected 2.0", message.JSONRPC)
	}
	if expected.Method != "" {
		if _, hasID := fields["id"]; hasID {
			return "", "", errors.New("notification must not contain id")
		}
		if _, hasResult := fields["result"]; hasResult {
			return "", "", errors.New("notification must not contain result")
		}
		if _, hasError := fields["error"]; hasError {
			return "", "", errors.New("notification must not contain error")
		}
		if message.Method != expected.Method {
			return "", "", fmt.Errorf("notification method %q, expected %q", message.Method, expected.Method)
		}
		return message.Method, "notification", nil
	}
	if _, hasID := fields["id"]; !hasID {
		return "", "", errors.New("response must contain id")
	}
	if _, hasMethod := fields["method"]; hasMethod {
		return "", "", errors.New("response must not contain method")
	}
	_, hasResult := fields["result"]
	_, hasError := fields["error"]
	if hasResult == hasError {
		return "", "", errors.New("response must contain exactly one of result or error")
	}
	if !jsonEqual(message.ID, expected.ID) {
		return "", "", fmt.Errorf("response id %s, expected %s", message.ID, expected.ID)
	}
	method, found := pending[canonicalID(message.ID)]
	if !found {
		return "", "", fmt.Errorf("response has no pending request for id %s", message.ID)
	}
	if expected.Error != nil {
		if message.Error == nil {
			return "", "", fmt.Errorf("response for %s has no error", method)
		}
		if message.Error.Code != expected.Error.Code {
			return "", "", fmt.Errorf("response error code %d, expected %d", message.Error.Code, expected.Error.Code)
		}
		if expected.Error.Message != "" && message.Error.Message != expected.Error.Message {
			return "", "", fmt.Errorf("response error message %q, expected %q", message.Error.Message, expected.Error.Message)
		}
		return method, "error", nil
	}
	if message.Error != nil {
		return "", "", fmt.Errorf("response for %s returned error %d: %s", method, message.Error.Code, message.Error.Message)
	}
	if len(expected.Result) > 0 && !jsonEqual(message.Result, expected.Result) {
		return "", "", fmt.Errorf("response result %s, expected %s", message.Result, expected.Result)
	}
	return method, "result", nil
}

func jsonEqual(left, right json.RawMessage) bool {
	var leftValue any
	var rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return bytes.Equal(bytes.TrimSpace(left), bytes.TrimSpace(right))
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func processExitCode(waitErr error) (int, error) {
	if waitErr == nil {
		return 0, nil
	}
	var exitError *exec.ExitError
	if errors.As(waitErr, &exitError) {
		return exitError.ExitCode(), nil
	}
	return 0, fmt.Errorf("wait for server: %w", waitErr)
}

func sampleProcessRSS(ctx context.Context, pid int) (uint64, bool, error) {
	if runtime.GOOS == "windows" {
		return 0, false, nil
	}
	if err := waitForSample(ctx, 25*time.Millisecond); err != nil {
		return 0, true, err
	}
	var maximum uint64
	for sample := 0; sample < 3; sample++ {
		output, err := exec.CommandContext(ctx, "ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return 0, true, fmt.Errorf("sample RSS: %w", err)
		}
		rss, err := parseRSS(output)
		if err != nil {
			return 0, true, err
		}
		if rss > maximum {
			maximum = rss
		}
		if sample < 2 {
			if err := waitForSample(ctx, 10*time.Millisecond); err != nil {
				return 0, true, err
			}
		}
	}
	return maximum, true, nil
}

func sampleChildProcessRSS(ctx context.Context, parentPID int) (uint64, bool, error) {
	if runtime.GOOS == "windows" {
		return 0, false, nil
	}
	output, err := exec.CommandContext(ctx, "pgrep", "-P", strconv.Itoa(parentPID)).Output()
	if err != nil {
		return 0, true, fmt.Errorf("locate worker process: %w", err)
	}
	childPIDs := strings.Fields(string(output))
	if len(childPIDs) == 0 {
		return 0, true, errors.New("no worker process found")
	}
	var total uint64
	for _, childPID := range childPIDs {
		pid, err := strconv.Atoi(childPID)
		if err != nil {
			return 0, true, fmt.Errorf("parse worker PID: %w", err)
		}
		rss, _, err := sampleProcessRSS(ctx, pid)
		if err != nil {
			return 0, true, err
		}
		total += rss
	}
	return total, true, nil
}

func waitForSample(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
