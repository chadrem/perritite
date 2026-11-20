package main_test

// End-to-end: build the real binary and run it as separate processes
// against a fake Slack, the way a Claude Code session runs it.

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"net/url"

	"github.com/chadrem/slack-thread/internal/slacktest"
)

const threadURL = "https://x.slack.com/archives/C0123ABCDEF/p1752241234567890"

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "slack-thread-e2e")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "slack-thread")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type world struct {
	fake *slacktest.Server
	env  []string
	mu   sync.Mutex
	msgs []map[string]any
}

func newWorld(t *testing.T) *world {
	w := &world{fake: slacktest.New(t), msgs: []map[string]any{slacktest.Msg("1752241234.567890", "U1", "root")}}
	dir := t.TempDir()
	w.env = append(os.Environ(),
		"SLACK_THREAD_TOKEN=xoxb-test",
		"SLACK_THREAD_API_URL="+w.fake.APIURL(),
		"SLACK_THREAD_CONFIG="+filepath.Join(dir, "none.json"),
		"SLACK_THREAD_STATE_DIR="+filepath.Join(dir, "state"),
		"SLACK_THREAD_DOWNLOAD_DIR="+filepath.Join(dir, "files"),
	)
	w.fake.OK("auth.test", map[string]any{"user_id": "UME"})
	w.fake.Members(slacktest.DefaultMembers())
	w.fake.On("conversations.replies", func(url.Values) slacktest.Response {
		w.mu.Lock()
		defer w.mu.Unlock()
		return slacktest.Body(map[string]any{"messages": append([]map[string]any(nil), w.msgs...)})
	})
	return w
}

func (w *world) post(ts, user, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.msgs = append(w.msgs, slacktest.Msg(ts, user, text))
}

func (w *world) start(t *testing.T, args ...string) (*exec.Cmd, *syncBuffer) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = w.env
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	return cmd, out
}

func (w *world) run(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = w.env
	out, _ := cmd.CombinedOutput()
	return string(out)
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitExit(t *testing.T, cmd *exec.Cmd, timeout time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("process did not exit within %s", timeout)
	}
}

func TestWaitIsSilentUntilSomeoneElsePostsThenExits(t *testing.T) {
	w := newWorld(t)
	cmd, out := w.start(t, "wait", threadURL, "--after", "1752241234.567890", "--interval", "1s", "--debounce", "0")

	time.Sleep(1500 * time.Millisecond)
	w.post("1752241300.000000", "UME", "our own reply")
	time.Sleep(1500 * time.Millisecond)
	if out.String() != "" || cmd.ProcessState != nil {
		t.Fatalf("must stay silent and running through our own post; output %q", out.String())
	}

	w.post("1752241400.000000", "U2", "a real question")
	waitExit(t, cmd, 5*time.Second)
	got := out.String()
	if !strings.HasPrefix(got, "NEW ts=1752241400.000000 user=U2 role=member | Abby Holtz") || !strings.Contains(got, "  | a real question") {
		t.Fatalf("output %q", got)
	}
	if cmd.ProcessState.ExitCode() != 0 {
		t.Fatalf("exit code %d", cmd.ProcessState.ExitCode())
	}
}

func TestASecondWaiterDefersAndStopEndsTheFirst(t *testing.T) {
	w := newWorld(t)
	first, firstOut := w.start(t, "wait", threadURL, "--after", "1752241234.567890", "--interval", "1s")
	time.Sleep(1500 * time.Millisecond)

	second := w.run(t, "wait", threadURL, "--interval", "1s")
	if !strings.HasPrefix(second, "ALREADY: another waiter (pid ") {
		t.Fatalf("second waiter: %q", second)
	}

	stop := w.run(t, "stop", threadURL)
	if !strings.HasPrefix(stop, "stopped waiter pid ") {
		t.Fatalf("stop: %q", stop)
	}
	waitExit(t, first, 5*time.Second)
	if runtime.GOOS != "windows" { // Windows has no SIGTERM: the stop is a hard kill
		if got := firstOut.String(); !strings.HasPrefix(got, "STOPPED:") {
			t.Fatalf("first waiter output %q", got)
		}
	}

	// The lock is gone, so a new waiter starts normally and resumes.
	w.post("1752241500.000000", "U3", "after the stop")
	if got := w.run(t, "wait", threadURL, "--interval", "1s", "--debounce", "0"); !strings.Contains(got, "after the stop") {
		t.Fatalf("resume after stop: %q", got)
	}
}

func TestReadAndPostThroughTheBinary(t *testing.T) {
	w := newWorld(t)
	w.fake.OK("conversations.info", map[string]any{"channel": map[string]any{"name": "eng"}})
	w.fake.OK("chat.postMessage", map[string]any{"channel": "C0123ABCDEF", "ts": "1752241600.000000"})
	w.fake.OK("chat.getPermalink", map[string]any{"permalink": "https://x.slack.com/archives/C0123ABCDEF/p1752241600000000"})

	if got := w.run(t, "read", threadURL); !strings.Contains(got, "#eng (C0123ABCDEF) — thread 1752241234.567890 — 1 messages") {
		t.Fatalf("read: %q", got)
	}
	cmd := exec.Command(binary, "post", threadURL, "-")
	cmd.Env = w.env
	cmd.Stdin = strings.NewReader("hello @{Abby Holtz}")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.HasPrefix(string(out), "posted ts=1752241600.000000") {
		t.Fatalf("post: %q %v", out, err)
	}
	if got := w.fake.Calls("chat.postMessage")[0].Params.Get("text"); got != "hello <@U2>" {
		t.Fatalf("text %q", got)
	}
	sc := bufio.NewScanner(strings.NewReader(w.run(t, "version")))
	if !sc.Scan() || sc.Text() != "slack-thread dev" {
		t.Fatalf("version: %q", sc.Text())
	}
}
