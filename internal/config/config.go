// Package config resolves slack-thread's settings: a JSON file in the
// user's config directory, overridden by environment variables. There is
// deliberately no per-project config file: a repo you clone must never be
// able to choose the command that produces your Slack token.
package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// File is the on-disk config (all fields optional).
type File struct {
	// Exactly one way to get the token; the first non-empty wins in this
	// order after the SLACK_THREAD_TOKEN env var.
	Token        string `json:"token,omitempty"`
	TokenFile    string `json:"token_file,omitempty"`
	TokenCommand string `json:"token_command,omitempty"`
	// OwnerUserIDs are the Slack users whose in-thread words carry the
	// session owner's authority (the watch tags them role=owner).
	OwnerUserIDs []string `json:"owner_user_ids,omitempty"`
	DownloadDir  string   `json:"download_dir,omitempty"`
	StateDir     string   `json:"state_dir,omitempty"`
	// AsUser sends as_user=true on posts (default true; needed for legacy
	// and user tokens to post as their own identity).
	AsUser *bool `json:"as_user,omitempty"`
}

// Config is the effective configuration.
type Config struct {
	Path        string // config file path (may not exist)
	FileExists  bool
	File        File
	Owners      []string
	DownloadDir string
	StateDir    string
	AsUser      bool
	APIURL      string // test hook: SLACK_THREAD_API_URL
	getenv      func(string) string
}

// Load reads the config file (if any) and applies environment overrides.
func Load(getenv func(string) string) (*Config, error) {
	c := &Config{getenv: getenv, AsUser: true}
	c.Path = getenv("SLACK_THREAD_CONFIG")
	if c.Path == "" {
		dir, err := configDir(getenv)
		if err != nil {
			return nil, err
		}
		c.Path = filepath.Join(dir, "slack-thread", "config.json")
	}
	data, err := os.ReadFile(c.Path)
	switch {
	case err == nil:
		c.FileExists = true
		if err := json.Unmarshal(data, &c.File); err != nil {
			return nil, fmt.Errorf("bad config %s: %w", c.Path, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}

	c.Owners = c.File.OwnerUserIDs
	if v := getenv("SLACK_THREAD_OWNER_IDS"); v != "" {
		c.Owners = splitList(v)
	}
	if c.File.AsUser != nil {
		c.AsUser = *c.File.AsUser
	}
	c.DownloadDir = firstNonEmpty(getenv("SLACK_THREAD_DOWNLOAD_DIR"), expandHome(c.File.DownloadDir, getenv))
	if c.DownloadDir == "" {
		cache, err := cacheDir(getenv)
		if err != nil {
			return nil, err
		}
		c.DownloadDir = filepath.Join(cache, "slack-thread", "files")
	}
	c.StateDir = firstNonEmpty(getenv("SLACK_THREAD_STATE_DIR"), expandHome(c.File.StateDir, getenv))
	if c.StateDir == "" {
		state, err := stateDir(getenv)
		if err != nil {
			return nil, err
		}
		c.StateDir = filepath.Join(state, "slack-thread")
	}
	if v := getenv("SLACK_THREAD_API_URL"); v != "" {
		// A test hook: honoured only for loopback hosts, so an environment
		// variable can never send the token anywhere else.
		u, err := url.Parse(v)
		if err != nil || !loopback(u.Hostname()) {
			return nil, fmt.Errorf("SLACK_THREAD_API_URL must point at localhost (it is a test hook), not %q", v)
		}
		c.APIURL = v
	}
	return c, nil
}

// TokenSource describes where the token comes from, without revealing it.
func (c *Config) TokenSource() string {
	switch {
	case c.getenv("SLACK_THREAD_TOKEN") != "":
		return "SLACK_THREAD_TOKEN environment variable"
	case c.File.Token != "":
		return "token in " + c.Path
	case c.File.TokenFile != "":
		return "token_file " + c.File.TokenFile
	case c.File.TokenCommand != "":
		return "token_command in " + c.Path
	default:
		return "none"
	}
}

// Token resolves the Slack token.
func (c *Config) Token(ctx context.Context) (string, error) {
	if t := strings.TrimSpace(c.getenv("SLACK_THREAD_TOKEN")); t != "" {
		return t, nil
	}
	if t := strings.TrimSpace(c.File.Token); t != "" {
		return t, nil
	}
	if c.File.TokenFile != "" {
		data, err := os.ReadFile(expandHome(c.File.TokenFile, c.getenv))
		if err != nil {
			return "", fmt.Errorf("token_file: %w", err)
		}
		if t := strings.TrimSpace(string(data)); t != "" {
			return t, nil
		}
		return "", fmt.Errorf("token_file %s is empty", c.File.TokenFile)
	}
	if c.File.TokenCommand != "" {
		return runTokenCommand(ctx, c.File.TokenCommand)
	}
	return "", fmt.Errorf("no Slack token: set SLACK_THREAD_TOKEN, or token / token_file / token_command in %s (see `slack-thread help`)", c.Path)
}

// tokenWaitDelay bounds how long a token_command's leftover children may
// hold its output pipes open after it exits (a variable for tests).
var tokenWaitDelay = 5 * time.Second

func runTokenCommand(ctx context.Context, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := shellCommand(ctx, command)
	// A grandchild that inherits the pipes must not hold us past the timeout.
	cmd.WaitDelay = tokenWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		err = nil // it printed the token and exited; a leftover child kept the pipe open
	}
	if err != nil {
		// Keep the echo short: wait prints errors where the model reads them.
		detail := strings.TrimSpace(stderr.String())
		if i := strings.IndexAny(detail, "\r\n"); i >= 0 {
			detail = detail[:i]
		}
		if len(detail) > 200 {
			detail = detail[:200] + "..."
		}
		return "", fmt.Errorf("token_command failed: %v: %s", err, detail)
	}
	token := strings.TrimSpace(stdout.String())
	if token == "" {
		return "", errors.New("token_command printed nothing")
	}
	return token, nil
}

// configDir: XDG on macOS and Linux (~/.config, as most CLIs do), the
// roaming AppData folder on Windows.
func configDir(getenv func(string) string) (string, error) {
	if runtime.GOOS == "windows" {
		return os.UserConfigDir()
	}
	if v := getenv("XDG_CONFIG_HOME"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".config"), err
}

// stateDir: XDG_STATE_HOME (~/.local/state) on macOS and Linux, the local
// AppData folder on Windows.
func stateDir(getenv func(string) string) (string, error) {
	if runtime.GOOS == "windows" {
		return os.UserCacheDir()
	}
	if v := getenv("XDG_STATE_HOME"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".local", "state"), err
}

func cacheDir(getenv func(string) string) (string, error) {
	if runtime.GOOS != "windows" {
		if v := getenv("XDG_CACHE_HOME"); v != "" {
			return v, nil
		}
	}
	return os.UserCacheDir()
}

func expandHome(path string, getenv func(string) string) string {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

// IsLoopbackHost reports whether a host[:port] is localhost or a loopback IP.
func IsLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return loopback(host)
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func splitList(v string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
		out = append(out, part)
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
