package python

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amirhasanzadehpy/Pogo/internal/schema"
)

func TestManagerDefaultSchemaLoadTimeoutSupportsLargeColdProjects(t *testing.T) {
	pythonPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{ProjectRoot: t.TempDir(), PythonPath: pythonPath}, &schema.Cache{}, nil)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if manager.config.RequestTimeout != defaultSchemaLoadTimeout {
		t.Fatalf("request timeout = %s, want %s", manager.config.RequestTimeout, defaultSchemaLoadTimeout)
	}
	if manager.config.ConnectTimeout != 5*time.Second {
		t.Fatalf("connect timeout = %s, want 5s", manager.config.ConnectTimeout)
	}
	if manager.config.ShutdownTimeout != time.Second {
		t.Fatalf("shutdown timeout = %s, want 1s", manager.config.ShutdownTimeout)
	}

	const override = 17 * time.Second
	overridden, err := NewManager(Config{ProjectRoot: t.TempDir(), PythonPath: pythonPath, RequestTimeout: override}, &schema.Cache{}, nil)
	if err != nil {
		t.Fatalf("NewManager() with override error = %v", err)
	}
	if overridden.config.RequestTimeout != override {
		t.Fatalf("overridden request timeout = %s, want %s", overridden.config.RequestTimeout, override)
	}
}

func TestManagerLoadsFixtureAndCleansRuntime(t *testing.T) {
	const fixtureLoadTimeout = 30 * time.Second
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
		ConnectTimeout: 5 * time.Second,
		RequestTimeout: fixtureLoadTimeout,
	}, cache, nil)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	notifications := make(chan error, 1)
	manager.Start(ctx, func(_ uint64, err error) {
		if err != nil {
			notifications <- err
		}
	})
	deadline := time.Now().Add(fixtureLoadTimeout)
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
		SchemaVersion: schema.Version, PositionEncoding: "utf-8-bytes", LookupTransformMaxDepth: 2, LookupPathMaxCount: 512,
		Apps: map[string]schema.App{},
	})
	if err != nil {
		t.Fatal(err)
	}
	cache := &schema.Cache{}
	cache.Replace(retainedGraph)
	manager := &Manager{config: Config{RestartLimit: 3, BackoffBase: time.Millisecond}, cache: cache}
	var attempts atomic.Int32
	reached := make(chan struct{})
	manager.run = func(context.Context, func(uint64, int)) (bool, error) {
		if attempts.Add(1) == 4 {
			close(reached)
		}
		return false, errors.New("injected failure")
	}
	var notifications atomic.Int32
	manager.Start(context.Background(), func(_ uint64, err error) {
		if err != nil {
			notifications.Add(1)
		}
	})
	select {
	case <-reached:
	case <-time.After(time.Second):
		t.Fatal("restart loop did not reach degraded state")
	}
	stopContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Stop(stopContext); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("Stop() error = %v", err)
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
	outages := make(chan struct{}, 4)
	manager.Start(context.Background(), func(_ uint64, err error) {
		if err != nil {
			notifications.Add(1)
			outages <- struct{}{}
		}
	})
	for count := 0; count < 4; count++ {
		select {
		case <-outages:
		case <-time.After(time.Second):
			t.Fatal("post-load crash loop did not report every outage")
		}
	}
	stopContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = manager.Stop(stopContext)
	if attempts.Load() != 4 || notifications.Load() != 4 {
		t.Fatalf("attempts=%d notifications=%d, want 4 and 4 accepted-generation outages", attempts.Load(), notifications.Load())
	}
}

