//go:build windows

package control

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
)

// shellCommand runs a command line through cmd.exe. The line is passed raw:
// Go's argument quoting escapes quotes in a way cmd.exe does not understand.
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd.exe /S /C "` + command + `"`}
	cmd.Cancel = func() error {
		// Kills the whole tree; cmd.exe does not pass termination on to what it started.
		return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	}
	return cmd
}
