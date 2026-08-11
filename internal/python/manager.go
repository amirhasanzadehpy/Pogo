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
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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
	EnvironmentFile string
	Environment     map[string]*string
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

	mu                  sync.Mutex
	cancel              context.CancelFunc
	done                chan struct{}
	lastErr             error
	activeRuntimeDir    string
	run                 func(context.Context, func(uint64, int)) (bool, error)
	workerScript        []byte
	workerEnvironment   []workerEnvironmentEntry
	platformEnvironment []workerEnvironmentEntry
	coordinatorPath     string
	requestCount        atomic.Uint64
	sessionCancel       context.CancelFunc
	refreshTimer        *time.Timer
	refreshWake         chan struct{}
	refreshEpoch        uint64
	releasedEpoch       uint64
	activeEpoch         uint64
	refreshPending      bool
	refreshDeadline     time.Time
	refreshDelay        time.Duration
	refreshStarted      time.Time
}

const schemaRefreshDebounce = 300 * time.Millisecond
const defaultSchemaLoadTimeout = 90 * time.Second

var errWorkerCleanup = errors.New("worker cleanup failed")

func (manager *Manager) RequestCount() uint64 {
	if manager == nil {
		return 0
	}
	return manager.requestCount.Load()
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
		return nil, errors.New("Python executable is required; configure an explicit interpreter or project .venv")
	}
	if !filepath.IsAbs(pythonPath) {
		pythonPath = filepath.Join(projectRoot, pythonPath)
	}
	pythonPath, err = filepath.Abs(pythonPath)
	if err != nil {
		return nil, fmt.Errorf("resolve Python executable: %w", err)
	}
	pythonPath = filepath.Clean(pythonPath)
	pythonInfo, err := os.Stat(pythonPath)
	if err != nil {
		return nil, fmt.Errorf("inspect Python executable %q: %w", pythonPath, err)
	}
	if !pythonInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("Python executable %q is not a regular file", pythonPath)
	}
	if err := validatePythonExecutable(pythonPath, pythonInfo); err != nil {
		return nil, fmt.Errorf("Python executable %q %w", pythonPath, err)
	}
	environmentFile, err := resolveWorkerEnvironmentFile(projectRoot, config.EnvironmentFile)
	if err != nil {
		return nil, err
	}
	config.EnvironmentFile = environmentFile
	environment := make(map[string]*string, len(config.Environment))
	for name, configuredValue := range config.Environment {
		if configuredValue == nil {
			environment[name] = nil
			continue
		}
		value := *configuredValue
		environment[name] = &value
	}
	config.Environment = environment
	workerEnvironment, err := loadWorkerEnvironment(config, logger)
	if err != nil {
		return nil, err
	}
	if environmentSettings, present := workerEnvironmentValue(workerEnvironment, "DJANGO_SETTINGS_MODULE"); present {
		if config.SettingsModule != "" && config.SettingsModule != environmentSettings {
			return nil, errors.New("settings module conflict between explicit settingsModule and worker environment DJANGO_SETTINGS_MODULE")
		}
	}
	platformEnvironment, err := platformWorkerEnvironment()
	if err != nil {
		return nil, err
	}
	probe := &Manager{
		config:              Config{PythonPath: pythonPath},
		workerEnvironment:   workerEnvironment,
		platformEnvironment: platformEnvironment,
		coordinatorPath:     os.Getenv("PATH"),
	}
	runtimeProbe := filepath.Join(os.TempDir(), "pogo-worker-000000000")
	addressProbe := filepath.Join(runtimeProbe, "worker.sock")
	if runtime.GOOS == "windows" {
		addressProbe = "127.0.0.1:65535"
	}
	if _, err := probe.buildWorkerEnvironment(filepath.Join(runtimeProbe, "tmp"), "unix", addressProbe, filepath.Join(runtimeProbe, "token")); err != nil {
		return nil, fmt.Errorf("validate worker environment configuration: %w", err)
	}
	if config.ConnectTimeout <= 0 {
		config.ConnectTimeout = 5 * time.Second
	}
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = defaultSchemaLoadTimeout
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
	config.Environment = nil
	manager := &Manager{
		config: config, cache: cache, log: logger,
		workerEnvironment:   workerEnvironment,
		platformEnvironment: platformEnvironment,
		coordinatorPath:     probe.coordinatorPath,
	}
	manager.workerScript = workerdaemon.IntrospectPython
	manager.run = manager.runSession
	return manager, nil
}

