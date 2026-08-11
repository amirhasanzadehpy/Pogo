//go:build windows

package python

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func configureProcess(_ *exec.Cmd) {}

func killProcess(process *os.Process) error {
	return process.Kill()
}

func normalizeEnvironmentKey(name string) string {
	return strings.ToLower(name)
}

func workerEnvironmentFileIsBroadlyReadable(os.FileMode) bool {
	return false
}

func validatePythonExecutable(string, os.FileInfo) error {
	return nil
}

var getWindowsDirectory = windows.GetWindowsDirectory

func platformWorkerEnvironment() ([]workerEnvironmentEntry, error) {
	root, err := getWindowsDirectory()
	if err != nil {
		return nil, fmt.Errorf("get Windows directory: %w", err)
	}
	if root == "" || strings.IndexByte(root, 0) >= 0 || !filepath.IsAbs(root) || filepath.Clean(root) == "." {
		return nil, errors.New("get Windows directory: API returned an invalid path")
	}
	return []workerEnvironmentEntry{{name: "SystemRoot", value: filepath.Clean(root)}}, nil
}
