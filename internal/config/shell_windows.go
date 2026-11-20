//go:build windows

package config

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// shellCommand runs command through cmd.exe. The command line is passed
// verbatim: Go's default argument escaping is the C runtime's, which
// cmd.exe does not understand, and would mangle quoted paths such as
// "C:\Program Files\...".
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	shell := "cmd.exe"
	if root := os.Getenv("SystemRoot"); root != "" {
		shell = filepath.Join(root, "System32", "cmd.exe") // never a cmd.exe found on PATH
	}
	cmd := exec.CommandContext(ctx, shell)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd.exe /d /s /c "` + command + `"`}
	return cmd
}