func (manager *Manager) Start(parent context.Context, notify func(uint64, error)) {
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
	manager.refreshPending = false
	manager.releasedEpoch = manager.refreshEpoch
	manager.refreshWake = make(chan struct{}, 1)
	if manager.refreshDelay <= 0 {
		manager.refreshDelay = schemaRefreshDebounce
	}
	manager.mu.Unlock()

	go manager.loop(ctx, done, notify)
}

func (manager *Manager) DidSave(path string) {
	if !manager.schemaAffectingPath(path) {
		return
	}
	manager.mu.Lock()
	if manager.cancel == nil {
		manager.mu.Unlock()
		return
	}
	manager.refreshEpoch++
	epoch := manager.refreshEpoch
	manager.refreshPending = false
	manager.refreshDeadline = time.Now().Add(manager.refreshDelay)
	if manager.refreshTimer != nil {
		manager.refreshTimer.Stop()
	}
	manager.refreshTimer = time.AfterFunc(manager.refreshDelay, func() {
		manager.mu.Lock()
		if manager.cancel == nil || manager.refreshEpoch != epoch || time.Now().Before(manager.refreshDeadline) {
			manager.mu.Unlock()
			return
		}
		manager.refreshPending = true
		manager.releasedEpoch = epoch
		manager.refreshStarted = time.Now()
		cancel := manager.sessionCancel
		wake := manager.refreshWake
		manager.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		select {
		case wake <- struct{}{}:
		default:
		}
	})
	manager.mu.Unlock()
}

