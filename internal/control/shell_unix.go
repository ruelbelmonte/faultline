//go:build !windows

package control

import (
	"context"
	"os/exec"
	"syscall"
)

// shellCommand runs a command line through sh in its own process group, so
// that cancelling it stops everything the shell started.
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return cmd
}
