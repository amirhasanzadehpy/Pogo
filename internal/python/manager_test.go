package python

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amirhasanzadehpy/Pogo/internal/schema"
)

func TestManagerLoadsFixtureAndCleansRuntime(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python process integration in short mode")
	}
	root := repositoryRoot(t)
	pythonPath := filepath.Join(root, ".venv-fixture", "bin", "python")
	if runtime.GOOS == "windows" {
		pythonPath = filepath.Join(root, ".venv-fixture", "Scripts", "python.exe")
	}
	if _, err := os.Stat(pythonPath); err != nil {
		t.Skipf("fixture Python is unavailable: %v", err)
	}
	cache := &schema.Cache{}
	manager, err := NewManager(Config{
		ProjectRoot:    filepath.Join(root, "testdata", "sample_django_project"),
		PythonPath:     pythonPath,
		SettingsModule: "sample_project.settings",
		ConnectTimeout: 5 * time.Second,
		RequestTimeout: 10 * time.Second,
	}, cache, nil)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	notifications := make(chan error, 1)
	manager.Start(ctx, func(err error) { notifications <- err })
	deadline := time.Now().Add(10 * time.Second)
	var runtimeDirectory string
	for time.Now().Before(deadline) {
		graph, generation := cache.Load()
		if generation == 1 {
			if graph == nil || graph.ModelCount() != 7 || !graph.HasModel("myapp.Book") {
				t.Fatalf("loaded graph = %#v, generation %d", graph, generation)
			}
			book, ok := graph.ModelInfo("myapp.Book")
			if !ok || !book.Managed || !book.HasAbstractParent || book.IndexCount != 1 {
				t.Fatalf("Book model metadata = %#v, %v", book, ok)
			}
			author, ok := graph.Field("myapp.Book", "author")
			if !ok || author.HelpText() != "Author who wrote the book." {
				t.Fatalf("Book.author metadata = %#v, %v", author, ok)
			}
			if databaseType, ok := author.DBType(); !ok || databaseType != "bigint" {
				t.Fatalf("Book.author DB type = %q, %v", databaseType, ok)
			}
			manager.mu.Lock()
			runtimeDirectory = manager.activeRuntimeDir
			manager.mu.Unlock()
			break
		}
		select {
		case notification := <-notifications:
			t.Fatalf("worker notification before load: %v", notification)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if runtimeDirectory == "" {
		t.Fatal("worker did not load fixture before deadline")
	}
	if _, err := os.Stat(runtimeDirectory); err != nil {
		t.Fatalf("active runtime directory: %v", err)
	}
	stopContext, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := manager.Stop(stopContext); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if _, err := os.Stat(runtimeDirectory); !os.IsNotExist(err) {
		t.Fatalf("runtime directory remains after Stop(): %v", err)
	}
}

func TestManagerBoundsRestartsAndNotifiesOncePerOutage(t *testing.T) {
	retainedGraph, err := schema.Build(schema.Snapshot{
		SchemaVersion: 1, PositionEncoding: "utf-8-bytes", LookupTransformMaxDepth: 2, LookupPathMaxCount: 512,
		Apps: map[string]schema.App{},
	})
	if err != nil {
		t.Fatal(err)
	}
	cache := &schema.Cache{}
	cache.Replace(retainedGraph)
	manager := &Manager{config: Config{RestartLimit: 3, BackoffBase: time.Millisecond}, cache: cache}
	var attempts atomic.Int32
	manager.run = func(context.Context, func(uint64, int)) (bool, error) {
		attempts.Add(1)
		return false, errors.New("injected failure")
	}
	var notifications atomic.Int32
	manager.Start(context.Background(), func(error) { notifications.Add(1) })
	manager.mu.Lock()
	done := manager.done
	manager.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("restart loop did not stop")
	}
	if attempts.Load() != 4 {
		t.Fatalf("attempts = %d, want 4", attempts.Load())
	}
	if notifications.Load() != 1 {
		t.Fatalf("notifications = %d, want 1", notifications.Load())
	}
	graph, generation := cache.Load()
	if graph != retainedGraph || generation != 1 {
		t.Fatalf("failed reload replaced stale cache: %p, generation %d", graph, generation)
	}
}

func TestManagerBoundsImmediatePostLoadCrashes(t *testing.T) {
	manager := &Manager{config: Config{RestartLimit: 3, BackoffBase: time.Millisecond}}
	var attempts atomic.Int32
	manager.run = func(_ context.Context, loaded func(uint64, int)) (bool, error) {
		attempt := attempts.Add(1)
		loaded(uint64(attempt), 7)
		return false, errors.New("injected post-load crash")
	}
	var notifications atomic.Int32
	manager.Start(context.Background(), func(error) { notifications.Add(1) })
	manager.mu.Lock()
	done := manager.done
	manager.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("post-load crash loop did not stop")
	}
	if attempts.Load() != 4 || notifications.Load() != 1 {
		t.Fatalf("attempts=%d notifications=%d, want 4 and 1", attempts.Load(), notifications.Load())
	}
}

