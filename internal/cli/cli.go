// Package cli turns argv into calls on the thread and watch packages and
// renders their results for a terminal or a Claude Code session.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/chadrem/perritite/internal/config"
	"github.com/chadrem/perritite/internal/frame"
	"github.com/chadrem/perritite/internal/slack"
	"github.com/chadrem/perritite/internal/thread"
	"github.com/chadrem/perritite/internal/watch"
)

// Usage is printed by `perritite help`.
const Usage = `Usage: perritite <command> [args]

Commands:
  read <url> [--after TS] [--limit N] [--no-download] [--json]
      Print the thread oldest first. Attachments are downloaded and their
      local paths printed. A channel URL (no /p...) prints recent history.

  post <url> <message|-> [--file PATH]... [--raw] [--broadcast]
      Reply in the thread ('-' reads the message from stdin). Mentions:
      @{Full Name} (strict), @handle (unique match), @here/@channel.
      --file uploads attachments with the message as their comment.
      --raw sends the text verbatim (no escaping, no mention resolution).
      --broadcast also shows the reply in the channel.

  wait <url> [--after TS] [--idle 24h] [--interval 15s] [--debounce 20s]
             [--keep-warm 55m] [--keep-warm-for 4h] [--max-run DUR] [--replace]
      Block until someone else posts, print their messages, and exit. Prints
      nothing while waiting and never reports your own posts, so run it as a
      background command and treat its exit as the notification. Re-run it
      with no --after to resume where it stopped. --idle 0 waits forever.

  stop <url>        Stop the waiter on a thread.
  users <query>     Find workspace users by name, handle or email.
  whoami            Show the identity the token posts as.
  config            Show where settings, state and downloads live.
  version           Print the version.

Token: PERRITITE_TOKEN, or token / token_file / token_command in the
config file (see 'perritite config' for its path). Setup guide:
https://github.com/chadrem/perritite#quick-start
`

// Env is the process environment, injectable for tests.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	Getwd  func() (string, error)
	PID    int
	// Version is stamped at build time.
	Version string
	// Context, when set, replaces the signal-aware default (tests).
	Context context.Context
}

// Main runs the CLI and returns the exit code.
func Main(args []string, env Env) int {
	c := &cmd{env: env}
	if err := c.run(args); err != nil {
		var exit exitError
		if errors.As(err, &exit) {
			return int(exit)
		}
		fmt.Fprintln(env.Stderr, "error: "+err.Error())
		return 1
	}
	return 0
}

type exitError int

func (e exitError) Error() string { return "exit " + strconv.Itoa(int(e)) }

type cmd struct {
	env        Env
	positional []string
	files      []string
	flags      map[string]string
	bools      map[string]bool
	cfg        *config.Config
}

func (c *cmd) run(args []string) error {
	if err := c.parse(args); err != nil {
		return err
	}
	if len(c.positional) == 0 {
		fmt.Fprint(c.env.Stdout, Usage)
		return nil
	}
	name := c.positional[0]
	c.positional = c.positional[1:]
	switch name {
	case "help", "-h", "--help":
		fmt.Fprint(c.env.Stdout, Usage)
		return nil
	case "version", "--version":
		fmt.Fprintln(c.env.Stdout, "perritite "+c.env.Version)
		return nil
	case "read":
		return c.read()
	case "post":
		return c.post()
	case "wait":
		return c.wait()
	case "stop":
		return c.stop()
	case "users":
		return c.users()
	case "whoami":
		return c.whoami()
	case "config":
		return c.showConfig()
	default:
		fmt.Fprint(c.env.Stderr, Usage)
		return fmt.Errorf("unknown command %q", name)
	}
}

var valueFlags = map[string]bool{
	"--after": true, "--limit": true, "--idle": true, "--interval": true, "--debounce": true,
	"--keep-warm": true, "--keep-warm-for": true, "--max-run": true,
}

var boolFlags = map[string]bool{
	"--raw": true, "--broadcast": true, "--no-download": true, "--json": true, "--replace": true,
}