func TestManagerDebouncesSchemaAffectingSaves(t *testing.T) {
	project := t.TempDir()
	manager := &Manager{
		config: Config{ProjectRoot: project, RestartLimit: 1, BackoffBase: time.Millisecond},
		cache:  &schema.Cache{}, refreshDelay: 25 * time.Millisecond,
	}
	started := make(chan int32, 8)
	var attempts atomic.Int32
	manager.run = func(ctx context.Context, loaded func(uint64, int)) (bool, error) {
		attempt := attempts.Add(1)
		loaded(uint64(attempt), 1)
		started <- attempt
		<-ctx.Done()
		return true, ctx.Err()
	}
	var generations atomic.Int32
	manager.Start(context.Background(), func(generation uint64, err error) {
		if err == nil && generation > 0 {
			generations.Add(1)
		}
	})
	waitForAttempt(t, started, 1)
	path := filepath.Join(project, "myapp", "models.py")
	for index := 0; index < 10; index++ {
		manager.DidSave(path)
	}
	waitForAttempt(t, started, 2)
	select {
	case attempt := <-started:
		t.Fatalf("save burst started attempt %d, want one replacement", attempt)
	case <-time.After(75 * time.Millisecond):
	}
	manager.DidSave(filepath.Join(project, "README.md"))
	select {
	case attempt := <-started:
		t.Fatalf("non-Python save started attempt %d", attempt)
	case <-time.After(50 * time.Millisecond):
	}
	for want := int32(3); want <= 12; want++ {
		manager.DidSave(path)
		waitForAttempt(t, started, want)
	}
	stopContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = manager.Stop(stopContext)
	if attempts.Load() != 12 || generations.Load() != 12 {
		t.Fatalf("attempts=%d generation events=%d, want 12 and 12", attempts.Load(), generations.Load())
	}
}

