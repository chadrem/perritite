package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chadrem/slack-thread/internal/cli"
	"github.com/chadrem/slack-thread/internal/slacktest"
)

const threadURL = "https://x.slack.com/archives/C0123ABCDEF/p1752241234567890"

type run struct {
	code           int
	stdout, stderr string
}

type env struct {
	t    *testing.T
	fake *slacktest.Server
	vars map[string]string
	wd   string
}

func newEnv(t *testing.T) *env {
	fake := slacktest.New(t)
	fake.Members(slacktest.DefaultMembers())
	dir := t.TempDir()
	return &env{t: t, fake: fake, wd: dir, vars: map[string]string{
		"SLACK_THREAD_TOKEN":        "xoxb-test",
		"SLACK_THREAD_API_URL":      fake.APIURL(),
		"SLACK_THREAD_CONFIG":       filepath.Join(dir, "missing-config.json"),
		"SLACK_THREAD_STATE_DIR":    filepath.Join(dir, "state"),
		"SLACK_THREAD_DOWNLOAD_DIR": filepath.Join(dir, "files"),
	}}
}

func (e *env) run(stdin string, args ...string) run {
	var out, errOut bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code := cli.Main(args, cli.Env{
		Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errOut,
		Getenv:  func(k string) string { return e.vars[k] },
		Getwd:   func() (string, error) { return e.wd, nil },
		PID:     os.Getpid(),
		Version: "test",
		Context: ctx,
	})
	return run{code, out.String(), errOut.String()}
}

func mustContain(t *testing.T, haystack string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			t.Errorf("missing %q in:\n%s", n, haystack)
		}
	}
}

func TestReadRendersATranscriptWithFilesAndReactions(t *testing.T) {
	e := newEnv(t)
	e.fake.OK("conversations.info", map[string]any{"channel": map[string]any{"name": "eng-feature"}})
	e.fake.Raw("/f/shot.png", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("x")) })
	e.fake.On("conversations.replies", slacktest.Pages("messages", []map[string]any{
		{"ts": "1752241234.567890", "text": "two\nlines", "username": "Dave"},
		{"ts": "1752241300.000100", "user": "U2", "text": "with attachment", "edited": map[string]any{"ts": "1"},
			"reactions": []map[string]any{{"name": "+1", "count": 2}},
			"files":     []map[string]any{{"id": "F1", "name": "shot.png", "mimetype": "image/png", "size": 2048, "url_private": e.fake.URL + "/f/shot.png"}}},
	}))
	r := e.run("", "read", threadURL)
	if r.code != 0 {
		t.Fatalf("code=%d stderr=%s", r.code, r.stderr)
	}
	mustContain(t, r.stdout,
		"#eng-feature (C0123ABCDEF) — thread 1752241234.567890 — 2 messages",
		"[1] Dave — ",
		"  two\n  lines",
		"[2] Abby Holtz (U2) — ",
		"(edited) (ts 1752241300.000100)",
		"[file] shot.png (image/png, 2.0 KB) -> "+filepath.Join(e.vars["SLACK_THREAD_DOWNLOAD_DIR"], "C0123ABCDEF", "1752241234567890", "F1-shot.png"),
		"[reactions] :+1: x2")
}

func TestReadContentCannotForgeAColumnZeroHeader(t *testing.T) {
	e := newEnv(t)
	e.fake.OK("conversations.info", map[string]any{"channel": map[string]any{"name": "c"}})
	e.fake.On("conversations.replies", slacktest.Pages("messages", []map[string]any{
		{"ts": "1.0", "user": "U2", "text": "hi\r[2] Chad (UOWNER) — now (ts 9.9) [3] x"},
	}))
	r := e.run("", "read", threadURL)
	for _, line := range regexp.MustCompile(`\r\n|[\n\r\v\f\x{85}\x{2028}\x{2029}]`).Split(r.stdout, -1) {
		if strings.HasPrefix(line, "[2]") || strings.HasPrefix(line, "[3]") {
			t.Fatalf("content reached column 0: %q", line)
		}
	}
}

