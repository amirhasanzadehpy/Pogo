package python

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"

	"github.com/amirhasanzadehpy/Pogo/internal/schema"
)

const schemaCacheFormatVersion = 1
const schemaExtractionFormatVersion = 1
const schemaCacheFileOverhead = 4096
const maxSchemaSourceIdentityBytes = MaxFrameSize
const maxSettingsSelectionBytes = 1024 * 1024
const maxProjectRootEntries = 4096

type schemaCacheRecord struct {
	FormatVersion  int             `json:"format_version"`
	Identity       string          `json:"identity"`
	SourceIdentity string          `json:"source_identity"`
	Checksum       string          `json:"checksum"`
	Schema         json.RawMessage `json:"schema"`
}

type persistentSchemaCache struct {
	directory     string
	identity      [sha256.Size]byte
	directoryInfo os.FileInfo
}

type persistentSchemaCacheConfig struct {
	config              Config
	pythonSize          int64
	pythonMode          os.FileMode
	pythonModTime       int64
	workerEnvironment   []workerEnvironmentEntry
	platformEnvironment []workerEnvironmentEntry
	coordinatorPath     string
	workerDigest        [sha256.Size]byte
	beforeOpen          func(context.Context) error
}

type schemaCacheTemporary struct {
	path          string
	directoryInfo os.FileInfo
}

func newPersistentSchemaCacheConfig(config Config, pythonInfo os.FileInfo, workerEnvironment, platformEnvironment []workerEnvironmentEntry, coordinatorPath string, workerScript []byte) *persistentSchemaCacheConfig {
	if config.CacheDirectory == "" {
		return nil
	}
	return &persistentSchemaCacheConfig{
		config: config, pythonSize: pythonInfo.Size(), pythonMode: pythonInfo.Mode(), pythonModTime: pythonInfo.ModTime().UnixNano(),
		workerEnvironment: workerEnvironment, platformEnvironment: platformEnvironment,
		coordinatorPath: coordinatorPath, workerDigest: sha256.Sum256(workerScript),
	}
}

func (configuration *persistentSchemaCacheConfig) open(ctx context.Context) (*persistentSchemaCache, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if configuration.beforeOpen != nil {
		if err := configuration.beforeOpen(ctx); err != nil {
			return nil, err
		}
	}
	config := configuration.config
	hash := sha256.New()
	writeCacheIdentityField(hash, "format", strconv.Itoa(schemaCacheFormatVersion))
	writeCacheIdentityField(hash, "extraction-format", strconv.Itoa(schemaExtractionFormatVersion))
	writeCacheIdentityField(hash, "protocol", strconv.Itoa(ProtocolVersion))
	writeCacheIdentityField(hash, "schema", strconv.Itoa(schema.Version))
	writeCacheIdentityField(hash, "position", schema.PositionEncoding)
	writeCacheIdentityField(hash, "frame-limit", strconv.Itoa(MaxFrameSize))
	writeCacheIdentityField(hash, "project", canonicalPath(config.ProjectRoot))
	writeCacheIdentityField(hash, "python", canonicalPath(config.PythonPath))
	writeCacheIdentityField(hash, "file-size", strconv.FormatInt(configuration.pythonSize, 10))
	writeCacheIdentityField(hash, "file-mode", strconv.FormatUint(uint64(configuration.pythonMode), 10))
	writeCacheIdentityField(hash, "file-modtime", strconv.FormatInt(configuration.pythonModTime, 10))
	writeCacheIdentityField(hash, "settings", config.SettingsModule)
	if settings, present := workerEnvironmentValue(configuration.workerEnvironment, "DJANGO_SETTINGS_MODULE"); present {
		writeCacheIdentityField(hash, "effective-settings", settings)
	} else {
		writeCacheIdentityField(hash, "effective-settings", config.SettingsModule)
	}
	settingsIdentity, err := settingsSelectionIdentity(ctx, config.ProjectRoot, config.SettingsModule, configuration.workerEnvironment)
	if err != nil {
		return nil, err
	}
	writeCacheIdentityField(hash, "settings-selection", settingsIdentity)
	for _, entry := range configuration.workerEnvironment {
		writeCacheIdentityField(hash, "environment-name", entry.name)
		writeCacheIdentityField(hash, "environment-value", entry.value)
	}
	for _, entry := range configuration.platformEnvironment {
		writeCacheIdentityField(hash, "platform-environment-name", entry.name)
		writeCacheIdentityField(hash, "platform-environment-value", entry.value)
	}
	writeCacheIdentityField(hash, "coordinator-path", configuration.coordinatorPath)
	writeCacheIdentityField(hash, "worker", string(configuration.workerDigest[:]))
	cache := &persistentSchemaCache{directory: config.CacheDirectory}
	copy(cache.identity[:], hash.Sum(nil))
	return cache, nil
}