func TestAuthenticateRejectsWrongTokenBeforeValidHello(t *testing.T) {
	manager := &Manager{config: Config{ConnectTimeout: time.Second}}
	wrongServer, wrongWorker := net.Pipe()
	validServer, validWorker := net.Pipe()
	endpoint := &queuedEndpoint{connections: []net.Conn{wrongServer, validServer}}
	go func() {
		_ = WriteFrame(wrongWorker, hello{ProtocolVersion: 1, Type: "hello", Token: "wrong"})
		_ = wrongWorker.Close()
	}()
	go func() {
		_ = WriteFrame(validWorker, hello{ProtocolVersion: 1, Type: "hello", Token: "expected"})
	}()
	connection, err := manager.authenticate(context.Background(), endpoint, "expected")
	if err != nil {
		t.Fatalf("authenticate() error = %v", err)
	}
	_ = connection.Close()
	_ = validWorker.Close()
	if endpoint.accepted != 2 {
		t.Fatalf("accepted connections = %d, want 2", endpoint.accepted)
	}
}

func TestStopReportsFinalCleanupError(t *testing.T) {
	manager := &Manager{}
	manager.run = func(ctx context.Context, _ func(uint64, int)) (bool, error) {
		<-ctx.Done()
		return false, errors.New("injected reap failure")
	}
	manager.Start(context.Background(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Stop(ctx); err == nil || !strings.Contains(err.Error(), "injected reap failure") {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestCleanWorkerEnvironmentRemovesInheritedCredentials(t *testing.T) {
	cleaned := cleanWorkerEnvironment([]string{
		"PATH=/bin",
		"POGO_WORKER_TOKEN=stale",
		"POGO_WORKER_ADDRESS=stale",
		"pogo_worker_token_file=stale",
		"OTHER=value",
	})
	if strings.Join(cleaned, ",") != "PATH=/bin,OTHER=value" {
		t.Fatalf("clean environment = %v", cleaned)
	}
}

func TestManagerRestartsActualCrashingWorkersAndCleansEndpoints(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fault helper uses Unix endpoint paths")
	}
	root := repositoryRoot(t)
	pythonPath := filepath.Join(root, ".venv-fixture", "bin", "python")
	if _, err := os.Stat(pythonPath); err != nil {
		t.Skipf("fixture Python is unavailable: %v", err)
	}
	trackingPath := filepath.Join(t.TempDir(), "workers.txt")
	t.Setenv("POGO_TEST_TRACK", trackingPath)
	cache := &schema.Cache{}
	manager, err := NewManager(Config{
		ProjectRoot:     filepath.Join(root, "testdata", "sample_django_project"),
		PythonPath:      pythonPath,
		ConnectTimeout:  time.Second,
		RequestTimeout:  time.Second,
		ShutdownTimeout: 100 * time.Millisecond,
		RestartLimit:    3,
		BackoffBase:     time.Millisecond,
		StabilityWindow: time.Hour,
	}, cache, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.workerScript = []byte(crashingWorkerScript)
	var notifications atomic.Int32
	manager.Start(context.Background(), func(error) { notifications.Add(1) })
	manager.mu.Lock()
	done := manager.done
	manager.mu.Unlock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("actual crash restart loop did not stop")
	}
	graph, generation := cache.Load()
	if graph == nil || generation != 4 || graph.ModelCount() != 0 {
		t.Fatalf("retained crash graph = %#v, generation %d", graph, generation)
	}
	if notifications.Load() != 1 {
		t.Fatalf("crash notifications = %d, want 1", notifications.Load())
	}
	contents, err := os.ReadFile(trackingPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(contents))
	if len(lines) != 4 {
		t.Fatalf("tracked runtimes = %v", lines)
	}
	for _, runtimeDirectory := range lines {
		if _, err := os.Stat(runtimeDirectory); !os.IsNotExist(err) {
			t.Fatalf("crashed worker runtime remains at %s: %v", runtimeDirectory, err)
		}
	}
}

const crashingWorkerScript = `
import json
import os
from pathlib import Path
import socket

address = os.environ["POGO_WORKER_ADDRESS"]
token_path = Path(os.environ["POGO_WORKER_TOKEN_FILE"])
token = token_path.read_text()
token_path.unlink()
with open(os.environ["POGO_TEST_TRACK"], "a", encoding="utf-8") as tracking:
    tracking.write(str(Path(address).parent) + "\n")
connection = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
connection.connect(address)
connection.sendall((json.dumps({"protocol_version": 1, "type": "hello", "token": token}, separators=(",", ":")) + "\n").encode())
request = json.loads(connection.makefile("rb").readline())
snapshot = {
    "schema_version": 1,
    "position_encoding": "utf-8-bytes",
    "lookup_transform_max_depth": 2,
    "lookup_path_max_count": 512,
    "schema_sources": [],
    "apps": {},
}
response = {"protocol_version": 1, "id": request["id"], "result": snapshot, "error": None}
connection.sendall((json.dumps(response, separators=(",", ":")) + "\n").encode())
connection.close()
raise SystemExit(23)
`

type queuedEndpoint struct {
	mu          sync.Mutex
	connections []net.Conn
	accepted    int
}

func (endpoint *queuedEndpoint) Network() string { return "test" }
func (endpoint *queuedEndpoint) Address() string { return "test" }
func (endpoint *queuedEndpoint) Seal() error     { return nil }
func (endpoint *queuedEndpoint) Close() error    { return nil }

func (endpoint *queuedEndpoint) Accept(context.Context) (net.Conn, error) {
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	if len(endpoint.connections) == 0 {
		return nil, errors.New("no queued connection")
	}
	connection := endpoint.connections[0]
	endpoint.connections = endpoint.connections[1:]
	endpoint.accepted++
	return connection, nil
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
}