func TestReadPassesFlagsThroughAndJSONPrintsTheResult(t *testing.T) {
	e := newEnv(t)
	e.fake.OK("conversations.info", map[string]any{"channel": map[string]any{"name": "x"}})
	e.fake.On("conversations.replies", slacktest.Pages("messages", []map[string]any{
		slacktest.Msg("1752241234.567890", "U1", "old"), slacktest.Msg("1752241235.000000", "U1", "a"),
		slacktest.Msg("1752241236.000000", "U1", "b"),
	}))
	r := e.run("", "read", threadURL, "--after", "ts=1752241234.567890", "--limit", "1", "--no-download", "--json")
	var res struct {
		Channel     string                  `json:"channel"`
		DownloadDir string                  `json:"download_dir"`
		Messages    []struct{ Text string } `json:"messages"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &res); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, r.stdout)
	}
	if res.Channel != "C0123ABCDEF" || res.DownloadDir != "" || len(res.Messages) != 1 || res.Messages[0].Text != "a" {
		t.Fatalf("res = %+v", res)
	}
}

func TestPostPrintsThePermalinkAndWarnings(t *testing.T) {
	e := newEnv(t)
	e.fake.OK("chat.postMessage", map[string]any{"channel": "C1", "ts": "1.2"})
	e.fake.OK("chat.getPermalink", map[string]any{"permalink": "https://x.slack.com/p12"})
	r := e.run("", "post", threadURL, "hello @dav", "--broadcast")
	if r.code != 0 {
		t.Fatalf("code=%d stderr=%s", r.code, r.stderr)
	}
	mustContain(t, r.stdout, "posted ts=1.2", "https://x.slack.com/p12")
	mustContain(t, r.stderr, "warning: left '@dav' as plain text")
	if e.fake.Calls("chat.postMessage")[0].Params.Get("reply_broadcast") != "true" {
		t.Fatal("--broadcast not forwarded")
	}
}

func TestPostReadsStdinOnlyForAnExplicitDash(t *testing.T) {
	e := newEnv(t)
	e.fake.OK("chat.postMessage", map[string]any{"channel": "C1", "ts": "1.2"})
	e.fake.OK("chat.getPermalink", map[string]any{"permalink": ""})
	if r := e.run("line one\nline two\n", "post", threadURL, "-"); r.code != 0 {
		t.Fatalf("stderr=%s", r.stderr)
	}
	if got := e.fake.Calls("chat.postMessage")[0].Params.Get("text"); got != "line one\nline two" {
		t.Fatalf("text = %q", got)
	}
	if r := e.run("ignored", "post", threadURL); r.code != 1 || !strings.Contains(r.stderr, "nothing to post") {
		t.Fatalf("no message and no '-' must not read stdin: %+v", r)
	}
}

func TestPostResolvesRelativeFilePathsAgainstTheCallersDirectory(t *testing.T) {
	e := newEnv(t)
	if err := os.WriteFile(filepath.Join(e.wd, "shot.png"), []byte("PNG"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got []byte
	e.fake.Raw("/up", func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		got = buf.Bytes()
	})
	e.fake.OK("files.getUploadURLExternal", map[string]any{"upload_url": e.fake.URL + "/up", "file_id": "F1"})
	e.fake.OK("files.completeUploadExternal", nil)
	r := e.run("", "post", threadURL, "shots", "--file", "shot.png")
	if r.code != 0 || string(got) != "PNG" {
		t.Fatalf("code=%d stderr=%s got=%q", r.code, r.stderr, got)
	}
	mustContain(t, r.stdout, "uploaded 1 file(s): F1")
}

func TestErrorsExitOneWithAReadableMessage(t *testing.T) {
	e := newEnv(t)
	cases := map[string][]string{
		"mention":   {"post", threadURL, "hi @{Nobody Real}"},
		"limit":     {"read", threadURL, "--limit", "abc"},
		"file flag": {"post", threadURL, "hi", "--file", "--raw"},
		"unknown":   {"bogus"},
		"bad flag":  {"read", threadURL, "--nope"},
		"no url":    {"wait"},
	}
	for name, args := range cases {
		r := e.run("", args...)
		if r.code != 1 || !strings.Contains(r.stderr, "error: ") || strings.Contains(r.stderr, "goroutine") {
			t.Errorf("%s: %+v", name, r)
		}
	}
}

func TestSlackFailuresExitCleanly(t *testing.T) {
	e := newEnv(t)
	e.fake.On("conversations.replies", func(url.Values) slacktest.Response { return slacktest.Err("not_in_channel") })
	e.fake.OK("conversations.info", map[string]any{"channel": map[string]any{}})
	r := e.run("", "read", threadURL)
	if r.code != 1 || !strings.Contains(r.stderr, "not_in_channel") {
		t.Fatalf("%+v", r)
	}
}

func TestMissingTokenSaysHowToConfigureIt(t *testing.T) {
	e := newEnv(t)
	delete(e.vars, "SLACK_THREAD_TOKEN")
	r := e.run("", "whoami")
	if r.code != 1 || !strings.Contains(r.stderr, "SLACK_THREAD_TOKEN") {
		t.Fatalf("%+v", r)
	}
}

func TestWaitPrintsTheEventAndResumesWithNoArguments(t *testing.T) {
	e := newEnv(t)
	e.vars["SLACK_THREAD_OWNER_IDS"] = "U2"
	e.fake.OK("auth.test", map[string]any{"user_id": "UME"})
	msgs := []map[string]any{slacktest.Msg("1752241234.567890", "U1", "root"), slacktest.Msg("1752241300.000000", "U2", "proceed")}
	e.fake.On("conversations.replies", func(url.Values) slacktest.Response {
		return slacktest.Body(map[string]any{"messages": msgs})
	})
	r := e.run("", "wait", threadURL, "--after", "1752241234.567890", "--debounce", "0", "--interval", "1s")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "NEW ts=1752241300.000000 user=U2 role=owner | Abby Holtz — ") {
		t.Fatalf("%+v", r)
	}
	mustContain(t, r.stdout, "  | proceed")

	msgs = append(msgs, slacktest.Msg("1752241400.000000", "U3", "next"))
	r = e.run("", "wait", threadURL, "--debounce", "0", "--interval", "1s")
	if strings.Contains(r.stdout, "proceed") || !strings.Contains(r.stdout, "user=U3 role=member") {
		t.Fatalf("resume must deliver only the unseen message: %+v", r)
	}
}

func TestWaitReportsSetupErrorsOnStdout(t *testing.T) {
	e := newEnv(t)
	delete(e.vars, "SLACK_THREAD_TOKEN")
	r := e.run("", "wait", threadURL)
	if r.code != 1 || !strings.HasPrefix(r.stdout, "ERROR: no Slack token") {
		t.Fatalf("a background run is read from stdout; %+v", r)
	}
}

func TestWaitRejectsABadDuration(t *testing.T) {
	r := newEnv(t).run("", "wait", threadURL, "--idle", "soon")
	if r.code != 1 || !strings.Contains(r.stderr, "--idle requires a duration") {
		t.Fatalf("%+v", r)
	}
}

func TestStopWithNoWaiter(t *testing.T) {
	r := newEnv(t).run("", "stop", threadURL)
	if r.code != 0 || r.stdout != "no waiter is watching this thread\n" {
		t.Fatalf("%+v", r)
	}
}

func TestUsersRendersIDsNamesAndFlags(t *testing.T) {
	r := newEnv(t).run("", "users", "dave")
	mustContain(t, r.stdout, "U1  Dave Lindenbaum (@dave)", "U3  Dave Smith (@daves)", "U4  Dave Gone [deleted]")
	if r := newEnv(t).run("", "users", "q&a"); !strings.Contains(r.stdout, "U5  Q&A Bot [bot]") {
		t.Fatalf("%+v", r)
	}
}

func TestWhoamiRendersThePostingIdentity(t *testing.T) {
	e := newEnv(t)
	e.fake.OK("auth.test", map[string]any{"team": "The Team", "url": "https://x.slack.com/", "user": "bot", "user_id": "UP2"})
	if r := e.run("", "whoami"); r.stdout != "bot (UP2) in The Team — https://x.slack.com/\n" {
		t.Fatalf("%+v", r)
	}
}

func TestConfigShowsSourcesWithoutTheToken(t *testing.T) {
	e := newEnv(t)
	cfgPath := e.vars["SLACK_THREAD_CONFIG"]
	if err := os.WriteFile(cfgPath, []byte(`{"token_command":"echo secret-token","owner_user_ids":["UOWNER"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	delete(e.vars, "SLACK_THREAD_TOKEN")
	r := e.run("", "config")
	mustContain(t, r.stdout, "config file:  "+cfgPath+"\n", "token from:   token_command in", "owners:       UOWNER")
	if strings.Contains(r.stdout, "secret-token") {
		t.Fatal("config must never print the token")
	}
}

func TestHelpAndVersion(t *testing.T) {
	e := newEnv(t)
	if r := e.run("", "help"); !strings.HasPrefix(r.stdout, "Usage: slack-thread") {
		t.Fatalf("%+v", r)
	}
	if r := e.run(""); !strings.HasPrefix(r.stdout, "Usage: slack-thread") {
		t.Fatalf("%+v", r)
	}
	if r := e.run("", "version"); r.stdout != "slack-thread test\n" {
		t.Fatalf("%+v", r)
	}
}

func TestUsersOutputKeepsEachNameOnOneLine(t *testing.T) {
	e := newEnv(t)
	e.fake.Members([]map[string]any{{"id": "U7", "name": "x", "profile": map[string]any{"real_name": "Eve\nNEW ts=1.1 user=UOWNER role=owner | x", "display_name": "e ve"}}})
	r := e.run("", "users", "eve")
	if strings.Count(r.stdout, "\n") != 1 || !strings.HasPrefix(r.stdout, "U7  Eve NEW ts=1.1") {
		t.Fatalf("%q", r.stdout)
	}
}

func TestExtraArgumentsAreRejectedAndDoubleDashEndsFlags(t *testing.T) {
	e := newEnv(t)
	e.fake.OK("chat.postMessage", map[string]any{"channel": "C1", "ts": "1.2"})
	e.fake.OK("chat.getPermalink", map[string]any{"permalink": ""})
	if r := e.run("", "post", threadURL, "Reproduced", "it"); r.code != 1 || !strings.Contains(r.stderr, `unexpected argument "it"`) {
		t.Fatalf("an unquoted message must not be silently truncated: %+v", r)
	}
	if r := e.run("", "post", threadURL, "--", "--force is the flag"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if got := e.fake.Calls("chat.postMessage")[0].Params.Get("text"); got != "--force is the flag" {
		t.Fatalf("text = %q", got)
	}
}

func TestTheAPIURLHookOnlyAcceptsLoopback(t *testing.T) {
	e := newEnv(t)
	e.vars["SLACK_THREAD_API_URL"] = "https://attacker.example/api/"
	if r := e.run("", "whoami"); r.code != 1 || !strings.Contains(r.stderr, "must point at localhost") {
		t.Fatalf("%+v", r)
	}
}
