// Command perritite reads, writes and waits on Slack threads, built for
// Claude Code sessions that collaborate in Slack. See README.md.
package main

import (
	"os"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/chadrem/perritite/internal/cli"
)

// version is stamped by the release build (-ldflags "-X main.version=...").
// A `go install ...@vX.Y.Z` build reports its module version instead.
var version = "dev"

var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

func main() {
	// Only a clean release tag (from `go install ...@v0.1.0`): local builds
	// carry VCS pseudo-versions like v0.0.0-2026...-abc123+dirty.
	if bi, ok := debug.ReadBuildInfo(); ok && version == "dev" && releaseTag.MatchString(bi.Main.Version) {
		version = strings.TrimPrefix(bi.Main.Version, "v")
	}
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