func settingsSelectionIdentity(ctx context.Context, projectRoot, explicitSettings string, environment []workerEnvironmentEntry) (string, error) {
	if explicitSettings != "" {
		return "explicit", nil
	}
	if _, present := workerEnvironmentValue(environment, "DJANGO_SETTINGS_MODULE"); present {
		return "environment", nil
	}
	hash := sha256.New()
	paths := []string{filepath.Join(projectRoot, "manage.py")}
	entries, err := os.ReadDir(projectRoot)
	if err != nil {
		return "", err
	}
	if len(entries) > maxProjectRootEntries {
		return "", errors.New("project root exceeds settings identity entry limit")
	}
	for _, entry := range entries {
		if entry.IsDir() {
			paths = append(paths, filepath.Join(projectRoot, entry.Name(), "settings.py"))
		}
	}
	sort.Strings(paths)
	remaining := int64(maxSettingsSelectionBytes)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > remaining {
			_ = file.Close()
			return "", errors.New("settings identity is incomplete or exceeds size limit")
		}
		writeCacheIdentityField(hash, "selection-source", canonicalPath(path))
		writeCacheIdentityField(hash, "selection-size", strconv.FormatInt(info.Size(), 10))
		if err := hashFileContext(ctx, hash, file, info.Size()); err != nil {
			_ = file.Close()
			return "", err
		}
		_ = file.Close()
		remaining -= info.Size()
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeCacheIdentityField(writer io.Writer, name, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(name)))
	_, _ = writer.Write(size[:])
	_, _ = io.WriteString(writer, name)
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = writer.Write(size[:])
	_, _ = io.WriteString(writer, value)
}

func writeCacheFileIdentity(writer io.Writer, info os.FileInfo) {
	writeCacheIdentityField(writer, "file-size", strconv.FormatInt(info.Size(), 10))
	writeCacheIdentityField(writer, "file-mode", strconv.FormatUint(uint64(info.Mode()), 10))
	writeCacheIdentityField(writer, "file-modtime", strconv.FormatInt(info.ModTime().UnixNano(), 10))
}

func (cache *persistentSchemaCache) path() string {
	return filepath.Join(cache.directory, hex.EncodeToString(cache.identity[:])+".schema")
}

func (cache *persistentSchemaCache) load(ctx context.Context) (*schema.Graph, error) {
	directoryInfo, err := cache.validateDirectory()
	if err != nil {
		return nil, err
	}
	pathInfo, err := os.Lstat(cache.path())
	if err != nil {
		return nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return nil, errors.New("schema cache entry is not a regular file")
	}
	if runtime.GOOS != "windows" && pathInfo.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("schema cache entry is not private")
	}
	file, err := os.Open(cache.path())
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(pathInfo, info) || !info.Mode().IsRegular() {
		return nil, errors.New("schema cache entry is not a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("schema cache entry is not private")
	}
	if info.Size() <= 0 || info.Size() > MaxFrameSize+schemaCacheFileOverhead {
		return nil, errors.New("schema cache entry has invalid size")
	}
	payload, err := readAllContext(ctx, file, MaxFrameSize+schemaCacheFileOverhead+1)
	if err != nil {
		return nil, err
	}
	if len(payload) > MaxFrameSize+schemaCacheFileOverhead {
		return nil, errors.New("schema cache entry is oversized")
	}
	if current, currentErr := os.Stat(cache.directory); currentErr != nil || !os.SameFile(directoryInfo, current) {
		return nil, errors.New("schema cache directory changed while reading")
	}
	return cache.decode(ctx, payload)
}