func (manager *Manager) schemaAffectingPath(path string) bool {
	if strings.ToLower(filepath.Ext(path)) != ".py" {
		return false
	}
	graph, _ := manager.cache.Load()
	if graph != nil && graph.SchemaAffectingPath(path) {
		return true
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absolute = canonicalPath(absolute)
	if graph == nil && pathWithin(manager.config.ProjectRoot, absolute) {
		return true
	}
	if manager.config.SettingsModule != "" {
		settingsPath := filepath.Join(manager.config.ProjectRoot, filepath.FromSlash(strings.ReplaceAll(manager.config.SettingsModule, ".", "/")))
		if sameFilePath(absolute, settingsPath+".py") || sameFilePath(absolute, filepath.Join(settingsPath, "__init__.py")) {
			return true
		}
	}
	return false
}

func pathWithin(root, candidate string) bool {
	root = canonicalPath(root)
	candidate = canonicalPath(candidate)
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func sameFilePath(left, right string) bool {
	left, right = canonicalPath(left), canonicalPath(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func canonicalPath(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	ancestor := path
	var suffix []string
	for {
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return path
		}
		suffix = append([]string{filepath.Base(ancestor)}, suffix...)
		ancestor = parent
		if resolved, err := filepath.EvalSymlinks(ancestor); err == nil {
			parts := append([]string{resolved}, suffix...)
			return filepath.Clean(filepath.Join(parts...))
		}
	}
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
		manager.mu.Lock()
		manager.refreshEpoch++
		if manager.refreshTimer != nil {
			manager.refreshTimer.Stop()
			manager.refreshTimer = nil
		}
		manager.mu.Unlock()
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

func (manager *Manager) loop(ctx context.Context, done chan struct{}, notify func(uint64, error)) {
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
		manager.mu.Lock()
		requestedEpoch := manager.refreshEpoch
		releasedEpoch := manager.releasedEpoch
		wake := manager.refreshWake
		manager.mu.Unlock()
		if requestedEpoch > releasedEpoch {
			if !manager.waitForRefresh(ctx, wake, releasedEpoch) {
				return
			}
			continue
		}
		sessionContext, cancelSession := context.WithCancel(ctx)
		manager.mu.Lock()
		manager.sessionCancel = cancelSession
		attemptEpoch := manager.refreshEpoch
		manager.activeEpoch = attemptEpoch
		manager.mu.Unlock()
		loaded, err := manager.run(sessionContext, func(generation uint64, modelCount int) {
			manager.info("schema cache generation=%d models=%d", generation, modelCount)
			manager.mu.Lock()
			started := manager.refreshStarted
			manager.refreshStarted = time.Time{}
			manager.mu.Unlock()
			if !started.IsZero() {
				manager.info("schema refresh duration=%s", time.Since(started))
			}
			notified = false
			if notify != nil {
				notify(generation, nil)
			}
		})
		cancelSession()
		manager.mu.Lock()
		manager.sessionCancel = nil
		refresh := manager.refreshPending
		currentEpoch := manager.refreshEpoch
		if refresh {
			manager.refreshPending = false
		}
		wake = manager.refreshWake
		manager.mu.Unlock()
		if ctx.Err() != nil {
			finalErr = err
			return
		}
		superseded := attemptEpoch != currentEpoch
		if superseded && !refresh {
			if !manager.waitForRefresh(ctx, wake, attemptEpoch) {
				finalErr = err
				return
			}
			refresh = true
		}
		if refresh {
			if errors.Is(err, errWorkerCleanup) {
				manager.warning("schema refresh cleanup failed; retaining generation: %s", err)
				if !notified && notify != nil {
					notified = true
					notify(0, errors.New(outageMessage))
				}
				finalErr = err
				return
			}
			failures = 0
			continue
		}
		if err == nil {
			return
		}
		if errors.Is(err, errWorkerCleanup) {
			manager.warning("worker cleanup failed; refusing to start another process: %s", err)
			if !notified && notify != nil {
				notify(0, errors.New(outageMessage))
			}
			finalErr = err
			return
		}
		if loaded {
			failures = 0
			notified = false
		}
		failures++
		var retainedGeneration uint64
		if manager.cache != nil {
			_, retainedGeneration = manager.cache.Load()
		}
		manager.warning("schema_refresh_failed retained_generation=%d error=%s", retainedGeneration, err)
		if !notified {
			notified = true
			if notify != nil {
				notify(0, errors.New(outageMessage))
			}
		}
		if failures > manager.config.RestartLimit {
			manager.error("worker restart limit reached")
			if !manager.waitForRefresh(ctx, wake, currentEpoch) {
				finalErr = err
				return
			}
			failures = 0
			continue
		}
		delay := manager.config.BackoffBase << (failures - 1)
		if delay > 2*time.Second {
			delay = 2 * time.Second
		}
		refreshed, stopped := manager.waitForBackoff(ctx, wake, attemptEpoch, delay)
		if stopped {
			return
		}
		if refreshed {
			failures = 0
		}
	}
}

func (manager *Manager) waitForRefresh(ctx context.Context, wake <-chan struct{}, afterEpoch uint64) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case <-wake:
			manager.mu.Lock()
			valid := manager.refreshEpoch > afterEpoch && manager.releasedEpoch == manager.refreshEpoch && manager.refreshPending && !time.Now().Before(manager.refreshDeadline)
			if valid {
				manager.refreshPending = false
			}
			manager.mu.Unlock()
			if valid {
				return true
			}
		}
	}
}