func (c *cmd) parse(args []string) error {
	c.flags, c.bools = map[string]string{}, map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			// Everything after -- is positional, so a message may start with --.
			c.positional = append(c.positional, args[i+1:]...)
			i = len(args)
		case arg == "--file":
			v, err := value(args, &i)
			if err != nil {
				return err
			}
			c.files = append(c.files, v)
		case valueFlags[arg]:
			v, err := value(args, &i)
			if err != nil {
				return err
			}
			c.flags[arg] = v
		case boolFlags[arg]:
			c.bools[arg] = true
		case strings.HasPrefix(arg, "--") && arg != "--help" && arg != "--version":
			return fmt.Errorf("unknown flag %s", arg)
		default:
			c.positional = append(c.positional, arg)
		}
	}
	if after, ok := c.flags["--after"]; ok {
		c.flags["--after"] = strings.TrimPrefix(after, "ts=") // a pasted ts= prefix is fine
	}
	return nil
}

func value(args []string, i *int) (string, error) {
	flag := args[*i]
	if *i+1 >= len(args) || strings.HasPrefix(args[*i+1], "--") {
		return "", fmt.Errorf("%s requires a value", flag)
	}
	*i++
	return args[*i], nil
}

func (c *cmd) intFlag(name string) (int, error) {
	v, ok := c.flags[name]
	if !ok {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s requires a positive integer", name)
	}
	return n, nil
}

// durationFlag accepts Go durations ("24h", "90s") or bare seconds ("86400").
func (c *cmd) durationFlag(name string, def time.Duration) (time.Duration, error) {
	v, ok := c.flags[name]
	if !ok {
		return def, nil
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s requires a duration like 15s, 20m or 24h", name)
	}
	return d, nil
}

// done rejects leftover positionals: an unquoted message would otherwise
// be silently cut to its first word.
func (c *cmd) done(command string) error {
	if len(c.positional) > 0 {
		return fmt.Errorf("unexpected argument %q for %s (quote a multi-word message, or use - to read it from stdin)", c.positional[0], command)
	}
	return nil
}

func (c *cmd) need(what string) (string, error) {
	if len(c.positional) == 0 {
		return "", fmt.Errorf("%s", what)
	}
	v := c.positional[0]
	c.positional = c.positional[1:]
	return v, nil
}

func (c *cmd) config() (*config.Config, error) {
	if c.cfg == nil {
		cfg, err := config.Load(c.env.Getenv)
		if err != nil {
			return nil, err
		}
		c.cfg = cfg
	}
	return c.cfg, nil
}

func (c *cmd) service(ctx context.Context) (*thread.Service, error) {
	cfg, err := c.config()
	if err != nil {
		return nil, err
	}
	token, err := cfg.Token(ctx)
	if err != nil {
		return nil, err
	}
	api := slack.New(token)
	if cfg.APIURL != "" {
		api.BaseURL = cfg.APIURL
		// Test servers are plain-http localhost: trust loopback only, never
		// anything a redirect points at.
		api.TrustedHost = config.IsLoopbackHost
	}
	return &thread.Service{API: api, DownloadRoot: cfg.DownloadDir, AsUser: cfg.AsUser(token)}, nil
}

