package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLifecycleScenarios(t *testing.T) {
	root := repositoryRoot(t)
	temp := t.TempDir()
	serverPath := executablePath(temp, "pogo")
	clientPath := executablePath(temp, "testclient")
	buildCommand(t, root, serverPath, "./cmd/pogo")
	buildCommand(t, root, clientPath, "./cmd/testclient")

	scenarios := []string{
		"normal-shutdown.json",
		"exit-without-shutdown.json",
		"malformed-payload.json",
		"unknown-method.json",
		"eof.json",
		"invalid-params.json",
		"invalid-order.json",
		"worker-lifecycle.json",
	}
	for _, scenario := range scenarios {
		t.Run(strings.TrimSuffix(scenario, ".json"), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			arguments := []string{
				"-scenario", filepath.Join(root, "testdata", "requests", scenario),
				"--", serverPath,
			}
			if scenario == "normal-shutdown.json" {
				arguments = append(arguments, "-log-file", filepath.Join(temp, "protocol.log"))
			}
			if scenario == "worker-lifecycle.json" {
				arguments = append(arguments,
					"-log-file", filepath.Join(temp, "worker.log"),
					"-project", filepath.Join(root, "testdata", "sample_django_project"),
					"-settings", "sample_project.settings",
					"-python", fixturePython(root),
				)
			}
			command := exec.CommandContext(ctx, clientPath, arguments...)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("testclient failed: %v\n%s", err, output)
			}
			if !bytes.Contains(output, []byte("PASS ")) {
				t.Fatalf("testclient output has no PASS line:\n%s", output)
			}
		})
	}

	logContent, err := os.ReadFile(filepath.Join(temp, "protocol.log"))
	if err != nil {
		t.Fatalf("read protocol log: %v", err)
	}
	for _, event := range []string{"initialize received", "initialized received", "shutdown received", "exit received"} {
		if !bytes.Contains(logContent, []byte(event)) {
			t.Errorf("protocol log does not contain %q:\n%s", event, logContent)
		}
	}
	workerLog, err := os.ReadFile(filepath.Join(temp, "worker.log"))
	if err != nil {
		t.Fatalf("read worker log: %v", err)
	}
	if !bytes.Contains(workerLog, []byte("schema cache generation=1 models=7")) {
		t.Fatalf("worker log has no loaded schema generation:\n%s", workerLog)
	}
}

func executablePath(directory, name string) string {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(directory, name)
}

func fixturePython(root string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(root, ".venv-fixture", "Scripts", "python.exe")
	}
	return filepath.Join(root, ".venv-fixture", "bin", "python")
}

func TestRunRequiresScenario(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "-scenario is required") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func buildCommand(t *testing.T, root, output, packagePath string) {
	t.Helper()
	arguments := []string{"build", "-race"}
	if runtime.GOOS == "darwin" {
		arguments = append(arguments, "-ldflags=-linkmode=external")
	}
	arguments = append(arguments, "-o", output, packagePath)
	command := exec.Command("go", arguments...)
	command.Dir = root
	if buildOutput, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", packagePath, err, buildOutput)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
}
