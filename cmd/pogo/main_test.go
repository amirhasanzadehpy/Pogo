package main

import (
	"bytes"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	protocol "github.com/tliron/glsp/protocol_3_16"
)

func TestPogoCLIIdentity(t *testing.T) {
	var stderr bytes.Buffer
	if exitCode := run([]string{"-version"}, &stderr); exitCode != 0 || stderr.String() != "pogo 0.1.0\n" {
		t.Fatalf("pogo -version = code %d, output %q", exitCode, stderr.String())
	}
	stderr.Reset()
	if exitCode := run([]string{"-help"}, &stderr); exitCode != 0 || !strings.Contains(stderr.String(), "Usage: pogo [options]") || !strings.Contains(stderr.String(), "Pogo LSP 3.16 server") {
		t.Fatalf("pogo -help = code %d, output %q", exitCode, stderr.String())
	}
}

func TestResolveWorkerConfigPrecedenceAndWorkspaceFallback(t *testing.T) {
	t.Setenv("VIRTUAL_ENV", "")
	t.Setenv("DJANGO_SETTINGS_MODULE", "environment.settings")
	project := t.TempDir()
	rootURI := testFileURI(project)
	params := &protocol.InitializeParams{
		RootURI: &rootURI,
		InitializationOptions: map[string]any{
			"djangoOrm": map[string]any{
				"pythonPath":     "/options/python",
				"settingsModule": "options.settings",
			},
		},
	}
	config, enabled, err := resolveWorkerConfig("", "", "", params)
	if err != nil || !enabled {
		t.Fatalf("resolveWorkerConfig() = %#v, %v, %v", config, enabled, err)
	}
	if config.ProjectRoot != project || config.PythonPath != "/options/python" || config.SettingsModule != "options.settings" {
		t.Fatalf("resolved config = %#v", config)
	}

	config, enabled, err = resolveWorkerConfig(project, "/cli/python", "cli.settings", params)
	if err != nil || !enabled {
		t.Fatalf("CLI resolveWorkerConfig() = %#v, %v, %v", config, enabled, err)
	}
	if config.PythonPath != "/cli/python" || config.SettingsModule != "cli.settings" {
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
	config, enabled, err := resolveWorkerConfig("", "", "", &protocol.InitializeParams{})
	if err != nil || enabled || config.ProjectRoot != "" {
		t.Fatalf("empty workspace config = %#v, %v, %v", config, enabled, err)
	}
	params := &protocol.InitializeParams{
		WorkspaceFolders: []protocol.WorkspaceFolder{
			{URI: protocol.DocumentUri("file:///first"), Name: "first"},
			{URI: protocol.DocumentUri("file:///second"), Name: "second"},
		},
	}
	if _, _, err := resolveWorkerConfig("", "", "", params); err == nil {
		t.Fatal("ambiguous workspace error = nil")
	}
}
