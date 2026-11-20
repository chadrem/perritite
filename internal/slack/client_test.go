package slack_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chadrem/slack-thread/internal/slack"
	"github.com/chadrem/slack-thread/internal/slacktest"
)

func newClient(fake *slacktest.Server, sleeps *[]time.Duration) *slack.Client {
	c := slack.New("xoxb-test")
	c.BaseURL = fake.APIURL()
	c.Sleep = func(_ context.Context, d time.Duration) error {
		if sleeps != nil {
			*sleeps = append(*sleeps, d)
		}
		return nil
	}
	return c
}

func TestCallSendsBearerTokenAndDecodes(t *testing.T) {
	fake := slacktest.New(t)
	fake.OK("auth.test", map[string]any{"user": "bot", "user_id": "U9", "team": "T", "url": "https://x.slack.com/"})

	info, err := newClient(fake, nil).AuthTest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.UserID != "U9" {
		t.Fatalf("user id = %q", info.UserID)
	}
	if got := fake.Calls("auth.test")[0].Auth; got != "Bearer xoxb-test" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestRateLimitedCallsRetryAfterTheAdvertisedDelay(t *testing.T) {
	fake := slacktest.New(t)
	attempts := 0
	fake.On("auth.test", func(url.Values) slacktest.Response {
		attempts++
		if attempts == 1 {
			return slacktest.Response{Status: 429, Headers: map[string]string{"Retry-After": "7"}}
		}
		return slacktest.Body(map[string]any{"user_id": "U9"})
	})
	var sleeps []time.Duration
	info, err := newClient(fake, &sleeps).AuthTest(context.Background())
	if err != nil || info.UserID != "U9" {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if len(sleeps) != 1 || sleeps[0] != 7*time.Second {
		t.Fatalf("sleeps = %v, want [7s] (must honour Retry-After, not a fixed floor)", sleeps)
	}
}

func TestRateLimitRetriesExhaustAndReturnAnError(t *testing.T) {
	fake := slacktest.New(t)
	attempts := 0
	fake.On("auth.test", func(url.Values) slacktest.Response {
		attempts++
		return slacktest.Response{Status: 429, Headers: map[string]string{"Retry-After": "1"}}
	})
	var sleeps []time.Duration
	_, err := newClient(fake, &sleeps).AuthTest(context.Background())
	var rl *slack.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want RateLimitError", err)
	}
	if attempts != slack.RateLimitRetries+1 || len(sleeps) != slack.RateLimitRetries {
		t.Fatalf("attempts=%d sleeps=%d", attempts, len(sleeps))
	}
	if !slack.IsTransient(err) {
		t.Fatal("an exhausted rate limit is transient")
	}
}

func TestAPIErrorsArePermanentExceptKnownTransientCodes(t *testing.T) {
	fake := slacktest.New(t)
	fake.On("auth.test", func(url.Values) slacktest.Response { return slacktest.Err("invalid_auth") })
	_, err := newClient(fake, nil).AuthTest(context.Background())
	var apiErr *slack.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "invalid_auth" {
		t.Fatalf("err = %v", err)
	}
	if slack.IsTransient(err) {
		t.Fatal("invalid_auth must not be retried")
	}
	if !slack.IsTransient(&slack.APIError{Code: "internal_error"}) {
		t.Fatal("internal_error is transient")
	}
	if !slack.IsTransient(&slack.HTTPError{Status: 503}) || slack.IsTransient(&slack.HTTPError{Status: 404}) {
		t.Fatal("5xx transient, 4xx permanent")
	}
}

func TestRepliesFollowsPaginationCursors(t *testing.T) {
	fake := slacktest.New(t)
	fake.On("conversations.replies", slacktest.Pages("messages",
		[]map[string]any{slacktest.Msg("1.0", "U1", "a")},
		[]map[string]any{slacktest.Msg("2.0", "U1", "b")}))
	msgs, err := newClient(fake, nil).Replies(context.Background(), "C1", "1.0", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[1].Text != "b" {
		t.Fatalf("msgs = %+v", msgs)
	}
	calls := fake.Calls("conversations.replies")
	if calls[0].Params.Get("ts") != "1.0" || calls[1].Params.Get("cursor") != "page-1" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestPostMessageSendsThreadFields(t *testing.T) {
	fake := slacktest.New(t)
	fake.OK("chat.postMessage", map[string]any{"channel": "C1", "ts": "5.5"})
	_, ts, err := newClient(fake, nil).PostMessage(context.Background(), slack.PostParams{
		Channel: "C1", Text: "hi", ThreadTS: "1.0", Broadcast: true, AsUser: true,
	})
	if err != nil || ts != "5.5" {
		t.Fatalf("ts=%q err=%v", ts, err)
	}
	p := fake.Calls("chat.postMessage")[0].Params
	for k, want := range map[string]string{"thread_ts": "1.0", "reply_broadcast": "true", "as_user": "true", "unfurl_links": "false"} {
		if p.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, p.Get(k), want)
		}
	}
}

func TestUploadFilesUsesTheExternalUploadFlow(t *testing.T) {
	fake := slacktest.New(t)
	var uploaded string
	var uploadAuth string
	fake.Raw("/upload/F1", func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		uploaded = buf.String()
		uploadAuth = r.Header.Get("Authorization")
	})
	fake.On("files.getUploadURLExternal", func(p url.Values) slacktest.Response {
		if p.Get("filename") != "shot.png" || p.Get("length") != "8" {
			t.Errorf("params = %v", p)
		}
		return slacktest.Body(map[string]any{"upload_url": fake.URL + "/upload/F1", "file_id": "F1"})
	})
	fake.OK("files.completeUploadExternal", nil)

	ids, err := newClient(fake, nil).UploadFiles(context.Background(), "C1", "1.0", "see shot",
		[]slack.Upload{{Name: "shot.png", Content: []byte("PNGBYTES")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "F1" || uploaded != "PNGBYTES" {
		t.Fatalf("ids=%v uploaded=%q", ids, uploaded)
	}
	if uploadAuth != "" {
		t.Fatal("the pre-signed upload URL must not receive the token")
	}
	p := fake.Calls("files.completeUploadExternal")[0].Params
	if p.Get("channel_id") != "C1" || p.Get("thread_ts") != "1.0" || p.Get("initial_comment") != "see shot" ||
		!strings.Contains(p.Get("files"), `"id":"F1"`) {
		t.Fatalf("complete params = %v", p)
	}
}

func TestDownloadAuthorizesOnlyTrustedHostsAcrossRedirects(t *testing.T) {
	cdn := slacktest.New(t)
	var cdnAuth = "unset"
	cdn.Raw("/signed", func(w http.ResponseWriter, r *http.Request) {
		cdnAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("BYTES"))
	})
	files := slacktest.New(t)
	var filesAuth string
	files.Raw("/a/b.png", func(w http.ResponseWriter, r *http.Request) {
		filesAuth = r.Header.Get("Authorization")
		http.Redirect(w, r, cdn.URL+"/signed", http.StatusFound)
	})
	c := newClient(files, nil)
	filesHost := strings.TrimPrefix(files.URL, "http://")
	c.TrustedHost = func(host string) bool { return host == filesHost }

	data, err := c.Download(context.Background(), files.URL+"/a/b.png", 1<<20)
	if err != nil || string(data) != "BYTES" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	if filesAuth != "Bearer xoxb-test" {
		t.Fatalf("trusted host auth = %q", filesAuth)
	}
	if cdnAuth != "" {
		t.Fatal("the token must never be presented to a non-Slack redirect target")
	}
}

func TestDownloadGivesUpOnARedirectLoop(t *testing.T) {
	fake := slacktest.New(t)
	fake.Raw("/loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	})
	_, err := newClient(fake, nil).Download(context.Background(), fake.URL+"/loop", 100)
	if err == nil || !strings.Contains(err.Error(), "too many redirects") {
		t.Fatalf("err = %v", err)
	}
}

func TestDownloadEnforcesTheSizeCap(t *testing.T) {
	fake := slacktest.New(t)
	fake.Raw("/big", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("0123456789")) })
	_, err := newClient(fake, nil).Download(context.Background(), fake.URL+"/big", 5)
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("err = %v", err)
	}
}

func TestSlackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"files.slack.com": true, "slack.com": true, "x.slack-files.com": true, "FILES.SLACK.COM:443": true,
		"evilslack.com": false, "slack.com.evil.io": false, "cdn.example.com": false,
	} {
		if got := slack.SlackHost(host); got != want {
			t.Errorf("SlackHost(%q) = %v, want %v", host, got, want)
		}
	}
}
