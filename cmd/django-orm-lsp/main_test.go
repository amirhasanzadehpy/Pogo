package main

import (
	"path/filepath"
	"testing"

	protocol "github.com/tliron/glsp/protocol_3_16"
)

func TestResolveWorkerConfigPrecedenceAndWorkspaceFallback(t *testing.T) {
	t.Setenv("VIRTUAL_ENV", "")
	t.Setenv("DJANGO_SETTINGS_MODULE", "environment.settings")
	project := t.TempDir()
	rootURI := protocol.DocumentUri("file://" + filepath.ToSlash(project))
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
