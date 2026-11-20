// Command slack-thread reads, writes and waits on Slack threads, built for
// Claude Code sessions that collaborate in Slack. See README.md.
package main

import (
	"os"

	"github.com/chadrem/slack-thread/internal/cli"
)

// version is stamped by the release build (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	os.Exit(cli.Main(os.Args[1:], cli.Env{
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Getenv:  os.Getenv,
		Getwd:   os.Getwd,
		PID:     os.Getpid(),
		Version: version,
	}))
}
