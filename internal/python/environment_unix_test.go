//go:build !windows

package python

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amirhasanzadehpy/Pogo/internal/schema"
)

func TestNewManagerRequiresExecutablePythonAndPreservesSymlinkPath(t *testing.T) {
	project := t.TempDir()
	realPython := filepath.Join(project, "python-real")
	if err := os.WriteFile(realPython, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkPython := filepath.Join(project, ".venv", "bin", "python")
	if err := os.MkdirAll(filepath.Dir(symlinkPython), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPython, symlinkPython); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{ProjectRoot: project, PythonPath: filepath.Join(".venv", "bin", "python")}, &schema.Cache{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if manager.config.PythonPath != symlinkPython {
		t.Fatalf("Python path = %q, want symlink spelling %q", manager.config.PythonPath, symlinkPython)
	}

	nonExecutable := filepath.Join(project, "not-executable")
	if err := os.WriteFile(nonExecutable, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = NewManager(Config{ProjectRoot: project, PythonPath: nonExecutable}, &schema.Cache{}, nil)
	if err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("non-executable Python error = %v", err)
	}

	otherOnly := filepath.Join(project, "other-only-executable")
	if err := os.WriteFile(otherOnly, nil, 0o001); err != nil {
		t.Fatal(err)
	}
	_, err = NewManager(Config{ProjectRoot: project, PythonPath: otherOnly}, &schema.Cache{}, nil)
	if os.Geteuid() != 0 && (err == nil || !strings.Contains(err.Error(), "current user")) {
		t.Fatalf("other-only Python error = %v", err)
	}
}
