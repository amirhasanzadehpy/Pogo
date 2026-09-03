package python

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/amirhasanzadehpy/Pogo/internal/schema"
)

func TestPersistentSchemaCacheColdWriteAndWarmProvisionalReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent cache is disabled on Windows")
	}
	project := t.TempDir()
	source := filepath.Join(project, "models.py")
	if err := os.WriteFile(source, []byte("class Cached: pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pythonPath := testExecutable(t)
	cacheDirectory := filepath.Join(t.TempDir(), "cache")
	config := Config{ProjectRoot: project, PythonPath: pythonPath, CacheDirectory: cacheDirectory}

	coldSchema := testSchemaJSON(t, source, "cached")
	coldManager, err := NewManager(config, &schema.Cache{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if graph, generation := coldManager.cache.Load(); graph != nil || generation != 0 {
		t.Fatalf("cold cache = %p generation=%d", graph, generation)
	}
	coldManager.activeEpoch = 0
	coldManager.releasedEpoch = 0
	coldManager.publishedAuthority = 1
	coldManager.persistGraph(context.Background(), coldSchema)
	persistent := managerCache(t, coldManager)
	if _, err := os.Stat(persistent.path()); err != nil {
		t.Fatalf("persisted cache: %v", err)
	}

	warmCache := &schema.Cache{}
	warmManager, err := NewManager(config, warmCache, nil)
	if err != nil {
		t.Fatal(err)
	}
	authoritative, err := schema.Build(testSnapshot(source, "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	releaseRuntime := make(chan struct{})
	warmManager.run = func(ctx context.Context, loaded func(uint64, int)) (bool, error) {
		close(started)
		<-releaseRuntime
		generation, accepted := warmManager.publishGraph(ctx, authoritative)
		if !accepted {
			return false, errors.New("authoritative graph rejected")
		}
		loaded(generation, authoritative.ModelCount())
		<-ctx.Done()
		return true, nil
	}
	warmManager.Start(context.Background(), nil)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("authoritative worker did not publish")
	}
	deadline := time.Now().Add(time.Second)
	for {
		provisional, generation := warmCache.Load()
		warmManager.mu.Lock()
		authority := warmManager.authority
		warmManager.mu.Unlock()
		if provisional != nil && generation == 1 && authority == authorityProvisional {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("provisional graph was not published before delayed runtime")
		}
		time.Sleep(time.Millisecond)
	}
	close(releaseRuntime)
	deadline = time.Now().Add(time.Second)
	for {
		graph, generation := warmCache.Load()
		warmManager.mu.Lock()
		authority := warmManager.authority
		warmManager.mu.Unlock()
		if graph == authoritative && generation == 2 && authority == authorityRuntime {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("authoritative cache = %p generation=%d authority=%d", graph, generation, authority)
		}
		time.Sleep(time.Millisecond)
	}
	stopContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := warmManager.Stop(stopContext); err != nil {
		t.Fatal(err)
	}
}

func TestPersistentSchemaCacheRejectsUntrustedRecords(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent cache is disabled on Windows")
	}
	project := t.TempDir()
	source := filepath.Join(project, "models.py")
	if err := os.WriteFile(source, []byte("pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{ProjectRoot: project, PythonPath: testExecutable(t), CacheDirectory: filepath.Join(t.TempDir(), "cache")}, &schema.Cache{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	persistent := managerCache(t, manager)
	valid, err := persistent.marshal(context.Background(), testSchemaJSON(t, source, "cached"))
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(valid, &record); err != nil {
		t.Fatal(err)
	}
	mutate := func(field, value string) []byte {
		copyRecord := make(map[string]json.RawMessage, len(record))
		for key, raw := range record {
			copyRecord[key] = raw
		}
		copyRecord[field] = json.RawMessage(value)
		payload, marshalErr := json.Marshal(copyRecord)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return payload
	}
	tests := map[string][]byte{
		"truncated":      valid[:len(valid)/2],
		"wrong checksum": mutate("checksum", `"0000000000000000000000000000000000000000000000000000000000000000"`),
		"wrong identity": mutate("identity", `"wrong"`),
		"unknown":        append(valid[:len(valid)-1], []byte(`,"unknown":true}`)...),
		"duplicate":      append(valid[:len(valid)-1], []byte(`,"checksum":"duplicate"}`)...),
		"missing":        []byte(`{"format_version":1}`),
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := persistent.decode(context.Background(), payload); err == nil {
				t.Fatal("decode error = nil")
			}
		})
	}
	if err := os.MkdirAll(persistent.directory, 0o700); err != nil {
		t.Fatal(err)
	}
	oversized, err := os.Create(persistent.path())
	if err != nil {
		t.Fatal(err)
	}
	if err := oversized.Truncate(MaxFrameSize + schemaCacheFileOverhead + 1); err != nil {
		t.Fatal(err)
	}
	_ = oversized.Close()
	if _, err := persistent.load(context.Background()); err == nil {
		t.Fatal("oversized load error = nil")
	}
}

func TestPersistentSchemaCacheInvalidatesSourcesAndConfigurationWithoutSecrets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent cache is disabled on Windows")
	}
	project := t.TempDir()
	source := filepath.Join(project, "models.py")
	if err := os.WriteFile(source, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := "database-password-never-store-plaintext"
	pythonPath := testExecutable(t)
	pythonInfo, err := os.Stat(pythonPath)
	if err != nil {
		t.Fatal(err)
	}
	base := Config{ProjectRoot: project, PythonPath: pythonPath, CacheDirectory: t.TempDir(), SettingsModule: "first.settings"}
	first := openCacheConfig(t, newPersistentSchemaCacheConfig(base, pythonInfo, []workerEnvironmentEntry{{name: "SECRET", value: secret}}, nil, "/bin", []byte("worker")))
	variants := []*persistentSchemaCache{
		openCacheConfig(t, newPersistentSchemaCacheConfig(Config{ProjectRoot: t.TempDir(), PythonPath: pythonPath, CacheDirectory: base.CacheDirectory, SettingsModule: base.SettingsModule}, pythonInfo, []workerEnvironmentEntry{{name: "SECRET", value: secret}}, nil, "/bin", []byte("worker"))),
		openCacheConfig(t, newPersistentSchemaCacheConfig(Config{ProjectRoot: project, PythonPath: pythonPath, CacheDirectory: base.CacheDirectory, SettingsModule: "second.settings"}, pythonInfo, []workerEnvironmentEntry{{name: "SECRET", value: secret}}, nil, "/bin", []byte("worker"))),
		openCacheConfig(t, newPersistentSchemaCacheConfig(base, pythonInfo, []workerEnvironmentEntry{{name: "SECRET", value: secret + "-changed"}}, nil, "/bin", []byte("worker"))),
		openCacheConfig(t, newPersistentSchemaCacheConfig(base, pythonInfo, []workerEnvironmentEntry{{name: "SECRET", value: secret}}, nil, "/other", []byte("worker"))),
		openCacheConfig(t, newPersistentSchemaCacheConfig(Config{ProjectRoot: project, PythonPath: filepath.Join(project, "other-python"), CacheDirectory: base.CacheDirectory, SettingsModule: base.SettingsModule}, pythonInfo, []workerEnvironmentEntry{{name: "SECRET", value: secret}}, nil, "/bin", []byte("worker"))),
	}
	for index, variant := range variants {
		if variant.path() == first.path() {
			t.Fatalf("configuration variant %d reused identity", index)
		}
	}
	managePath := filepath.Join(project, "manage.py")
	if err := os.WriteFile(managePath, []byte("DJANGO_SETTINGS_MODULE = 'selected.settings'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Explicit settings bypass automatic discovery inputs.
	if strings.Contains(first.path(), secret) {
		t.Fatalf("cache filename exposed secret: %q", first.path())
	}
	logger := &captureLogger{}
	value := secret
	if _, err := NewManager(Config{ProjectRoot: project, PythonPath: pythonPath, CacheDirectory: filepath.Join(t.TempDir(), "cache"), Environment: map[string]*string{"SECRET": &value}}, &schema.Cache{}, logger); err != nil {
		t.Fatal(err)
	}
	if logger.contains(secret) {
		t.Fatalf("cache logs exposed secret: %v", logger.messages())
	}
	payload, err := first.marshal(context.Background(), testSchemaJSON(t, source, "cached"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("new content with another size\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := first.decode(context.Background(), payload); err == nil || !strings.Contains(err.Error(), "source identity") {
		t.Fatalf("changed source decode error = %v", err)
	}
	unchangedTime, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	sameSizePayload, err := first.marshal(context.Background(), testSchemaJSON(t, source, "cached"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("NEW content with another size\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(source, unchangedTime.ModTime(), unchangedTime.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := first.decode(context.Background(), sameSizePayload); err == nil || !strings.Contains(err.Error(), "source identity") {
		t.Fatalf("same-size source decode error = %v", err)
	}
}

func TestPersistentSchemaCachePermissionsAndDisabledUnavailableBehavior(t *testing.T) {
	project := t.TempDir()
	pythonPath := testExecutable(t)
	disabled, err := NewManager(Config{ProjectRoot: project, PythonPath: pythonPath}, &schema.Cache{}, nil)
	if err != nil || disabled.cacheConfig != nil {
		t.Fatalf("disabled manager cache=%v error=%v", disabled.cacheConfig, err)
	}
	source := filepath.Join(project, "models.py")
	if err := os.WriteFile(source, []byte("pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{ProjectRoot: project, PythonPath: pythonPath, CacheDirectory: filepath.Join(t.TempDir(), "cache")}, &schema.Cache{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.publishedAuthority = 1
	manager.persistGraph(context.Background(), testSchemaJSON(t, source, "cached"))
	if runtime.GOOS != "windows" {
		persistent := managerCache(t, manager)
		directoryInfo, _ := os.Stat(persistent.directory)
		fileInfo, _ := os.Stat(persistent.path())
		if directoryInfo.Mode().Perm() != 0o700 || fileInfo.Mode().Perm() != 0o600 {
			t.Fatalf("cache permissions directory=%o file=%o", directoryInfo.Mode().Perm(), fileInfo.Mode().Perm())
		}
	}
	unavailable := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(unavailable, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	logger := &captureLogger{}
	failed, err := NewManager(Config{ProjectRoot: project, PythonPath: pythonPath, CacheDirectory: unavailable}, &schema.Cache{}, logger)
	if err != nil {
		t.Fatalf("unavailable cache prevented manager creation: %v", err)
	}
	failed.publishedAuthority = 1
	failed.persistGraph(context.Background(), testSchemaJSON(t, source, "cached"))
	if !logger.contains("provisional schema cache") {
		t.Fatalf("cache failure warning missing: %v", logger.messages())
	}
}

func TestManagerFailureRetainsProvisionalAndUsesConservativeSavePolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent cache is disabled on Windows")
	}
	project := t.TempDir()
	source := filepath.Join(project, "installed", "models.py")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := Config{ProjectRoot: project, PythonPath: testExecutable(t), CacheDirectory: filepath.Join(t.TempDir(), "cache"), RestartLimit: 1, BackoffBase: time.Millisecond}
	seed, err := NewManager(config, &schema.Cache{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	seed.publishedAuthority = 1
	seed.persistGraph(context.Background(), testSchemaJSON(t, source, "cached"))
	cache := &schema.Cache{}
	manager, err := NewManager(config, cache, nil)
	if err != nil {
		t.Fatal(err)
	}
	outsideKnownRoots := filepath.Join(project, "unreported", "module.py")
	manager.run = func(context.Context, func(uint64, int)) (bool, error) { return false, errors.New("worker failed") }
	outage := make(chan struct{}, 1)
	manager.Start(context.Background(), func(_ uint64, err error) {
		if err != nil {
			select {
			case outage <- struct{}{}:
			default:
			}
		}
	})
	deadline := time.Now().Add(time.Second)
	for {
		manager.mu.Lock()
		authority := manager.authority
		manager.mu.Unlock()
		if authority == authorityProvisional {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("provisional cache was not published")
		}
		time.Sleep(time.Millisecond)
	}
	if !manager.schemaAffectingPath(outsideKnownRoots) {
		t.Fatalf("provisional schema did not conservatively include %q", outsideKnownRoots)
	}
	select {
	case <-outage:
	case <-time.After(time.Second):
		t.Fatal("worker outage was not reported")
	}
	graph, generation := cache.Load()
	manager.mu.Lock()
	authority := manager.authority
	manager.mu.Unlock()
	if graph == nil || generation != 1 || authority != authorityProvisional {
		t.Fatalf("failed worker cache=%p generation=%d authority=%d", graph, generation, authority)
	}
	stopContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = manager.Stop(stopContext)
}

func TestManagerRuntimeAuthorityRejectsLateProvisional(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent cache is disabled on Windows")
	}
	project := t.TempDir()
	source := filepath.Join(project, "models.py")
	if err := os.WriteFile(source, []byte("pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := Config{ProjectRoot: project, PythonPath: testExecutable(t), CacheDirectory: filepath.Join(t.TempDir(), "cache")}
	seed, err := NewManager(config, &schema.Cache{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	seed.publishedAuthority = 1
	seed.persistGraph(context.Background(), testSchemaJSON(t, source, "cached"))

	cache := &schema.Cache{}
	manager, err := NewManager(config, cache, nil)
	if err != nil {
		t.Fatal(err)
	}
	cacheBlocked := make(chan struct{})
	releaseCache := make(chan struct{})
	manager.cacheConfig.beforeOpen = func(ctx context.Context) error {
		close(cacheBlocked)
		select {
		case <-releaseCache:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	runtimeGraph, err := schema.Build(testSnapshot(source, "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	published := make(chan struct{})
	manager.run = func(ctx context.Context, loaded func(uint64, int)) (bool, error) {
		generation, accepted := manager.publishGraph(ctx, runtimeGraph)
		if !accepted {
			return false, errors.New("runtime graph rejected")
		}
		loaded(generation, 0)
		close(published)
		<-ctx.Done()
		return true, nil
	}
	manager.Start(context.Background(), nil)
	<-cacheBlocked
	<-published
	close(releaseCache)
	time.Sleep(25 * time.Millisecond)
	graph, generation := cache.Load()
	if graph != runtimeGraph || generation != 1 {
		t.Fatalf("late provisional replaced runtime graph=%p generation=%d", graph, generation)
	}
	stopManager(t, manager)
}

func TestManagerExistingGraphRejectsProvisionalAndCancellationStopsLoad(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent cache is disabled on Windows")
	}
	project := t.TempDir()
	source := filepath.Join(project, "models.py")
	if err := os.WriteFile(source, []byte("pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := Config{ProjectRoot: project, PythonPath: testExecutable(t), CacheDirectory: filepath.Join(t.TempDir(), "cache")}
	seed, err := NewManager(config, &schema.Cache{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	seed.publishedAuthority = 1
	seed.persistGraph(context.Background(), testSchemaJSON(t, source, "cached"))
	existing, err := schema.Build(testSnapshot(source, "existing"))
	if err != nil {
		t.Fatal(err)
	}
	cache := &schema.Cache{}
	cache.Replace(existing)
	manager, err := NewManager(config, cache, nil)
	if err != nil {
		t.Fatal(err)
	}
	loadStarted := make(chan struct{})
	loadCanceled := make(chan struct{})
	manager.cacheConfig.beforeOpen = func(ctx context.Context) error {
		close(loadStarted)
		<-ctx.Done()
		close(loadCanceled)
		return ctx.Err()
	}
	manager.run = func(ctx context.Context, _ func(uint64, int)) (bool, error) {
		<-ctx.Done()
		return false, nil
	}
	manager.Start(context.Background(), nil)
	<-loadStarted
	stopManager(t, manager)
	select {
	case <-loadCanceled:
	case <-time.After(time.Second):
		t.Fatal("provisional load did not observe cancellation")
	}
	graph, generation := cache.Load()
	if graph != existing || generation != 1 {
		t.Fatalf("existing graph replaced graph=%p generation=%d", graph, generation)
	}
}

func TestPersistentSchemaCacheRejectsIncompleteManifestAndNonprivateDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent cache is disabled on Windows")
	}
	project := t.TempDir()
	source := filepath.Join(project, "models.py")
	if err := os.WriteFile(source, []byte("pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheDirectory := filepath.Join(t.TempDir(), "cache")
	manager, err := NewManager(Config{ProjectRoot: project, PythonPath: testExecutable(t), CacheDirectory: cacheDirectory}, &schema.Cache{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	persistent := managerCache(t, manager)
	incomplete := testSnapshot(source, "cached")
	incomplete.SchemaSourcesComplete = false
	payload, err := json.Marshal(incomplete)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := persistent.marshal(context.Background(), payload); err == nil {
		t.Fatal("incomplete manifest marshal error = nil")
	}
	checksum := sha256.Sum256(payload)
	record, err := marshalStrict(schemaCacheRecord{
		FormatVersion: schemaCacheFormatVersion,
		Identity:      hex.EncodeToString(persistent.identity[:]),
		Checksum:      hex.EncodeToString(checksum[:]),
		Schema:        payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := persistent.decode(context.Background(), record); err == nil {
		t.Fatal("incomplete manifest load error = nil")
	}
	if err := os.Mkdir(cacheDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := persistent.writeTemp(context.Background(), testSchemaJSON(t, source, "cached")); err == nil {
		t.Fatal("nonprivate directory write error = nil")
	}
}

func TestSettingsSelectionIdentityHashesContentAndBoundsDiscovery(t *testing.T) {
	project := t.TempDir()
	managePath := filepath.Join(project, "manage.py")
	firstContent := []byte("DJANGO_SETTINGS_MODULE='first.settings'\n")
	if err := os.WriteFile(managePath, firstContent, 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := settingsSelectionIdentity(context.Background(), project, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(managePath)
	if err != nil {
		t.Fatal(err)
	}
	secondContent := []byte("DJANGO_SETTINGS_MODULE='other.settings'\n")
	if len(secondContent) != len(firstContent) {
		t.Fatal("test settings contents must have equal length")
	}
	if err := os.WriteFile(managePath, secondContent, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(managePath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	second, err := settingsSelectionIdentity(context.Background(), project, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("same-stat settings content reused identity")
	}

	oversized := filepath.Join(project, "app", "settings.py")
	if err := os.Mkdir(filepath.Dir(oversized), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(oversized)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxSettingsSelectionBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := settingsSelectionIdentity(context.Background(), project, "", nil); err == nil {
		t.Fatal("oversized settings identity error = nil")
	}
}

func TestNewManagerDefersPersistentFilesystemWorkUntilStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent cache is disabled on Windows")
	}
	project := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "cache-target")
	if err := os.WriteFile(cachePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	logger := &captureLogger{}
	manager, err := NewManager(Config{ProjectRoot: project, PythonPath: testExecutable(t), CacheDirectory: cachePath}, &schema.Cache{}, logger)
	if err != nil {
		t.Fatalf("NewManager performed persistent filesystem work: %v", err)
	}
	if logger.contains("provisional schema cache") {
		t.Fatalf("NewManager emitted cache I/O warning: %v", logger.messages())
	}
	manager.run = func(ctx context.Context, _ func(uint64, int)) (bool, error) {
		<-ctx.Done()
		return false, nil
	}
	manager.Start(context.Background(), nil)
	deadline := time.Now().Add(time.Second)
	for !logger.contains("provisional schema cache") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !logger.contains("provisional schema cache") {
		t.Fatalf("Start did not perform deferred cache work: %v", logger.messages())
	}
	stopManager(t, manager)
}

func testExecutable(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func testSchemaJSON(t *testing.T, source, appLabel string) []byte {
	t.Helper()
	payload, err := json.Marshal(testSnapshot(source, appLabel))
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func testSnapshot(source, appLabel string) schema.Snapshot {
	return schema.Snapshot{
		SchemaVersion: schema.Version, PositionEncoding: schema.PositionEncoding,
		LookupTransformMaxDepth: 2, LookupPathMaxCount: 512, SchemaSources: []string{source},
		SchemaSourcesComplete: true,
		QuerySetMethodDefs:    []schema.Method{},
		Apps:                  map[string]schema.App{appLabel: {Label: appLabel, ImportName: appLabel, RootPath: filepath.Dir(source), Models: map[string]schema.Model{}}},
	}
}

func managerCache(t *testing.T, manager *Manager) *persistentSchemaCache {
	t.Helper()
	return openCacheConfig(t, manager.cacheConfig)
}

func openCacheConfig(t *testing.T, configuration *persistentSchemaCacheConfig) *persistentSchemaCache {
	t.Helper()
	if configuration == nil {
		t.Fatal("persistent cache configuration is disabled")
	}
	cache, err := configuration.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cache
}

func stopManager(t *testing.T, manager *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPersistentSchemaCacheIdentityIsDigestOnly(t *testing.T) {
	digest := sha256.Sum256([]byte("identity"))
	cache := &persistentSchemaCache{directory: t.TempDir(), identity: digest}
	if filepath.Base(cache.path()) != hex.EncodeToString(digest[:])+".schema" {
		t.Fatalf("cache filename = %q", cache.path())
	}
}