func (manager *Manager) waitForBackoff(ctx context.Context, wake <-chan struct{}, afterEpoch uint64, delay time.Duration) (bool, bool) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, true
		case <-timer.C:
			return false, false
		case <-wake:
			manager.mu.Lock()
			valid := manager.refreshEpoch > afterEpoch && manager.releasedEpoch == manager.refreshEpoch && manager.refreshPending && !time.Now().Before(manager.refreshDeadline)
			if valid {
				manager.refreshPending = false
			}
			manager.mu.Unlock()
			if valid {
				return true, false
			}
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
		if err := os.RemoveAll(runtimeDirectory); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("%w: remove worker runtime: %v", errWorkerCleanup, err))
		}
	}()
	tempDirectory := filepath.Join(runtimeDirectory, "tmp")
	if err := os.Mkdir(tempDirectory, 0o700); err != nil {
		return false, fmt.Errorf("create worker temporary directory: %w", err)
	}

	scriptPath := filepath.Join(runtimeDirectory, "introspect.py")
	if err := os.WriteFile(scriptPath, manager.workerScript, 0o600); err != nil {
		return false, fmt.Errorf("materialize worker script: %w", err)
	}
	endpoint, err := newEndpoint(runtimeDirectory)
	if err != nil {
		return false, fmt.Errorf("create worker endpoint: %w", err)
	}
	defer func() {
		if err := endpoint.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			runErr = errors.Join(runErr, fmt.Errorf("%w: close worker endpoint: %v", errWorkerCleanup, err))
		}
	}()

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
	command.Dir = manager.config.ProjectRoot
	command.Env, err = manager.buildWorkerEnvironment(tempDirectory, endpoint.Network(), endpoint.Address(), tokenPath)
	if err != nil {
		return false, err
	}
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
				runErr = errors.Join(runErr, fmt.Errorf("%w: kill Python worker: %v", errWorkerCleanup, err))
			}
		}
		select {
		case <-waitDone:
		case <-time.After(manager.config.ShutdownTimeout):
			runErr = errors.Join(runErr, fmt.Errorf("%w: timed out reaping Python worker", errWorkerCleanup))
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
	workerClient := newClient(connection, manager.config.RequestTimeout, func() {
		manager.requestCount.Add(1)
	})
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
	generation, accepted := manager.publishGraph(ctx, graph)
	if !accepted {
		return false, nil
	}
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
					return true, fmt.Errorf("%w: kill Python worker: %v", errWorkerCleanup, err)
				}
			}
			select {
			case <-waitDone:
				processExited = true
			case <-time.After(manager.config.ShutdownTimeout):
				return true, fmt.Errorf("%w: timed out reaping Python worker after kill", errWorkerCleanup)
			}
		}
		return true, nil
	}
}

func (manager *Manager) publishGraph(ctx context.Context, graph *schema.Graph) (uint64, bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if ctx.Err() != nil || manager.refreshPending || manager.activeEpoch != manager.refreshEpoch || manager.activeEpoch != manager.releasedEpoch {
		return 0, false
	}
	return manager.cache.Replace(graph), true
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

func (manager *Manager) buildWorkerEnvironment(tempDirectory, network, address, tokenPath string) ([]string, error) {
	entries := make([]workerEnvironmentEntry, 0, len(manager.workerEnvironment)+len(manager.platformEnvironment)+10)
	entries = append(entries, manager.workerEnvironment...)
	if _, present := workerEnvironmentValue(entries, "PATH"); !present {
		pathValue := filepath.Dir(manager.config.PythonPath)
		if manager.coordinatorPath != "" {
			pathValue += string(os.PathListSeparator) + manager.coordinatorPath
		}
		entries = append(entries, workerEnvironmentEntry{name: "PATH", value: pathValue})
	}
	entries = append(entries,
		workerEnvironmentEntry{name: "PYTHONDONTWRITEBYTECODE", value: "1"},
		workerEnvironmentEntry{name: "PYTHONUNBUFFERED", value: "1"},
		workerEnvironmentEntry{name: "PYTHONUTF8", value: "1"},
		workerEnvironmentEntry{name: "TMPDIR", value: tempDirectory},
		workerEnvironmentEntry{name: "TMP", value: tempDirectory},
		workerEnvironmentEntry{name: "TEMP", value: tempDirectory},
		workerEnvironmentEntry{name: "POGO_WORKER_NETWORK", value: network},
		workerEnvironmentEntry{name: "POGO_WORKER_ADDRESS", value: address},
		workerEnvironmentEntry{name: "POGO_WORKER_TOKEN_FILE", value: tokenPath},
	)
	entries = append(entries, manager.platformEnvironment...)
	if err := validateWorkerProcessEnvironment(entries); err != nil {
		return nil, fmt.Errorf("build worker process environment: %w", err)
	}
	sortWorkerEnvironment(entries)
	environment := make([]string, len(entries))
	for index, entry := range entries {
		environment[index] = entry.name + "=" + entry.value
	}
	return environment, nil
}

func workerEnvironmentValue(entries []workerEnvironmentEntry, name string) (string, bool) {
	normalized := normalizeEnvironmentKey(name)
	for _, entry := range entries {
		if normalizeEnvironmentKey(entry.name) == normalized {
			return entry.value, true
		}
	}
	return "", false
}