func TestManagerRejectsCandidateSupersededDuringLoad(t *testing.T) {
	project := t.TempDir()
	appRoot := filepath.Join(project, "myapp")
	if err := os.MkdirAll(appRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	buildGraph := func() *schema.Graph {
		graph, err := schema.Build(schema.Snapshot{
			SchemaVersion: schema.Version, PositionEncoding: schema.PositionEncoding, LookupTransformMaxDepth: 2, LookupPathMaxCount: 512,
			Apps: map[string]schema.App{"myapp": {Label: "myapp", ImportName: "myapp", RootPath: appRoot}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return graph
	}
	retained := buildGraph()
	replacement := buildGraph()
	cache := &schema.Cache{}
	cache.Replace(retained)
	manager := &Manager{
		config: Config{ProjectRoot: project, RestartLimit: 1, BackoffBase: time.Millisecond},
		cache:  cache, refreshDelay: 30 * time.Millisecond,
	}
	var attempts atomic.Int32
	initialStarted := make(chan struct{})
	candidateStarted := make(chan struct{})
	releaseCandidate := make(chan struct{})
	rejected := make(chan struct{})
	manager.run = func(ctx context.Context, loaded func(uint64, int)) (bool, error) {
		attempt := attempts.Add(1)
		if attempt == 1 {
			loaded(1, 0)
			close(initialStarted)
			<-ctx.Done()
			return true, ctx.Err()
		}
		if attempt == 2 {
			close(candidateStarted)
			<-releaseCandidate
		}
		generation, accepted := manager.publishGraph(ctx, replacement)
		if !accepted {
			if attempt == 2 {
				close(rejected)
			}
			return false, nil
		}
		loaded(generation, 0)
		<-ctx.Done()
		return true, ctx.Err()
	}
	generations := make(chan uint64, 4)
	readerContext, cancelReaders := context.WithCancel(context.Background())
	var readers sync.WaitGroup
	defer func() {
		cancelReaders()
		readers.Wait()
	}()
	var invalidRead atomic.Bool
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for readerContext.Err() == nil {
				graph, generation := cache.Load()
				if graph == nil || generation < 1 || generation > 2 || graph != retained && graph != replacement {
					invalidRead.Store(true)
					return
				}
			}
		}()
	}
	manager.Start(context.Background(), func(generation uint64, err error) {
		if err == nil {
			generations <- generation
		}
	})
	select {
	case <-initialStarted:
	case <-time.After(time.Second):
		t.Fatal("initial worker did not start")
	}
	<-generations
	modelsPath := filepath.Join(appRoot, "models.py")
	manager.DidSave(modelsPath)
	select {
	case <-candidateStarted:
	case <-time.After(time.Second):
		t.Fatal("replacement candidate did not start")
	}
	manager.DidSave(modelsPath)
	close(releaseCandidate)
	select {
	case <-rejected:
	case <-time.After(time.Second):
		t.Fatal("superseded candidate was not rejected")
	}
	if graph, generation := cache.Load(); graph != retained || generation != 1 {
		t.Fatalf("superseded candidate published graph=%p generation=%d", graph, generation)
	}
	select {
	case generation := <-generations:
		if generation != 2 {
			t.Fatalf("replacement generation = %d", generation)
		}
	case <-time.After(time.Second):
		t.Fatal("latest save did not publish replacement")
	}
	cancelReaders()
	readers.Wait()
	if invalidRead.Load() {
		t.Fatal("concurrent cache reader observed a partial refresh state")
	}
	stopContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = manager.Stop(stopContext)
}

func TestManagerShutdownCancelsPendingRefresh(t *testing.T) {
	project := t.TempDir()
	manager := &Manager{
		config: Config{ProjectRoot: project}, cache: &schema.Cache{}, refreshDelay: 40 * time.Millisecond,
	}
	started := make(chan struct{})
	var attempts atomic.Int32
	manager.run = func(ctx context.Context, loaded func(uint64, int)) (bool, error) {
		attempts.Add(1)
		loaded(1, 0)
		close(started)
		<-ctx.Done()
		return true, nil
	}
	manager.Start(context.Background(), nil)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("initial worker did not start")
	}
	manager.DidSave(filepath.Join(project, "models.py"))
	stopContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Stop(stopContext); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if attempts.Load() != 1 {
		t.Fatalf("attempts after shutdown = %d, want 1", attempts.Load())
	}
}

func TestManagerDoesNotStartBeforeReleasedDebounceEpoch(t *testing.T) {
	project := t.TempDir()
	manager := &Manager{
		config: Config{ProjectRoot: project, RestartLimit: 3, BackoffBase: time.Millisecond},
		cache:  &schema.Cache{}, refreshDelay: 35 * time.Millisecond,
	}
	var attempts atomic.Int32
	secondStarted := make(chan time.Time, 1)
	manager.run = func(ctx context.Context, _ func(uint64, int)) (bool, error) {
		if attempts.Add(1) == 1 {
			return false, errors.New("initial startup failure")
		}
		secondStarted <- time.Now()
		<-ctx.Done()
		return false, nil
	}
	var savedAt time.Time
	manager.Start(context.Background(), func(_ uint64, err error) {
		if err != nil && savedAt.IsZero() {
			savedAt = time.Now()
			manager.DidSave(filepath.Join(project, "models.py"))
		}
	})
	select {
	case started := <-secondStarted:
		if elapsed := started.Sub(savedAt); elapsed < 30*time.Millisecond {
			t.Fatalf("replacement started after %s, before debounce release", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement did not start after debounce release")
	}
	stopContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = manager.Stop(stopContext)
}

func TestManagerSchemaSavePolicyUsesSourcesAndAppRoots(t *testing.T) {
	project := t.TempDir()
	appRoot := filepath.Join(project, "myapp")
	settings := filepath.Join(project, "sample_project", "settings.py")
	graph, err := schema.Build(schema.Snapshot{
		SchemaVersion: schema.Version, PositionEncoding: schema.PositionEncoding, LookupTransformMaxDepth: 2, LookupPathMaxCount: 512,
		Apps:          map[string]schema.App{"myapp": {Label: "myapp", ImportName: "myapp", RootPath: appRoot}},
		SchemaSources: []string{settings},
	})
	if err != nil {
		t.Fatal(err)
	}
	cache := &schema.Cache{}
	cache.Replace(graph)
	manager := &Manager{config: Config{ProjectRoot: project, SettingsModule: "sample_project.settings"}, cache: cache}
	for _, path := range []string{filepath.Join(appRoot, "models.py"), filepath.Join(appRoot, "nested", "lookups.py"), settings} {
		if !manager.schemaAffectingPath(path) {
			t.Errorf("schemaAffectingPath(%q) = false", path)
		}
	}
	for _, path := range []string{filepath.Join(project, "queries.py"), filepath.Join(project, "myapp2", "models.py"), filepath.Join(appRoot, "models.pyi")} {
		if manager.schemaAffectingPath(path) {
			t.Errorf("schemaAffectingPath(%q) = true", path)
		}
	}
}

func TestManagerFailedRefreshRetainsCacheAndNotifiesOnce(t *testing.T) {
	project := t.TempDir()
	appRoot := filepath.Join(project, "myapp")
	if err := os.MkdirAll(appRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	graph, err := schema.Build(schema.Snapshot{
		SchemaVersion: schema.Version, PositionEncoding: schema.PositionEncoding, LookupTransformMaxDepth: 2, LookupPathMaxCount: 512,
		Apps: map[string]schema.App{"myapp": {Label: "myapp", ImportName: "myapp", RootPath: appRoot}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cache := &schema.Cache{}
	cache.Replace(graph)
	manager := &Manager{
		config: Config{ProjectRoot: project, RestartLimit: 1, BackoffBase: time.Millisecond},
		cache:  cache, refreshDelay: 10 * time.Millisecond,
	}
	started := make(chan int32, 1)
	failed := make(chan struct{})
	var attempts atomic.Int32
	manager.run = func(ctx context.Context, loaded func(uint64, int)) (bool, error) {
		attempt := attempts.Add(1)
		if attempt == 1 {
			loaded(1, 0)
			started <- attempt
			<-ctx.Done()
			return true, ctx.Err()
		}
		if attempt == 3 {
			close(failed)
		}
		_, buildErr := schema.Build(schema.Snapshot{
			SchemaVersion: schema.Version, PositionEncoding: schema.PositionEncoding, LookupTransformMaxDepth: 2, LookupPathMaxCount: 512,
			Apps: map[string]schema.App{"broken": {Label: "broken", ImportName: "broken", RootPath: "relative/path"}},
		})
		return false, fmt.Errorf("validate replacement schema: %w", buildErr)
	}
	var notifications atomic.Int32
	notified := make(chan struct{}, 1)
	manager.Start(context.Background(), func(_ uint64, err error) {
		if err != nil {
			notifications.Add(1)
			notified <- struct{}{}
		}
	})
	waitForAttempt(t, started, 1)
	modelsPath := filepath.Join(appRoot, "models.py")
	if !manager.schemaAffectingPath(modelsPath) {
		t.Fatalf("models path %q was not schema-affecting", modelsPath)
	}
	manager.DidSave(modelsPath)
	select {
	case <-failed:
	case <-time.After(time.Second):
		t.Fatalf("replacement retries did not finish; attempts=%d", attempts.Load())
	}
	select {
	case <-notified:
	case <-time.After(time.Second):
		t.Fatal("replacement failure was not reported")
	}
	if current, generation := cache.Load(); current != graph || generation != 1 {
		t.Fatalf("failed refresh replaced cache: graph=%p generation=%d", current, generation)
	}
	if notifications.Load() != 1 {
		t.Fatalf("failure notifications = %d, want 1", notifications.Load())
	}
	stopContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = manager.Stop(stopContext)
}

func TestManagerRejectsMissingPythonBeforeStart(t *testing.T) {
	project := t.TempDir()
	retained, err := schema.Build(schema.Snapshot{
		SchemaVersion: schema.Version, PositionEncoding: schema.PositionEncoding, LookupTransformMaxDepth: 2, LookupPathMaxCount: 512,
		Apps: map[string]schema.App{},
	})
	if err != nil {
		t.Fatal(err)
	}
	cache := &schema.Cache{}
	cache.Replace(retained)
	_, err = NewManager(Config{
		ProjectRoot: project, PythonPath: filepath.Join(project, "missing-python"),
		RestartLimit: 1, BackoffBase: time.Millisecond,
	}, cache, nil)
	if err == nil || !strings.Contains(err.Error(), "inspect Python executable") {
		t.Fatalf("NewManager() error = %v", err)
	}
	if graph, generation := cache.Load(); graph != retained || generation != 1 {
		t.Fatalf("configuration failure replaced cache: graph=%p generation=%d", graph, generation)
	}
}

func TestManagerRefreshesFixtureAfterSaveBurst(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture environment uses a Unix virtualenv")
	}
	root := repositoryRoot(t)
	pythonPath := filepath.Join(root, ".venv-fixture", "bin", "python")
	if _, err := os.Stat(pythonPath); err != nil {
		t.Skipf("fixture Python is unavailable: %v", err)
	}
	project := filepath.Join(t.TempDir(), "project")
	copyFixtureProject(t, filepath.Join(root, "testdata", "sample_django_project"), project)
	cache := &schema.Cache{}
	manager, err := NewManager(Config{
		ProjectRoot: project, PythonPath: pythonPath, SettingsModule: "sample_project.settings",
		ConnectTimeout: 5 * time.Second, RequestTimeout: 10 * time.Second, ShutdownTimeout: time.Second,
	}, cache, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.refreshDelay = 25 * time.Millisecond
	generations := make(chan uint64, 8)
	manager.Start(context.Background(), func(generation uint64, err error) {
		if err != nil {
			t.Errorf("worker event error: %v", err)
			return
		}
		generations <- generation
	})
	if generation := waitForGeneration(t, generations); generation != 1 {
		t.Fatalf("initial generation = %d", generation)
	}
	manager.mu.Lock()
	initialRuntime := manager.activeRuntimeDir
	manager.mu.Unlock()
	modelsPath := filepath.Join(project, "myapp", "models.py")
	models, err := os.ReadFile(modelsPath)
	if err != nil {
		t.Fatal(err)
	}
	models = append(models, []byte("\nclass RefreshProbe(models.Model):\n    marker = models.CharField(max_length=20)\n")...)
	if err := os.WriteFile(modelsPath, models, 0o644); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 8; index++ {
		manager.DidSave(modelsPath)
	}
	if generation := waitForGeneration(t, generations); generation != 2 {
		t.Fatalf("refresh generation = %d", generation)
	}
	manager.mu.Lock()
	refreshedRuntime := manager.activeRuntimeDir
	manager.mu.Unlock()
	if initialRuntime == "" || refreshedRuntime == "" || initialRuntime == refreshedRuntime {
		t.Fatalf("runtime directories = %q then %q", initialRuntime, refreshedRuntime)
	}
	if _, err := os.Stat(initialRuntime); !os.IsNotExist(err) {
		t.Fatalf("retired runtime directory remains: %v", err)
	}
	graph, generation := cache.Load()
	if generation != 2 {
		t.Fatalf("cache generation = %d", generation)
	}
	if !graph.HasModel("myapp.RefreshProbe") {
		t.Fatal("refreshed graph does not contain myapp.RefreshProbe")
	}
	select {
	case generation := <-generations:
		t.Fatalf("save burst produced extra generation %d", generation)
	case <-time.After(100 * time.Millisecond):
	}
	stopContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.Stop(stopContext); err != nil {
		t.Fatal(err)
	}
}

func TestManagerActualWorkerUsesSnapshottedHermeticEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture environment uses a Unix virtualenv")
	}
	root := repositoryRoot(t)
	pythonPath := filepath.Join(root, ".venv-fixture", "bin", "python")
	if _, err := os.Stat(pythonPath); err != nil {
		t.Skipf("fixture Python is unavailable: %v", err)
	}
	project := filepath.Join(t.TempDir(), "project")
	copyFixtureProject(t, filepath.Join(root, "testdata", "sample_django_project"), project)
	settingsPath := filepath.Join(project, "sample_project", "settings.py")
	appendFile(t, settingsPath, `
_expected_environment = {
    "POGO_FILE_VALUE": "file-original",
    "POGO_LITERAL_VALUE": "literal-original",
    "POGO_OVERRIDDEN_VALUE": "literal-wins",
}
for _name, _value in _expected_environment.items():
    if os.environ.get(_name) != _value:
        raise RuntimeError(f"unexpected explicit worker environment: {_name}")
for _name in (
    "POGO_REMOVED_VALUE",
    "AWS_SECRET_ACCESS_KEY",
    "DATABASE_URL",
    "CI",
    "PYTHONPATH",
    "PYTHONHOME",
    "VIRTUAL_ENV",
    "HOME",
    "USERPROFILE",
    "HTTPS_PROXY",
    "SSL_CERT_FILE",
    "LD_LIBRARY_PATH",
):
    if _name in os.environ:
        raise RuntimeError(f"ambient worker environment leaked: {_name}")
if any(_name.startswith("POGO_WORKER_") for _name in os.environ):
    raise RuntimeError("worker transport environment leaked")
if len({os.environ.get("TMPDIR"), os.environ.get("TMP"), os.environ.get("TEMP")}) != 1:
    raise RuntimeError("worker temporary directories differ")
if not os.environ.get("TMPDIR", "").endswith("tmp"):
    raise RuntimeError("worker temporary directory is not private")
`)
	environmentPath := filepath.Join(project, "worker.env")
	if err := os.WriteFile(environmentPath, []byte("DJANGO_SETTINGS_MODULE=sample_project.settings\nPOGO_FILE_VALUE=file-original\nPOGO_OVERRIDDEN_VALUE=file-loses\nPOGO_REMOVED_VALUE=remove-me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	literal := "literal-original"
	override := "literal-wins"
	configuredEnvironment := map[string]*string{
		"POGO_LITERAL_VALUE":    &literal,
		"POGO_OVERRIDDEN_VALUE": &override,
		"POGO_REMOVED_VALUE":    nil,
	}
	for name, value := range map[string]string{
		"AWS_SECRET_ACCESS_KEY": "ambient-aws-secret",
		"DATABASE_URL":          "ambient-database",
		"CI":                    "ambient-ci",
		"PYTHONPATH":            "ambient-python-path",
		"PYTHONHOME":            "ambient-python-home",
		"VIRTUAL_ENV":           "ambient-virtual-environment",
		"HOME":                  "ambient-home",
		"USERPROFILE":           "ambient-profile",
		"HTTPS_PROXY":           "ambient-proxy",
		"SSL_CERT_FILE":         "ambient-certificate",
		"LD_LIBRARY_PATH":       "ambient-native-path",
		"POGO_WORKER_ADDRESS":   "ambient-transport",
	} {
		t.Setenv(name, value)
	}
	cache := &schema.Cache{}
	logger := &captureLogger{}
	manager, err := NewManager(Config{
		ProjectRoot: project, PythonPath: pythonPath,
		EnvironmentFile: environmentPath, Environment: configuredEnvironment,
		ConnectTimeout: 5 * time.Second, RequestTimeout: 10 * time.Second, ShutdownTimeout: time.Second,
	}, cache, logger)
	if err != nil {
		t.Fatal(err)
	}
	manager.refreshDelay = 10 * time.Millisecond

	// Manager construction owns a deep snapshot of both configuration sources.
	literal = "caller-mutated"
	configuredEnvironment["POGO_ADDED_LATE"] = stringPointer("late")
	if err := os.WriteFile(environmentPath, []byte("POGO_FILE_VALUE=file-mutated\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	generations := make(chan uint64, 2)
	failures := make(chan error, 1)
	manager.Start(context.Background(), func(generation uint64, err error) {
		if err != nil {
			failures <- err
			return
		}
		generations <- generation
	})
	if generation := waitForGeneration(t, generations); generation != 1 {
		t.Fatalf("initial generation = %d", generation)
	}
	manager.DidSave(filepath.Join(project, "myapp", "models.py"))
	if generation := waitForGeneration(t, generations); generation != 2 {
		t.Fatalf("refresh generation = %d", generation)
	}
	select {
	case failure := <-failures:
		t.Fatalf("hermetic worker failed: %v; logs=%v", failure, logger.messages())
	default:
	}
	stopContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.Stop(stopContext); err != nil {
		t.Fatalf("Stop() error = %v; logs=%v", err, logger.messages())
	}
	logs := strings.Join(logger.messages(), "\n")
	for _, secret := range []string{"file-original", "literal-original", "literal-wins", "ambient-aws-secret", "ambient-database"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("worker logs exposed environment value %q: %s", secret, logs)
		}
	}
}

func TestManagerBootstrapFailureRetainsLastValidGraph(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture environment uses a Unix virtualenv")
	}
	root := repositoryRoot(t)
	pythonPath := filepath.Join(root, ".venv-fixture", "bin", "python")
	if _, err := os.Stat(pythonPath); err != nil {
		t.Skipf("fixture Python is unavailable: %v", err)
	}
	project := filepath.Join(t.TempDir(), "project")
	copyFixtureProject(t, filepath.Join(root, "testdata", "sample_django_project"), project)
	settingsPath := filepath.Join(project, "sample_project", "settings.py")
	cache := &schema.Cache{}
	logger := &captureLogger{}
	manager, err := NewManager(Config{
		ProjectRoot: project, PythonPath: pythonPath, SettingsModule: "sample_project.settings",
		ConnectTimeout: 5 * time.Second, RequestTimeout: 10 * time.Second, ShutdownTimeout: time.Second,
		RestartLimit: 1, BackoffBase: time.Millisecond,
	}, cache, logger)
	if err != nil {
		t.Fatal(err)
	}
	manager.refreshDelay = 10 * time.Millisecond
	generations := make(chan uint64, 2)
	failures := make(chan error, 2)
	manager.Start(context.Background(), func(generation uint64, err error) {
		if err != nil {
			failures <- err
			return
		}
		generations <- generation
	})
	if generation := waitForGeneration(t, generations); generation != 1 {
		t.Fatalf("initial generation = %d", generation)
	}
	retained, retainedGeneration := cache.Load()
	appendFile(t, settingsPath, `
if "POGO_REQUIRED_SETTING" not in os.environ:
    raise RuntimeError("POGO_REQUIRED_SETTING is required")
`)
	manager.DidSave(settingsPath)
	select {
	case <-failures:
	case <-time.After(10 * time.Second):
		t.Fatalf("bootstrap failure was not reported; logs=%v", logger.messages())
	}
	deadline := time.Now().Add(5 * time.Second)
	for !logger.contains("POGO_REQUIRED_SETTING is required") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if current, generation := cache.Load(); current != retained || generation != retainedGeneration {
		t.Fatalf("failed bootstrap replaced graph=%p generation=%d, want %p generation=%d", current, generation, retained, retainedGeneration)
	}
	if !logger.contains("Traceback") || !logger.contains("POGO_REQUIRED_SETTING is required") {
		t.Fatalf("bootstrap failure details missing from logs: %v", logger.messages())
	}
	stopContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.Stop(stopContext); err != nil && !strings.Contains(err.Error(), "load Django schema") {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestManagerMissingRequiredEnvironmentPublishesNoGraph(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture environment uses a Unix virtualenv")
	}
	root := repositoryRoot(t)
	pythonPath := filepath.Join(root, ".venv-fixture", "bin", "python")
	if _, err := os.Stat(pythonPath); err != nil {
		t.Skipf("fixture Python is unavailable: %v", err)
	}
	project := filepath.Join(t.TempDir(), "project")
	copyFixtureProject(t, filepath.Join(root, "testdata", "sample_django_project"), project)
	appendFile(t, filepath.Join(project, "sample_project", "settings.py"), `
if "POGO_REQUIRED_SETTING" not in os.environ:
    raise RuntimeError("POGO_REQUIRED_SETTING is required")
`)
	if err := os.WriteFile(filepath.Join(project, ".env"), []byte("POGO_REQUIRED_SETTING=must-not-be-auto-loaded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := &schema.Cache{}
	logger := &captureLogger{}
	manager, err := NewManager(Config{
		ProjectRoot: project, PythonPath: pythonPath, SettingsModule: "sample_project.settings",
		ConnectTimeout: 5 * time.Second, RequestTimeout: 10 * time.Second,
		RestartLimit: 1, BackoffBase: time.Millisecond,
	}, cache, logger)
	if err != nil {
		t.Fatal(err)
	}
	failure := make(chan error, 1)
	manager.Start(context.Background(), func(_ uint64, err error) {
		if err != nil {
			failure <- err
		}
	})
	select {
	case <-failure:
	case <-time.After(10 * time.Second):
		t.Fatalf("missing environment failure was not reported; logs=%v", logger.messages())
	}
	if graph, generation := cache.Load(); graph != nil || generation != 0 {
		t.Fatalf("missing environment published graph=%p generation=%d", graph, generation)
	}
	if !logger.contains("POGO_REQUIRED_SETTING is required") {
		t.Fatalf("missing environment detail was not logged: %v", logger.messages())
	}
	stopContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = manager.Stop(stopContext)
}

func waitForGeneration(t *testing.T, generations <-chan uint64) uint64 {
	t.Helper()
	select {
	case generation := <-generations:
		return generation
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for schema generation")
		return 0
	}
}

func copyFixtureProject(t *testing.T, source, destination string) {
	t.Helper()
	if err := filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, content, info.Mode())
	}); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkManagerRefresh(b *testing.B) {
	if runtime.GOOS == "windows" {
		b.Skip("fixture environment uses a Unix virtualenv")
	}
	root := repositoryRoot(b)
	pythonPath := filepath.Join(root, ".venv-fixture", "bin", "python")
	if _, err := os.Stat(pythonPath); err != nil {
		b.Skipf("fixture Python is unavailable: %v", err)
	}
	project := filepath.Join(root, "testdata", "sample_django_project")
	cache := &schema.Cache{}
	manager, err := NewManager(Config{
		ProjectRoot: project, PythonPath: pythonPath, SettingsModule: "sample_project.settings",
		ConnectTimeout: 5 * time.Second, RequestTimeout: 10 * time.Second, ShutdownTimeout: time.Second,
	}, cache, nil)
	if err != nil {
		b.Fatal(err)
	}
	manager.refreshDelay = time.Nanosecond
	generations := make(chan uint64, 8)
	manager.Start(context.Background(), func(generation uint64, err error) {
		if err != nil {
			b.Errorf("worker event error: %v", err)
			return
		}
		generations <- generation
	})
	select {
	case <-generations:
	case <-time.After(15 * time.Second):
		b.Fatal("initial schema load timed out")
	}
	modelsPath := filepath.Join(project, "myapp", "models.py")
	totals := make([]time.Duration, b.N)
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		started := time.Now()
		manager.DidSave(modelsPath)
		select {
		case <-generations:
		case <-time.After(15 * time.Second):
			b.Fatal("schema refresh timed out")
		}
		totals[index] = time.Since(started)
	}
	b.StopTimer()
	sort.Slice(totals, func(left, right int) bool { return totals[left] < totals[right] })
	b.ReportMetric(float64(percentileDuration(totals, 50).Nanoseconds())/1e6, "p50-ms")
	b.ReportMetric(float64(percentileDuration(totals, 95).Nanoseconds())/1e6, "p95-ms")
	b.ReportMetric(float64(percentileDuration(totals, 99).Nanoseconds())/1e6, "p99-ms")
	stopContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.Stop(stopContext); err != nil {
		b.Fatal(err)
	}
}

func percentileDuration(values []time.Duration, percent int) time.Duration {
	if len(values) == 0 {
		return 0
	}
	index := (len(values)*percent + 99) / 100
	if index > 0 {
		index--
	}
	return values[index]
}

func waitForAttempt(t *testing.T, started <-chan int32, want int32) {
	t.Helper()
	select {
	case got := <-started:
		if got != want {
			t.Fatalf("attempt = %d, want %d", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for attempt %d", want)
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
	started := make(chan struct{})
	manager.run = func(ctx context.Context, _ func(uint64, int)) (bool, error) {
		close(started)
		<-ctx.Done()
		return false, errors.New("injected reap failure")
	}
	manager.Start(context.Background(), nil)
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Stop(ctx); err == nil || !strings.Contains(err.Error(), "injected reap failure") {
		t.Fatalf("Stop() error = %v", err)
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
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-crash-secret")
	t.Setenv("DATABASE_URL", "ambient-crash-database")
	t.Setenv("POGO_WORKER_TOKEN_FILE", "ambient-stale-token")
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
		Environment:     map[string]*string{"POGO_TEST_TRACK": stringPointer(trackingPath)},
	}, cache, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.workerScript = []byte(crashingWorkerScript)
	var notifications atomic.Int32
	failures := make(chan struct{}, 4)
	manager.Start(context.Background(), func(_ uint64, err error) {
		if err != nil {
			notifications.Add(1)
			failures <- struct{}{}
		}
	})
	for failure := 1; failure <= 4; failure++ {
		select {
		case <-failures:
		case <-time.After(10 * time.Second):
			t.Fatalf("actual crash restart loop produced %d of 4 outage events", failure-1)
		}
	}
	stopContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = manager.Stop(stopContext)
	graph, generation := cache.Load()
	if graph == nil || generation != 4 || graph.ModelCount() != 0 {
		t.Fatalf("retained crash graph = %#v, generation %d", graph, generation)
	}
	if notifications.Load() != 4 {
		t.Fatalf("crash notifications = %d, want 4 accepted-generation outages", notifications.Load())
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

for leaked_name in ("AWS_SECRET_ACCESS_KEY", "DATABASE_URL"):
    if leaked_name in os.environ:
        raise RuntimeError("ambient environment leaked: " + leaked_name)
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
    "schema_version": 2,
    "position_encoding": "utf-8-bytes",
    "lookup_transform_max_depth": 2,
    "lookup_path_max_count": 512,
    "schema_sources": [],
    "schema_sources_complete": True,
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

type managerTesting interface {
	Helper()
	Fatal(args ...any)
}

func repositoryRoot(t managerTesting) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
}
