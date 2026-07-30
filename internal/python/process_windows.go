//go:build windows

package python

import (
	"os"
	"os/exec"
)

func configureProcess(_ *exec.Cmd) {}

func killProcess(process *os.Process) error {
	return process.Kill()
}
