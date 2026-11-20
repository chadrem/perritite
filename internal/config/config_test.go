package config_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/chadrem/slack-thread/internal/config"
)

func load(t *testing.T, vars map[string]string, file string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if file != "" {
		if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if vars == nil {
		vars = map[string]string{}
	}
	vars["SLACK_THREAD_CONFIG"] = path
	if _, ok := vars["XDG_STATE_HOME"]; !ok {
		vars["XDG_STATE_HOME"] = filepath.Join(dir, "state")
	}
	cfg, err := config.Load(func(k string) string { return vars[k] })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func token(t *testing.T, cfg *config.Config) (string, error) {
	t.Helper()
	return cfg.Token(context.Background())
}

func TestTokenPrecedenceEnvThenTokenThenFileThenCommand(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "tok")
	_ = os.WriteFile(tokenFile, []byte("from-file\n"), 0o600)
	file := `{"token_file":"` + filepath.ToSlash(tokenFile) + `","token_command":"echo from-command"}`

	if got, _ := token(t, load(t, map[string]string{"SLACK_THREAD_TOKEN": "from-env"}, `{"token":"from-config"}`)); got != "from-env" {
		t.Errorf("env should win, got %q", got)
	}
	if got, _ := token(t, load(t, nil, `{"token":"from-config","token_command":"echo x"}`)); got != "from-config" {
		t.Errorf("token should win over command, got %q", got)
	}
	if got, _ := token(t, load(t, nil, file)); got != "from-file" {
		t.Errorf("token_file should win over command, got %q", got)
	}
	if got, err := token(t, load(t, nil, `{"token_command":"echo from-command"}`)); got != "from-command" {
		t.Errorf("token_command: got %q err=%v", got, err)
	}
}

func TestTokenCommandFailuresAreReported(t *testing.T) {
	cmd := "exit 3"
	if runtime.GOOS == "windows" {
		cmd = "exit /b 3"
	}
	if _, err := token(t, load(t, nil, `{"token_command":"`+cmd+`"}`)); err == nil || !strings.Contains(err.Error(), "token_command failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestNoTokenExplainsTheOptions(t *testing.T) {
	_, err := token(t, load(t, nil, ""))
	if err == nil || !strings.Contains(err.Error(), "SLACK_THREAD_TOKEN") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnvOverridesOwnersAndDirectories(t *testing.T) {
	cfg := load(t, map[string]string{"SLACK_THREAD_OWNER_IDS": "U1, U2", "SLACK_THREAD_STATE_DIR": "/s", "SLACK_THREAD_DOWNLOAD_DIR": "/d"},
		`{"owner_user_ids":["U9"],"state_dir":"/x","as_user":false}`)
	if strings.Join(cfg.Owners, ",") != "U1,U2" || cfg.StateDir != "/s" || cfg.DownloadDir != "/d" || cfg.AsUser {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestDefaultsWhenThereIsNoFile(t *testing.T) {
	cfg := load(t, nil, "")
	if cfg.FileExists || !cfg.AsUser || cfg.TokenSource() != "none" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if !strings.HasSuffix(cfg.StateDir, "slack-thread") || !strings.Contains(cfg.DownloadDir, "slack-thread") {
		t.Fatalf("dirs = %s / %s", cfg.StateDir, cfg.DownloadDir)
	}
}

func TestBadJSONIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	_ = os.WriteFile(path, []byte("{nope"), 0o600)
	if _, err := config.Load(func(k string) string {
		if k == "SLACK_THREAD_CONFIG" {
			return path
		}
		return ""
	}); err == nil {
		t.Fatal("want an error")
	}
}

func TestTheAPIURLHookIsLoopbackOnly(t *testing.T) {
	for url, ok := range map[string]bool{
		"http://127.0.0.1:1234/api/": true, "http://localhost:9/api/": true, "http://[::1]:9/": true,
		"https://slack.com/api/": false, "http://10.0.0.1/": false, "http://localhost.evil.io/": false,
	} {
		dir := t.TempDir()
		_, err := config.Load(func(k string) string {
			switch k {
			case "SLACK_THREAD_API_URL":
				return url
			case "SLACK_THREAD_CONFIG":
				return filepath.Join(dir, "none.json")
			}
			return ""
		})
		if (err == nil) != ok {
			t.Errorf("%s: err=%v", url, err)
		}
	}
}

func TestTokenCommandErrorsEchoOnlyAShortFirstLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh syntax")
	}
	cfg := load(t, nil, `{"token_command":"echo first-line >&2; echo second-line >&2; exit 1"}`)
	_, err := token(t, cfg)
	if err == nil || !strings.Contains(err.Error(), "first-line") || strings.Contains(err.Error(), "second-line") {
		t.Fatalf("err = %v", err)
	}
}
