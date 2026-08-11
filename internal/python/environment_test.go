package python

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/amirhasanzadehpy/Pogo/internal/schema"
)

func TestParseWorkerEnvironmentAcceptedSyntax(t *testing.T) {
	payload := []byte("\xef\xbb\xbf# comment\r\n" +
		"export ALPHA=one\r\n" +
		"EMPTY=\r\n" +
		"EMPTY_COMMENT= # ignored\r\n" +
		"SPACED = trimmed value   \r\n" +
		"URL=https://example.test/path#fragment\r\n" +
		"COMMENT=value   # ignored\r\n" +
		"SINGLE='literal \\n${VALUE} $()'\r\n" +
		"DOUBLE=\"slash\\\\ quote\\\" newline\\n return\\r tab\\t\"\r\n" +
		"MULTILINE=\"first\r\nsecond\" # comment\r\n" +
		"FINAL=no-newline")
	entries, err := parseWorkerEnvironment("fixture.env", payload)
	if err != nil {
		t.Fatalf("parseWorkerEnvironment() error = %v", err)
	}
	got := environmentEntryMap(entries)
	want := map[string]string{
		"ALPHA":         "one",
		"EMPTY":         "",
		"EMPTY_COMMENT": "",
		"SPACED":        "trimmed value",
		"URL":           "https://example.test/path#fragment",
		"COMMENT":       "value",
		"SINGLE":        "literal \\n${VALUE} $()",
		"DOUBLE":        "slash\\ quote\" newline\n return\r tab\t",
		"MULTILINE":     "first\nsecond",
		"FINAL":         "no-newline",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("parsed environment = %#v, want %#v", got, want)
	}
}

func TestParseWorkerEnvironmentRejectsMalformedInputWithoutValues(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		message string
	}{
		{name: "invalid UTF-8", payload: []byte{0xff}, message: "invalid UTF-8"},
		{name: "NUL", payload: []byte("KEY=value\x00tail"), message: "contains NUL"},
		{name: "invalid key", payload: []byte("1KEY=value"), message: "invalid variable name"},
		{name: "missing equals", payload: []byte("KEY value"), message: "expected '='"},
		{name: "duplicate", payload: []byte("KEY=one\nKEY=two\n"), message: "duplicate variable"},
		{name: "invalid escape", payload: []byte("KEY=\"secret\\q\""), message: "invalid escape"},
		{name: "unterminated escape", payload: []byte("KEY=\"secret\\"), message: "unterminated escape"},
		{name: "unterminated quote", payload: []byte("KEY='secret"), message: "unterminated quoted value"},
		{name: "trailing text", payload: []byte("KEY='secret' trailing"), message: "trailing text"},
		{name: "bare carriage return", payload: []byte("KEY=value\rNEXT=other"), message: "bare carriage return"},
		{name: "reserved transport", payload: []byte("pogo_worker_token=secret"), message: "reserved for worker transport"},
		{name: "reserved Python", payload: []byte("PYTHONUTF8=secret"), message: "reserved by Pogo"},
		{name: "oversized value", payload: []byte("KEY=" + strings.Repeat("s", maxWorkerEnvironmentValueSize+1)), message: "value exceeds"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseWorkerEnvironment("private.env", test.payload)
			if err == nil || !strings.Contains(err.Error(), test.message) || !strings.Contains(err.Error(), "private.env") {
				t.Fatalf("parse error = %v, want path and %q", err, test.message)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("parse error exposed value: %v", err)
			}
		})
	}
}

func TestParseWorkerEnvironmentBoundsCountAndPortableUnits(t *testing.T) {
	var payload strings.Builder
	for index := 0; index <= maxWorkerEnvironmentCount; index++ {
		fmt.Fprintf(&payload, "KEY_%03d=value\n", index)
	}
	if _, err := parseWorkerEnvironment("count.env", []byte(payload.String())); err == nil || !strings.Contains(err.Error(), "256 variables") {
		t.Fatalf("count limit error = %v", err)
	}

	entries := []workerEnvironmentEntry{{name: "VALUE", value: strings.Repeat("😀", maxWorkerEnvironmentValueSize/2+1)}}
	if err := validateWorkerEnvironment(entries); err == nil || !strings.Contains(err.Error(), "value exceeds") {
		t.Fatalf("UTF-16 value limit error = %v", err)
	}

	entries = make([]workerEnvironmentEntry, 0, 2)
	entries = append(entries,
		workerEnvironmentEntry{name: "FIRST", value: strings.Repeat("a", 15_000)},
		workerEnvironmentEntry{name: "SECOND", value: strings.Repeat("b", 15_000)},
	)
	if err := validateWorkerEnvironment(entries); err == nil || !strings.Contains(err.Error(), "total units") {
		t.Fatalf("total limit error = %v", err)
	}
}

