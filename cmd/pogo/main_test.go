package main

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	protocol "github.com/tliron/glsp/protocol_3_16"
)

func TestPogoCLIIdentity(t *testing.T) {
	var stderr bytes.Buffer
	if exitCode := run([]string{"-version"}, &stderr); exitCode != 0 || stderr.String() != "pogo 0.2.7\n" {
		t.Fatalf("pogo -version = code %d, output %q", exitCode, stderr.String())
	}
	stderr.Reset()
	if exitCode := run([]string{"-help"}, &stderr); exitCode != 0 || !strings.Contains(stderr.String(), "Usage: pogo [options]") || !strings.Contains(stderr.String(), "Pogo LSP 3.16 server") || !strings.Contains(stderr.String(), "-worker-env-file") {
		t.Fatalf("pogo -help = code %d, output %q", exitCode, stderr.String())
	}
}

func TestPogoCacheDirectoryUsesUserCache(t *testing.T) {
	if runtime.GOOS == "windows" {
		if directory := pogoCacheDirectory(); directory != "" {
			t.Fatalf("pogoCacheDirectory() = %q, want disabled on Windows", directory)
		}
		return
	}
	if runtime.GOOS != "darwin" {
		t.Setenv("XDG_CACHE_HOME", t.TempDir())
	}
	root, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if directory := pogoCacheDirectory(); directory != filepath.Join(root, "pogo") {
		t.Fatalf("pogoCacheDirectory() = %q", directory)
	}
}

func TestResolveWorkerConfigPrecedenceAndWorkspaceFallback(t *testing.T) {
	t.Setenv("VIRTUAL_ENV", filepath.Join(t.TempDir(), "ambient-venv"))
	t.Setenv("DJANGO_SETTINGS_MODULE", "environment.settings")
	project := t.TempDir()
	rootURI := testFileURI(project)
	optionsPython := filepath.Join(t.TempDir(), "options-python")
	environmentValue := "option-value"
	params := &protocol.InitializeParams{
		RootURI: &rootURI,
		InitializationOptions: map[string]any{
			"djangoOrm": map[string]any{
				"pythonPath":      optionsPython,
				"settingsModule":  "options.settings",
				"environmentFile": "options.env",
				"environment": map[string]any{
					"OPTION_VALUE":  environmentValue,
					"REMOVED_VALUE": nil,
				},
			},
		},
	}
	config, enabled, err := resolveWorkerConfig("", "", "", "", params)
	if err != nil || !enabled {
		t.Fatalf("resolveWorkerConfig() = %#v, %v, %v", config, enabled, err)
	}
	if config.ProjectRoot != project || config.PythonPath != optionsPython || config.SettingsModule != "options.settings" || config.EnvironmentFile != filepath.Join(project, "options.env") {
		t.Fatalf("resolved config = %#v", config)
	}
	if value := config.Environment["OPTION_VALUE"]; value == nil || *value != environmentValue {
		t.Fatalf("environment OPTION_VALUE = %v", value)
	}
	if value, ok := config.Environment["REMOVED_VALUE"]; !ok || value != nil {
		t.Fatalf("environment REMOVED_VALUE = %v, %v", value, ok)
	}

	config, enabled, err = resolveWorkerConfig(project, "cli/python", "cli.settings", "cli.env", params)
	if err != nil || !enabled {
		t.Fatalf("CLI resolveWorkerConfig() = %#v, %v, %v", config, enabled, err)
	}
	if config.PythonPath != filepath.Join(project, "cli/python") || config.SettingsModule != "cli.settings" || config.EnvironmentFile != filepath.Join(project, "cli.env") {
		t.Fatalf("CLI precedence config = %#v", config)
	}
}

func testFileURI(path string) protocol.DocumentUri {
	path = filepath.ToSlash(path)
	if runtime.GOOS == "windows" {
		path = "/" + path
	}
	return protocol.DocumentUri((&url.URL{Scheme: "file", Path: path}).String())
}

func TestResolveWorkerConfigDisablesEmptyWorkspaceAndRejectsAmbiguity(t *testing.T) {
	config, enabled, err := resolveWorkerConfig("", "", "", "", &protocol.InitializeParams{})
	if err != nil || enabled || config.ProjectRoot != "" {
		t.Fatalf("empty workspace config = %#v, %v, %v", config, enabled, err)
	}
	params := &protocol.InitializeParams{
		WorkspaceFolders: []protocol.WorkspaceFolder{
			{URI: protocol.DocumentUri("file:///first"), Name: "first"},
			{URI: protocol.DocumentUri("file:///second"), Name: "second"},
		},
	}
	if _, _, err := resolveWorkerConfig("", "", "", "", params); err == nil {
		t.Fatal("ambiguous workspace error = nil")
	}
}