func (c *cmd) ctx() (context.Context, context.CancelFunc) {
	if c.env.Context != nil {
		return context.WithCancel(c.env.Context)
	}
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// ---------------------------------------------------------------------------
// read
// ---------------------------------------------------------------------------

func (c *cmd) read() error {
	url, err := c.need("read requires a thread URL")
	if err != nil {
		return err
	}
	if err := c.done("read"); err != nil {
		return err
	}
	limit, err := c.intFlag("--limit")
	if err != nil {
		return err
	}
	ctx, cancel := c.ctx()
	defer cancel()
	svc, err := c.service(ctx)
	if err != nil {
		return err
	}
	res, err := svc.Read(ctx, url, thread.ReadOptions{After: c.flags["--after"], Limit: limit, Download: !c.bools["--no-download"]})
	if err != nil {
		return err
	}
	out := c.env.Stdout
	if c.bools["--json"] {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	scope := "recent channel history"
	if res.ThreadTS != "" {
		scope = "thread " + res.ThreadTS
	}
	fmt.Fprintf(out, "%s (%s) — %s — %d messages\n", res.ChannelLabel, res.Channel, scope, len(res.Messages))
	if res.DownloadDir != "" {
		fmt.Fprintln(out, "downloads: "+res.DownloadDir)
	}
	for i, m := range res.Messages {
		fmt.Fprintln(out)
		edited, id := "", ""
		if m.Edited {
			edited = " (edited)"
		}
		if m.UserID != "" {
			id = " (" + m.UserID + ")"
		}
		fmt.Fprintf(out, "[%d] %s%s — %s%s (ts %s)\n", i+1, frame.SafeAuthor(m.Author), id, m.Time, edited, m.TS)
		if m.Text != "" {
			fmt.Fprintln(out, frame.Indent("  ", m.Text))
		}
		for _, f := range m.Files {
			detail := f.Path
			if detail == "" {
				detail = f.Note
			}
			fmt.Fprintln(out, frame.Indent("  ", fmt.Sprintf("[file] %s (%s, %s) -> %s", f.Name, f.Mimetype, formatSize(f.Size), detail)))
		}
		if len(m.Reactions) > 0 {
			parts := make([]string, len(m.Reactions))
			for j, r := range m.Reactions {
				parts[j] = fmt.Sprintf(":%s: x%d", r.Name, r.Count)
			}
			fmt.Fprintln(out, frame.Indent("  ", "[reactions] "+strings.Join(parts, ", ")))
		}
	}
	return nil
}

func formatSize(bytes int64) string {
	switch {
	case bytes < 1024:
		return fmt.Sprintf("%d B", bytes)
	case bytes < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
	}
}

// ---------------------------------------------------------------------------
// post
// ---------------------------------------------------------------------------

func (c *cmd) post() error {
	url, err := c.need("post requires a thread URL")
	if err != nil {
		return err
	}
	text := ""
	if len(c.positional) > 0 {
		text = c.positional[0]
		c.positional = c.positional[1:]
	}
	if err := c.done("post"); err != nil {
		return err
	}
	// Only an explicit '-' reads stdin: an inherited, never-closed stdin
	// pipe would otherwise hang the post.
	if text == "-" {
		data, err := io.ReadAll(c.env.Stdin)
		if err != nil {
			return err
		}
		text = string(data)
	}
	files := make([]string, len(c.files))
	for i, f := range c.files {
		files[i] = c.absPath(f)
	}
	ctx, cancel := c.ctx()
	defer cancel()
	svc, err := c.service(ctx)
	if err != nil {
		return err
	}
	res, err := svc.Post(ctx, url, text, thread.PostOptions{Files: files, Raw: c.bools["--raw"], Broadcast: c.bools["--broadcast"]})
	if err != nil {
		return err
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(c.env.Stderr, "warning: "+w)
	}
	if len(res.Files) > 0 {
		fmt.Fprintf(c.env.Stdout, "uploaded %d file(s): %s\n", len(res.Files), strings.Join(res.Files, ", "))
		return nil
	}
	fmt.Fprintln(c.env.Stdout, "posted ts="+res.TS)
	if res.Permalink != "" {
		fmt.Fprintln(c.env.Stdout, res.Permalink)
	}
	return nil
}

func (c *cmd) absPath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	getwd := c.env.Getwd
	if getwd == nil {
		getwd = os.Getwd
	}
	if wd, err := getwd(); err == nil {
		return filepath.Join(wd, p)
	}
	return p
}

// ---------------------------------------------------------------------------
// wait / stop
// ---------------------------------------------------------------------------

func (c *cmd) wait() error {
	url, err := c.need("wait requires a thread URL")
	if err != nil {
		return err
	}
	if err := c.done("wait"); err != nil {
		return err
	}
	opts := watch.Options{URL: url, After: c.flags["--after"], Replace: c.bools["--replace"]}
	for _, d := range []struct {
		flag string
		def  time.Duration
		dst  *time.Duration
	}{
		{"--idle", 24 * time.Hour, &opts.Idle},
		{"--interval", 15 * time.Second, &opts.Interval},
		{"--debounce", 20 * time.Second, &opts.Debounce},
		{"--keep-warm", 0, &opts.KeepWarm},
		{"--keep-warm-for", 4 * time.Hour, &opts.KeepWarmFor},
		{"--max-run", 0, &opts.MaxRun},
	} {
		if *d.dst, err = c.durationFlag(d.flag, d.def); err != nil {
			return err
		}
	}
	if opts.Interval < time.Second {
		return errors.New("--interval must be at least 1s")
	}
	if opts.MaxRun > 0 && opts.MaxRun < 2*opts.Interval {
		return errors.New("--max-run must be at least twice --interval (or each run would do a single poll and exit)")
	}
	ctx, cancel := c.ctx()
	defer cancel()
	cfg, err := c.config()
	if err != nil {
		return err
	}
	w := &watch.Waiter{Store: &watch.Store{Dir: cfg.StateDir}, Out: c.env.Stdout, PID: c.env.PID, Owners: map[string]bool{}}
	for _, id := range cfg.Owners {
		w.Owners[id] = true
	}
	if w.Svc, err = c.service(ctx); err != nil {
		// Explain on stdout too: a background run is read from its output.
		fmt.Fprintln(c.env.Stdout, "ERROR: "+frame.OneLine(err.Error()))
		return err
	}
	if outcome, _ := w.Run(ctx, opts); outcome == watch.Failed {
		return exitError(1) // the ERROR line is already on stdout
	}
	return nil
}

func (c *cmd) stop() error {
	url, err := c.need("stop requires a thread URL")
	if err != nil {
		return err
	}
	if err := c.done("stop"); err != nil {
		return err
	}
	cfg, err := c.config()
	if err != nil {
		return err
	}
	ctx, cancel := c.ctx()
	defer cancel()
	msg, err := watch.Stop(ctx, &watch.Store{Dir: cfg.StateDir}, url, watch.Terminate, watch.RealClock{})
	if err != nil {
		return err
	}
	fmt.Fprintln(c.env.Stdout, msg)
	return nil
}

// ---------------------------------------------------------------------------
// users / whoami / config
// ---------------------------------------------------------------------------

func (c *cmd) users() error {
	query, err := c.need("users requires a query")
	if err != nil {
		return err
	}
	ctx, cancel := c.ctx()
	defer cancel()
	svc, err := c.service(ctx)
	if err != nil {
		return err
	}
	matches, err := svc.SearchUsers(ctx, query)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		fmt.Fprintf(c.env.Stdout, "no users match %q\n", query)
		return nil
	}
	for _, m := range matches {
		var flags []string
		if m.Deleted {
			flags = append(flags, "deleted")
		}
		if m.IsBot {
			flags = append(flags, "bot")
		}
		name := m.Profile.RealName
		if name == "" {
			name = m.Name
		}
		line := m.ID + "  " + frame.OneLine(name)
		if m.Profile.DisplayName != "" {
			line += " (@" + frame.OneLine(m.Profile.DisplayName) + ")"
		}
		if len(flags) > 0 {
			line += " [" + strings.Join(flags, ", ") + "]"
		}
		fmt.Fprintln(c.env.Stdout, line)
	}
	return nil
}

func (c *cmd) whoami() error {
	ctx, cancel := c.ctx()
	defer cancel()
	svc, err := c.service(ctx)
	if err != nil {
		return err
	}
	me, err := svc.Whoami(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.env.Stdout, "%s (%s) in %s — %s\n", me.User, me.UserID, me.Team, me.URL)
	return nil
}

func (c *cmd) showConfig() error {
	cfg, err := c.config()
	if err != nil {
		return err
	}
	exists := " (not created)"
	if cfg.FileExists {
		exists = ""
	}
	owners := strings.Join(cfg.Owners, ", ")
	if owners == "" {
		owners = "(none: no message is tagged role=owner)"
	}
	out := c.env.Stdout
	fmt.Fprintf(out, "config file:  %s%s\n", cfg.Path, exists)
	fmt.Fprintf(out, "token from:   %s\n", cfg.TokenSource())
	fmt.Fprintf(out, "owners:       %s\n", owners)
	fmt.Fprintf(out, "state dir:    %s\n", cfg.StateDir)
	fmt.Fprintf(out, "download dir: %s\n", cfg.DownloadDir)
	fmt.Fprintf(out, "as_user:      %s\n", cfg.AsUserSetting())
	return nil
}
