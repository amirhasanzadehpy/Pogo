package python

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/amirhasanzadehpy/Pogo/internal/schema"
	workerdaemon "github.com/amirhasanzadehpy/Pogo/src/daemon"
)

const outageMessage = "Django schema loading failed; ORM data is unavailable or stale. See the language server log for details."

type Logger interface {
	Infof(string, ...any)
	Warningf(string, ...any)
	Errorf(string, ...any)
}

type Config struct {
	ProjectRoot     string
	PythonPath      string
	SettingsModule  string
	ConnectTimeout  time.Duration
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
	RestartLimit    int
	BackoffBase     time.Duration
	StabilityWindow time.Duration
}

type Manager struct {
	config Config
	cache  *schema.Cache
	log    Logger

	mu               sync.Mutex
	cancel           context.CancelFunc
	done             chan struct{}
	lastErr          error
	activeRuntimeDir string
	run              func(context.Context, func(uint64, int)) (bool, error)
	workerScript     []byte
}

func NewManager(config Config, cache *schema.Cache, logger Logger) (*Manager, error) {
	if cache == nil {
		return nil, errors.New("schema cache is required")
	}
	projectRoot, err := filepath.Abs(config.ProjectRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve project root: %w", err)
	}
	pythonPath := config.PythonPath
	if pythonPath == "" {
		pythonPath = "python3"
	}
	if strings.ContainsRune(pythonPath, filepath.Separator) && !filepath.IsAbs(pythonPath) {
		pythonPath, err = filepath.Abs(pythonPath)
		if err != nil {
			return nil, fmt.Errorf("resolve Python executable: %w", err)
		}
	}
	if config.ConnectTimeout <= 0 {
		config.ConnectTimeout = 5 * time.Second
	}
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = 30 * time.Second
	}
	if config.ShutdownTimeout <= 0 {
		config.ShutdownTimeout = time.Second
	}
	if config.RestartLimit <= 0 {
		config.RestartLimit = 5
	}
	if config.BackoffBase <= 0 {
		config.BackoffBase = 100 * time.Millisecond
	}
	if config.StabilityWindow <= 0 {
		config.StabilityWindow = 10 * time.Second
	}
	config.ProjectRoot = projectRoot
	config.PythonPath = pythonPath
	manager := &Manager{config: config, cache: cache, log: logger}
	manager.workerScript = workerdaemon.IntrospectPython
	manager.run = manager.runSession
	return manager, nil
}