func TestResolveWorkerConfigProjectVirtualEnvironmentFallback(t *testing.T) {
	project := t.TempDir()
	pythonPath := createProjectPython(t, project)
	t.Setenv("VIRTUAL_ENV", filepath.Join(t.TempDir(), "ambient-venv"))
	t.Setenv("DJANGO_SETTINGS_MODULE", "ambient.settings")

	config, enabled, err := resolveWorkerConfig(project, "", "", "", nil)
	if err != nil || !enabled {
		t.Fatalf("resolveWorkerConfig() = %#v, %v, %v", config, enabled, err)
	}
	if config.PythonPath != pythonPath {
		t.Fatalf("PythonPath = %q, want %q", config.PythonPath, pythonPath)
	}
	if config.SettingsModule != "" {
		t.Fatalf("SettingsModule = %q, want no ambient setting", config.SettingsModule)
	}
}

func TestResolveWorkerConfigRequiresConfiguredPython(t *testing.T) {
	project := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("VIRTUAL_ENV", filepath.Join(t.TempDir(), "ambient-venv"))

	_, enabled, err := resolveWorkerConfig(project, "", "", "", nil)
	if err == nil || enabled {
		t.Fatalf("resolveWorkerConfig() = enabled %v, error %v", enabled, err)
	}
	candidate := virtualEnvironmentPython(filepath.Join(project, ".venv"))
	if message := err.Error(); !strings.Contains(message, "set -python or djangoOrm.pythonPath") || !strings.Contains(message, strconv.Quote(candidate)) {
		t.Fatalf("missing Python error = %q", message)
	}
}

func TestResolveWorkerConfigLeavesEnvironmentSettingsForManager(t *testing.T) {
	project := t.TempDir()
	params := &protocol.InitializeParams{InitializationOptions: map[string]any{
		"djangoOrm": map[string]any{
			"projectRoot": project,
			"pythonPath":  "venv/python",
			"environment": map[string]any{"DJANGO_SETTINGS_MODULE": "environment.settings"},
		},
	}}

	config, enabled, err := resolveWorkerConfig("", "", "", "", params)
	if err != nil || !enabled {
		t.Fatalf("resolveWorkerConfig() = %#v, %v, %v", config, enabled, err)
	}
	if config.SettingsModule != "" {
		t.Fatalf("SettingsModule = %q, want manager to resolve environment setting", config.SettingsModule)
	}
	if value := config.Environment["DJANGO_SETTINGS_MODULE"]; value == nil || *value != "environment.settings" {
		t.Fatalf("environment settings = %v", value)
	}

	config, enabled, err = resolveWorkerConfig("", "", "options.settings", "", params)
	if err != nil || !enabled || config.SettingsModule != "options.settings" {
		t.Fatalf("explicit settings config = %#v, %v, %v", config, enabled, err)
	}
	if value := config.Environment["DJANGO_SETTINGS_MODULE"]; value == nil || *value != "environment.settings" {
		t.Fatalf("explicit settings discarded environment value = %v", value)
	}
}

func TestResolveWorkerConfigRejectsMalformedInitializationOptions(t *testing.T) {
	project := t.TempDir()
	tests := []struct {
		name      string
		djangoORM any
	}{
		{name: "django ORM", djangoORM: "invalid"},
		{name: "environment file", djangoORM: map[string]any{"projectRoot": project, "pythonPath": "python", "environmentFile": true}},
		{name: "environment", djangoORM: map[string]any{"projectRoot": project, "pythonPath": "python", "environment": []string{"VALUE"}}},
		{name: "environment value", djangoORM: map[string]any{"projectRoot": project, "pythonPath": "python", "environment": map[string]any{"VALUE": 1}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			params := &protocol.InitializeParams{InitializationOptions: map[string]any{"djangoOrm": test.djangoORM}}
			if _, _, err := resolveWorkerConfig("", "", "", "", params); err == nil || !strings.Contains(err.Error(), "decode initialization options") {
				t.Fatalf("malformed initialization error = %v", err)
			}
		})
	}
}

func TestResolveWorkerConfigMultiRootWithExplicitProject(t *testing.T) {
	project := t.TempDir()
	params := &protocol.InitializeParams{
		WorkspaceFolders: []protocol.WorkspaceFolder{
			{URI: protocol.DocumentUri("file:///first"), Name: "first"},
			{URI: protocol.DocumentUri("file:///second"), Name: "second"},
		},
		InitializationOptions: map[string]any{"djangoOrm": map[string]any{
			"projectRoot": project,
			"pythonPath":  "venv/python",
		}},
	}
	config, enabled, err := resolveWorkerConfig("", "", "", "", params)
	if err != nil || !enabled || config.ProjectRoot != project {
		t.Fatalf("multi-root explicit config = %#v, %v, %v", config, enabled, err)
	}
}

func createProjectPython(t *testing.T, project string) string {
	t.Helper()
	pythonPath := virtualEnvironmentPython(filepath.Join(project, ".venv"))
	if err := os.MkdirAll(filepath.Dir(pythonPath), 0o755); err != nil {
		t.Fatalf("create project virtual environment: %v", err)
	}
	if err := os.WriteFile(pythonPath, nil, 0o755); err != nil {
		t.Fatalf("create project Python: %v", err)
	}
	return pythonPath
}