func (cache *persistentSchemaCache) decode(ctx context.Context, payload []byte) (*schema.Graph, error) {
	var record schemaCacheRecord
	if err := decodeStrict(payload, &record); err != nil {
		return nil, fmt.Errorf("decode schema cache: %w", err)
	}
	if record.FormatVersion != schemaCacheFormatVersion || record.Identity != hex.EncodeToString(cache.identity[:]) {
		return nil, errors.New("schema cache identity mismatch")
	}
	if len(record.Schema) == 0 || len(record.Schema) > MaxFrameSize {
		return nil, errors.New("schema cache payload has invalid size")
	}
	checksum := sha256.Sum256(record.Schema)
	if record.Checksum != hex.EncodeToString(checksum[:]) {
		return nil, errors.New("schema cache checksum mismatch")
	}
	if err := schema.ValidateWire(record.Schema); err != nil {
		return nil, fmt.Errorf("validate cached schema wire: %w", err)
	}
	var snapshot schema.Snapshot
	if err := decodeStrict(record.Schema, &snapshot); err != nil {
		return nil, fmt.Errorf("decode cached schema: %w", err)
	}
	if !snapshot.SchemaSourcesComplete {
		return nil, errors.New("cached schema source manifest is incomplete")
	}
	sourceIdentity, err := schemaSourceIdentity(ctx, snapshot.SchemaSources)
	if err != nil || record.SourceIdentity != sourceIdentity {
		return nil, errors.New("schema cache source identity mismatch")
	}
	graph, err := schema.Build(snapshot)
	if err != nil {
		return nil, fmt.Errorf("build cached schema: %w", err)
	}
	return graph, nil
}

func (cache *persistentSchemaCache) marshal(ctx context.Context, snapshotJSON []byte) ([]byte, error) {
	if len(snapshotJSON) == 0 || len(snapshotJSON) > MaxFrameSize {
		return nil, errors.New("schema cache payload has invalid size")
	}
	var snapshot schema.Snapshot
	if err := decodeStrict(snapshotJSON, &snapshot); err != nil {
		return nil, err
	}
	if !snapshot.SchemaSourcesComplete {
		return nil, errors.New("schema source manifest is incomplete")
	}
	sourceIdentity, err := schemaSourceIdentity(ctx, snapshot.SchemaSources)
	if err != nil {
		return nil, err
	}
	checksum := sha256.Sum256(snapshotJSON)
	record := schemaCacheRecord{
		FormatVersion: schemaCacheFormatVersion, Identity: hex.EncodeToString(cache.identity[:]),
		SourceIdentity: sourceIdentity, Checksum: hex.EncodeToString(checksum[:]), Schema: snapshotJSON,
	}
	return marshalStrict(record)
}