func (manager *Manager) Start(parent context.Context, notify func(error)) {
	manager.mu.Lock()
	if manager.cancel != nil {
		manager.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	manager.cancel = cancel
	manager.done = done
	manager.lastErr = nil
	manager.mu.Unlock()

	go manager.loop(ctx, done, notify)
}

func (manager *Manager) Stop(ctx context.Context) error {
	manager.mu.Lock()
	cancel := manager.cancel
	done := manager.done
	lastErr := manager.lastErr
	manager.mu.Unlock()
	if done == nil {
		return lastErr
	}
	if cancel != nil {
		cancel()
	}
	select {
	case <-done:
		manager.mu.Lock()
		err := manager.lastErr
		manager.mu.Unlock()
		return err
	case <-ctx.Done():
		return fmt.Errorf("stop worker supervisor: %w", ctx.Err())
	}
}

func (manager *Manager) loop(ctx context.Context, done chan struct{}, notify func(error)) {
	var finalErr error
	defer func() {
		manager.mu.Lock()
		if manager.done == done {
			manager.cancel = nil
			manager.done = nil
			manager.lastErr = finalErr
		}
		manager.mu.Unlock()
		close(done)
	}()

	failures := 0
	notified := false
	for {
		loaded, err := manager.run(ctx, func(generation uint64, modelCount int) {
			manager.info("schema cache generation=%d models=%d", generation, modelCount)
		})
		if ctx.Err() != nil {
			finalErr = err
			return
		}
		if err == nil {
			return
		}
		if loaded {
			failures = 0
			notified = false
		}
		failures++
		manager.warning("worker session failed: %s", err)
		if !notified {
			notified = true
			if notify != nil {
				notify(errors.New(outageMessage))
			}
		}
		if failures > manager.config.RestartLimit {
			manager.error("worker restart limit reached")
			return
		}
		delay := manager.config.BackoffBase << (failures - 1)
		if delay > 2*time.Second {
			delay = 2 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}

func (manager *Manager) runSession(ctx context.Context, loaded func(uint64, int)) (wasLoaded bool, runErr error) {
	runtimeDirectory, err := os.MkdirTemp("", "pogo-worker-")
	if err != nil {
		return false, fmt.Errorf("create worker runtime: %w", err)
	}
	if err := os.Chmod(runtimeDirectory, 0o700); err != nil {
		_ = os.RemoveAll(runtimeDirectory)
		return false, fmt.Errorf("secure worker runtime: %w", err)
	}
	manager.setRuntimeDirectory(runtimeDirectory)
	defer func() {
		manager.setRuntimeDirectory("")
		if err := os.RemoveAll(runtimeDirectory); err != nil && runErr == nil {
			runErr = fmt.Errorf("remove worker runtime: %w", err)
		}
	}()

	scriptPath := filepath.Join(runtimeDirectory, "introspect.py")
	if err := os.WriteFile(scriptPath, manager.workerScript, 0o600); err != nil {
		return false, fmt.Errorf("materialize worker script: %w", err)
	}
	endpoint, err := newEndpoint(runtimeDirectory)
	if err != nil {
		return false, fmt.Errorf("create worker endpoint: %w", err)
	}
	defer func() { _ = endpoint.Close() }()

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return false, fmt.Errorf("create worker token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	tokenPath := filepath.Join(runtimeDirectory, "token")
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		return false, fmt.Errorf("materialize worker token: %w", err)
	}
	arguments := []string{scriptPath, "--project", manager.config.ProjectRoot, "--connect"}
	if manager.config.SettingsModule != "" {
		arguments = append(arguments, "--settings", manager.config.SettingsModule)
	}
	command := exec.Command(manager.config.PythonPath, arguments...)
	configureProcess(command)
	command.Env = append(cleanWorkerEnvironment(os.Environ()),
		"PYTHONDONTWRITEBYTECODE=1",
		"POGO_WORKER_NETWORK="+endpoint.Network(),
		"POGO_WORKER_ADDRESS="+endpoint.Address(),
		"POGO_WORKER_TOKEN_FILE="+tokenPath,
	)
	output := &workerOutput{log: manager.log}
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		return false, fmt.Errorf("start Python worker: %w", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()
	processExited := false
	defer func() {
		if processExited {
			return
		}
		if command.Process != nil {
			if err := killProcess(command.Process); err != nil && !errors.Is(err, os.ErrProcessDone) {
				runErr = errors.Join(runErr, fmt.Errorf("kill Python worker: %w", err))
			}
		}
		select {
		case <-waitDone:
		case <-time.After(manager.config.ShutdownTimeout):
			runErr = errors.Join(runErr, errors.New("timed out reaping Python worker"))
		}
	}()

	connection, err := manager.authenticate(ctx, endpoint, token)
	if err != nil {
		if ctx.Err() != nil {
			return false, nil
		}
		return false, err
	}
	defer connection.Close()
	if err := endpoint.Seal(); err != nil && !errors.Is(err, net.ErrClosed) {
		return false, fmt.Errorf("seal worker endpoint: %w", err)
	}
	workerClient := newClient(connection, manager.config.RequestTimeout)
	var snapshot schema.Snapshot
	requestContext, cancelRequest := context.WithTimeout(ctx, manager.config.RequestTimeout)
	err = workerClient.Request(requestContext, "schema/load", &snapshot)
	cancelRequest()
	if err != nil {
		if ctx.Err() != nil {
			return false, nil
		}
		return false, fmt.Errorf("load Django schema: %w", err)
	}
	graph, err := schema.Build(snapshot)
	if err != nil {
		return false, fmt.Errorf("validate Django schema: %w", err)
	}
	generation := manager.cache.Replace(graph)
	wasLoaded = true
	loaded(generation, graph.ModelCount())
	loadedAt := time.Now()

	select {
	case waitErr := <-waitDone:
		processExited = true
		stable := time.Since(loadedAt) >= manager.config.StabilityWindow
		if waitErr == nil {
			return stable, errors.New("Python worker exited unexpectedly")
		}
		return stable, fmt.Errorf("Python worker crashed: %w", waitErr)
	case <-ctx.Done():
		shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), manager.config.ShutdownTimeout)
		_ = workerClient.Request(shutdownContext, "worker/shutdown", nil)
		cancelShutdown()
		_ = workerClient.Close()
		select {
		case <-waitDone:
			processExited = true
		case <-time.After(manager.config.ShutdownTimeout):
			if command.Process != nil {
				if err := killProcess(command.Process); err != nil && !errors.Is(err, os.ErrProcessDone) {
					return true, fmt.Errorf("kill Python worker: %w", err)
				}
			}
			select {
			case <-waitDone:
				processExited = true
			case <-time.After(manager.config.ShutdownTimeout):
				return true, errors.New("timed out reaping Python worker after kill")
			}
		}
		return true, nil
	}
}

func (manager *Manager) authenticate(parent context.Context, endpoint endpoint, token string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(parent, manager.config.ConnectTimeout)
	defer cancel()
	for {
		connection, err := endpoint.Accept(ctx)
		if err != nil {
			return nil, fmt.Errorf("accept worker connection: %w", err)
		}
		deadline, _ := ctx.Deadline()
		_ = connection.SetDeadline(deadline)
		cancelDone := make(chan struct{})
		stopCancellation := context.AfterFunc(ctx, func() {
			_ = connection.Close()
			close(cancelDone)
		})
		payload, err := ReadFrame(bufio.NewReader(connection))
		if !stopCancellation() {
			<-cancelDone
		}
		if err != nil {
			_ = connection.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		if err := rejectDuplicateKeys(payload); err != nil {
			_ = connection.Close()
			continue
		}
		var candidate struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(payload, &candidate); err != nil || subtle.ConstantTimeCompare([]byte(candidate.Token), []byte(token)) != 1 {
			_ = connection.Close()
			continue
		}
		var greeting hello
		if err := decodeStrict(payload, &greeting); err != nil || greeting.ProtocolVersion != ProtocolVersion || greeting.Type != "hello" {
			_ = connection.Close()
			return nil, errors.New("worker sent an invalid hello")
		}
		_ = connection.SetDeadline(time.Time{})
		return connection, nil
	}
}

func (manager *Manager) setRuntimeDirectory(path string) {
	manager.mu.Lock()
	manager.activeRuntimeDir = path
	manager.mu.Unlock()
}

func (manager *Manager) info(format string, values ...any) {
	if manager.log != nil {
		manager.log.Infof(format, values...)
	}
}

func (manager *Manager) warning(format string, values ...any) {
	if manager.log != nil {
		manager.log.Warningf(format, values...)
	}
}

func (manager *Manager) error(format string, values ...any) {
	if manager.log != nil {
		manager.log.Errorf(format, values...)
	}
}

type workerOutput struct {
	log Logger
	mu  sync.Mutex
}

func (output *workerOutput) Write(payload []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	message := strings.TrimSpace(string(payload))
	if message != "" && output.log != nil {
		output.log.Infof("python worker: %s", message)
	}
	return len(payload), nil
}

var _ io.Writer = (*workerOutput)(nil)

func cleanWorkerEnvironment(environment []string) []string {
	cleaned := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(name), "POGO_WORKER_") {
			continue
		}
		cleaned = append(cleaned, entry)
	}
	return cleaned
}
