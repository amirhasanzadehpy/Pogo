//go:build !windows

package python

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcess(process *os.Process) error {
	err := syscall.Kill(-process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func normalizeEnvironmentKey(name string) string {
	return name
}

func workerEnvironmentFileIsBroadlyReadable(mode os.FileMode) bool {
	return mode.Perm()&0o044 != 0
}

func validatePythonExecutable(path string, info os.FileInfo) error {
	if info.Mode().Perm()&0o111 == 0 {
		return errors.New("is not executable")
	}
	if err := syscall.Access(path, 1); err != nil {
		return fmt.Errorf("is not executable by the current user: %w", err)
	}
	return nil
}

func platformWorkerEnvironment() ([]workerEnvironmentEntry, error) {
	return nil, nil
}