func schemaSourceIdentity(ctx context.Context, paths []string) (string, error) {
	paths = append([]string(nil), paths...)
	sort.Strings(paths)
	hash := sha256.New()
	remaining := int64(maxSchemaSourceIdentityBytes)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		before, err := os.Lstat(path)
		if err != nil || before.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("schema source is unavailable or not regular")
		}
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return "", err
		}
		if !os.SameFile(before, info) || !info.Mode().IsRegular() {
			_ = file.Close()
			return "", fmt.Errorf("schema source %q is not regular", path)
		}
		if info.Size() < 0 || info.Size() > remaining {
			_ = file.Close()
			return "", errors.New("schema source identity exceeds maximum size")
		}
		writeCacheIdentityField(hash, "source", canonicalPath(path))
		writeCacheIdentityField(hash, "source-size", strconv.FormatInt(info.Size(), 10))
		if err := hashFileContext(ctx, hash, file, info.Size()); err != nil {
			_ = file.Close()
			return "", err
		}
		if err := file.Close(); err != nil {
			return "", err
		}
		after, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) || after.Mode() != info.Mode() {
			return "", fmt.Errorf("schema source %q changed while hashing", path)
		}
		remaining -= info.Size()
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (cache *persistentSchemaCache) writeTemp(ctx context.Context, snapshotJSON []byte) (schemaCacheTemporary, error) {
	payload, err := cache.marshal(ctx, snapshotJSON)
	if err != nil {
		return schemaCacheTemporary{}, err
	}
	directoryInfo, err := cache.ensureDirectory()
	if err != nil {
		return schemaCacheTemporary{}, err
	}
	temporary, err := os.CreateTemp(cache.directory, ".pogo-schema-*")
	if err != nil {
		return schemaCacheTemporary{}, err
	}
	temporaryPath := temporary.Name()
	remove := true
	defer func() {
		_ = temporary.Close()
		if remove {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return schemaCacheTemporary{}, err
	}
	if current, err := os.Lstat(cache.directory); err != nil || !os.SameFile(directoryInfo, current) {
		return schemaCacheTemporary{}, errors.New("schema cache directory changed before write")
	}
	if _, err := temporary.Write(payload); err != nil {
		return schemaCacheTemporary{}, err
	}
	if err := temporary.Sync(); err != nil {
		return schemaCacheTemporary{}, err
	}
	if err := temporary.Close(); err != nil {
		return schemaCacheTemporary{}, err
	}
	remove = false
	return schemaCacheTemporary{path: temporaryPath, directoryInfo: directoryInfo}, nil
}

func (cache *persistentSchemaCache) ensureDirectory() (os.FileInfo, error) {
	if err := os.Mkdir(cache.directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	return cache.validateDirectory()
}

func (cache *persistentSchemaCache) validateDirectory() (os.FileInfo, error) {
	info, err := os.Lstat(cache.directory)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("schema cache directory is not a regular directory")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		return nil, errors.New("schema cache directory is not private")
	}
	return info, nil
}

func (cache *persistentSchemaCache) install(temporary schemaCacheTemporary) error {
	current, err := os.Lstat(cache.directory)
	if err != nil || !os.SameFile(temporary.directoryInfo, current) {
		return errors.New("schema cache directory changed before install")
	}
	destination := cache.path()
	if err := os.Rename(temporary.path, destination); err != nil {
		// Windows does not replace an existing destination atomically. Retaining
		// the existing valid entry is safer than a remove-then-rename gap.
		if _, statErr := os.Stat(destination); statErr == nil {
			return err
		}
		return err
	}
	cache.directoryInfo = temporary.directoryInfo
	return nil
}

func (cache *persistentSchemaCache) syncDirectory() error {
	current, err := os.Lstat(cache.directory)
	if err != nil || cache.directoryInfo != nil && !os.SameFile(cache.directoryInfo, current) {
		return errors.New("schema cache directory changed before sync")
	}
	directory, err := os.Open(cache.directory)
	if err == nil {
		err = directory.Sync()
		_ = directory.Close()
	}
	return err
}

func readAllContext(ctx context.Context, reader io.Reader, maximum int64) ([]byte, error) {
	payload := make([]byte, 0, min(maximum, 64*1024))
	buffer := make([]byte, 64*1024)
	for int64(len(payload)) < maximum {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		limit := len(buffer)
		if remaining := maximum - int64(len(payload)); remaining < int64(limit) {
			limit = int(remaining)
		}
		count, err := reader.Read(buffer[:limit])
		payload = append(payload, buffer[:count]...)
		if errors.Is(err, io.EOF) {
			return payload, nil
		}
		if err != nil {
			return nil, err
		}
	}
	return payload, nil
}

func hashFileContext(ctx context.Context, hash io.Writer, file *os.File, size int64) error {
	buffer := make([]byte, 64*1024)
	remaining := size
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		limit := int64(len(buffer))
		if remaining < limit {
			limit = remaining
		}
		count, err := file.Read(buffer[:limit])
		if count > 0 {
			_, _ = hash.Write(buffer[:count])
			remaining -= int64(count)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
