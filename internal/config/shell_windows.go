//go:build windows

package config

import (
	"context"
	"os/exec"
	"syscall"
)

// shellCommand runs command through cmd.exe. The command line is passed
// verbatim: Go's default argument escaping is the C runtime's, which
// cmd.exe does not understand, and would mangle quoted paths such as
// "C:\Program Files\...".
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd.exe /d /s /c "` + command + `"`}
	return cmd
}