func TestLoadWorkerEnvironmentMergesOverridesAndWarnsAboutPermissions(t *testing.T) {
	project := t.TempDir()
	path := filepath.Join(project, "worker.env")
	if err := os.WriteFile(path, []byte("FILE_ONLY=file\nOVERRIDE=file\nREMOVE=file\nPATH=/explicit/bin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	override := "literal"
	empty := ""
	logger := &captureLogger{}
	entries, err := loadWorkerEnvironment(Config{
		EnvironmentFile: path,
		Environment: map[string]*string{
			"OVERRIDE": &override,
			"REMOVE":   nil,
			"EMPTY":    &empty,
			"ABSENT":   nil,
		},
	}, logger)
	if err != nil {
		t.Fatal(err)
	}
	got := environmentEntryMap(entries)
	if got["FILE_ONLY"] != "file" || got["OVERRIDE"] != "literal" || got["EMPTY"] != "" || got["PATH"] != "/explicit/bin" {
		t.Fatalf("merged environment = %#v", got)
	}
	if _, exists := got["REMOVE"]; exists {
		t.Fatalf("removed variable remains: %#v", got)
	}
	if runtime.GOOS != "windows" && !logger.contains("group- or world-readable") {
		t.Fatalf("permission warning log = %v", logger.messages())
	}
	if !logger.contains("inherited=PATH") || !logger.contains("FILE_ONLY") || logger.contains("literal") {
		t.Fatalf("environment audit log = %v", logger.messages())
	}
}

func TestReadWorkerEnvironmentFileRequiresBoundedRegularFile(t *testing.T) {
	project := t.TempDir()
	if _, err := readWorkerEnvironmentFile(project, nil); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory environment file error = %v", err)
	}
	missing := filepath.Join(project, "missing.env")
	if _, err := readWorkerEnvironmentFile(missing, nil); err == nil || !strings.Contains(err.Error(), "open worker environment file") {
		t.Fatalf("missing environment file error = %v", err)
	}
	oversized := filepath.Join(project, "oversized.env")
	if err := os.WriteFile(oversized, []byte(strings.Repeat("x", maxWorkerEnvironmentFileSize+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readWorkerEnvironmentFile(oversized, nil); err == nil || !strings.Contains(err.Error(), "exceeds 262144 bytes") {
		t.Fatalf("oversized environment file error = %v", err)
	}
}

func TestLoadWorkerEnvironmentRejectsReservedAndCaseCollisions(t *testing.T) {
	value := "not-logged"
	for _, name := range []string{"PYTHONDONTWRITEBYTECODE", "pythonutf8", "TMPDIR", "SystemRoot", "POGO_WORKER_ADDRESS"} {
		t.Run(name, func(t *testing.T) {
			_, err := loadWorkerEnvironment(Config{Environment: map[string]*string{name: &value}}, nil)
			if err == nil || !strings.Contains(err.Error(), "reserved") || strings.Contains(err.Error(), value) {
				t.Fatalf("reserved error = %v", err)
			}
		})
	}

	upper := "one"
	lower := "two"
	_, err := loadWorkerEnvironment(Config{Environment: map[string]*string{"VALUE": &upper, "value": &lower}}, nil)
	if runtime.GOOS == "windows" {
		if err == nil || !strings.Contains(err.Error(), "same variable") {
			t.Fatalf("Windows collision error = %v", err)
		}
	} else if err != nil {
		t.Fatalf("POSIX case-distinct variables error = %v", err)
	}
}

func TestNewManagerSnapshotsEnvironmentAndValidatesSettings(t *testing.T) {
	pythonPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	path := filepath.Join(project, "worker.env")
	if err := os.WriteFile(path, []byte("DJANGO_SETTINGS_MODULE=file.settings\nFILE_VALUE=original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	literal := "original"
	config := Config{
		ProjectRoot:     project,
		PythonPath:      pythonPath,
		EnvironmentFile: "worker.env",
		Environment:     map[string]*string{"LITERAL_VALUE": &literal},
	}
	manager, err := NewManager(config, &schema.Cache{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if manager.config.SettingsModule != "" || !filepath.IsAbs(manager.config.PythonPath) || manager.config.EnvironmentFile != path {
		t.Fatalf("normalized manager config = %#v", manager.config)
	}
	if value, present := workerEnvironmentValue(manager.workerEnvironment, "DJANGO_SETTINGS_MODULE"); !present || value != "file.settings" {
		t.Fatalf("snapshotted environment settings = %q, %v", value, present)
	}
	literal = "mutated"
	config.Environment["ADDED"] = &literal
	if err := os.WriteFile(path, []byte("FILE_VALUE=mutated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := environmentEntryMap(manager.workerEnvironment)
	if got["FILE_VALUE"] != "original" || got["LITERAL_VALUE"] != "original" {
		t.Fatalf("manager environment snapshot mutated: %#v", got)
	}
	if _, exists := got["ADDED"]; exists {
		t.Fatalf("caller map mutation entered snapshot: %#v", got)
	}

	_, err = NewManager(Config{
		ProjectRoot:    project,
		PythonPath:     pythonPath,
		SettingsModule: "explicit.settings",
		Environment:    map[string]*string{"DJANGO_SETTINGS_MODULE": stringPointer("environment.settings")},
	}, &schema.Cache{}, nil)
	if err == nil || !strings.Contains(err.Error(), "settings module conflict") || strings.Contains(err.Error(), "explicit.settings") || strings.Contains(err.Error(), "environment.settings") {
		t.Fatalf("settings conflict error = %v", err)
	}
}

func TestNewManagerRejectsEnvironmentThatCannotFitRuntimeValues(t *testing.T) {
	pythonPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	configured := make(map[string]*string, maxWorkerEnvironmentCount)
	for index := 0; index < maxWorkerEnvironmentCount; index++ {
		configured[fmt.Sprintf("VALUE_%03d", index)] = stringPointer("value")
	}
	_, err = NewManager(Config{ProjectRoot: t.TempDir(), PythonPath: pythonPath, Environment: configured}, &schema.Cache{}, nil)
	if err == nil || !strings.Contains(err.Error(), "validate worker environment configuration") || !strings.Contains(err.Error(), "256 variables") {
		t.Fatalf("runtime capacity error = %v", err)
	}
}

func TestBuildWorkerEnvironmentIsHermeticAndDeterministic(t *testing.T) {
	coordinatorPath := strings.Join([]string{"/usr/local/bin", "/usr/bin", "/bin"}, string(os.PathListSeparator))
	manager := &Manager{
		config:          Config{PythonPath: filepath.Join(t.TempDir(), "venv", "python")},
		coordinatorPath: coordinatorPath,
		workerEnvironment: []workerEnvironmentEntry{
			{name: "ZED", value: "last"},
			{name: "EXPLICIT", value: "present"},
		},
	}
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-secret")
	t.Setenv("DATABASE_URL", "ambient-database")
	first, err := manager.buildWorkerEnvironment("/private/tmp", "unix", "/private/socket", "/private/token")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.buildWorkerEnvironment("/private/tmp", "unix", "/private/socket", "/private/token")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(first, "\n") != strings.Join(second, "\n") || !sort.StringsAreSorted(normalizedEnvironmentStrings(first)) {
		t.Fatalf("environment is not deterministic and sorted: %v", first)
	}
	joined := strings.Join(first, "\n")
	for _, absent := range []string{"AWS_SECRET_ACCESS_KEY", "DATABASE_URL", "ambient-secret", "ambient-database"} {
		if strings.Contains(joined, absent) {
			t.Fatalf("ambient value %q entered worker environment: %v", absent, first)
		}
	}
	for _, present := range []string{
		"EXPLICIT=present", "PYTHONDONTWRITEBYTECODE=1", "PYTHONUNBUFFERED=1", "PYTHONUTF8=1",
		"TMPDIR=/private/tmp", "TMP=/private/tmp", "TEMP=/private/tmp", "POGO_WORKER_NETWORK=unix",
	} {
		if !containsEnvironmentEntry(first, present) {
			t.Fatalf("worker environment missing %q: %v", present, first)
		}
	}
	wantPath := "PATH=" + filepath.Dir(manager.config.PythonPath) + string(os.PathListSeparator) + coordinatorPath
	if !containsEnvironmentEntry(first, wantPath) {
		t.Fatalf("worker environment PATH = %v, want %q", first, wantPath)
	}
}

func TestBuildWorkerEnvironmentExplicitPathOverridesCoordinator(t *testing.T) {
	manager := &Manager{
		config:            Config{PythonPath: filepath.Join(t.TempDir(), "venv", "python")},
		coordinatorPath:   "/ambient/bin",
		workerEnvironment: []workerEnvironmentEntry{{name: "PATH", value: "/explicit/bin"}},
	}
	environment, err := manager.buildWorkerEnvironment("/private/tmp", "unix", "/private/socket", "/private/token")
	if err != nil {
		t.Fatal(err)
	}
	if !containsEnvironmentEntry(environment, "PATH=/explicit/bin") || strings.Contains(strings.Join(environment, "\n"), "/ambient/bin") {
		t.Fatalf("explicit PATH did not replace coordinator PATH: %v", environment)
	}
}

func FuzzParseWorkerEnvironment(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte("KEY=value\n"),
		[]byte("export EMPTY=\r\nQUOTED=\"line\\nvalue\""),
		[]byte("BROKEN='value"),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		entries, err := parseWorkerEnvironment("fuzz.env", payload)
		if err != nil {
			return
		}
		if err := validateWorkerEnvironment(entries); err != nil {
			t.Fatalf("accepted invalid environment: %v", err)
		}
		seen := make(map[string]struct{}, len(entries))
		for _, entry := range entries {
			key := normalizeEnvironmentKey(entry.name)
			if _, duplicate := seen[key]; duplicate {
				t.Fatalf("accepted duplicate key %q", entry.name)
			}
			seen[key] = struct{}{}
		}
	})
}

func BenchmarkParseWorkerEnvironment(b *testing.B) {
	payload := []byte("DATABASE_URL=postgres://localhost/pogo\nSECRET_KEY='development-only'\nAPP_MODE=development\nPATH=/opt/project/bin\n")
	b.ReportAllocs()
	for range b.N {
		if _, err := parseWorkerEnvironment("benchmark.env", payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBuildWorkerEnvironment(b *testing.B) {
	entries := make([]workerEnvironmentEntry, 200)
	for index := range entries {
		entries[index] = workerEnvironmentEntry{name: fmt.Sprintf("VARIABLE_%03d", index), value: strings.Repeat("v", 64)}
	}
	manager := &Manager{config: Config{PythonPath: "/project/.venv/bin/python"}, workerEnvironment: entries}
	b.ReportAllocs()
	for range b.N {
		if _, err := manager.buildWorkerEnvironment("/runtime/tmp", "unix", "/runtime/socket", "/runtime/token"); err != nil {
			b.Fatal(err)
		}
	}
}

type captureLogger struct {
	mu   sync.Mutex
	logs []string
}

func (logger *captureLogger) Infof(format string, values ...any) {
	logger.append(format, values...)
}

func (logger *captureLogger) Warningf(format string, values ...any) {
	logger.append(format, values...)
}

func (logger *captureLogger) Errorf(format string, values ...any) {
	logger.append(format, values...)
}

func (logger *captureLogger) append(format string, values ...any) {
	logger.mu.Lock()
	logger.logs = append(logger.logs, fmt.Sprintf(format, values...))
	logger.mu.Unlock()
}

func (logger *captureLogger) contains(value string) bool {
	return strings.Contains(strings.Join(logger.messages(), "\n"), value)
}

func (logger *captureLogger) messages() []string {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	return append([]string(nil), logger.logs...)
}

func environmentEntryMap(entries []workerEnvironmentEntry) map[string]string {
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		values[entry.name] = entry.value
	}
	return values
}

func stringPointer(value string) *string {
	return &value
}

func containsEnvironmentEntry(environment []string, want string) bool {
	for _, entry := range environment {
		if entry == want {
			return true
		}
	}
	return false
}

func normalizedEnvironmentStrings(environment []string) []string {
	normalized := make([]string, len(environment))
	for index, entry := range environment {
		name, value, _ := strings.Cut(entry, "=")
		normalized[index] = normalizeEnvironmentKey(name) + "=" + value
	}
	return normalized
}
