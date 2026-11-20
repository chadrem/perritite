// Package slack is a minimal Slack Web API client: just the methods
// perritite needs, over net/http, with rate-limit retries and cursor
// pagination. No third-party dependencies.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the Slack Web API root.
	DefaultBaseURL = "https://slack.com/api/"
	// RateLimitRetries is how many times a 429 is retried before giving up.
	RateLimitRetries = 3
)

// Client calls the Slack Web API with one token.
type Client struct {
	Token   string
	BaseURL string
	HTTP    *http.Client
	// Sleep waits between rate-limit retries and returns early (with the
	// context's error) when ctx is cancelled; tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
	// TrustedHost decides which hosts may receive the token on file
	// downloads. Defaults to Slack-owned hosts only.
	TrustedHost func(host string) bool
}

// New returns a client with production defaults.
func New(token string) *Client {
	return &Client{
		Token:   token,
		BaseURL: DefaultBaseURL,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		Sleep:   sleepCtx,
	}
}

// APIError is Slack answering ok:false. Most codes are permanent
// (invalid_auth, channel_not_found); Transient reports the ones worth a retry.
type APIError struct {
	Method string
	Code   string
}

func (e *APIError) Error() string { return fmt.Sprintf("slack %s: %s", e.Method, e.Code) }

// RateLimitError is a 429 that outlasted RateLimitRetries.
type RateLimitError struct {
	Method     string
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("slack %s: rate limited (retry after %s)", e.Method, e.RetryAfter)
}

// HTTPError is a non-2xx, non-429 response.
type HTTPError struct {
	Method string
	Status int
}

func (e *HTTPError) Error() string { return fmt.Sprintf("slack %s: HTTP %d", e.Method, e.Status) }

var transientCodes = map[string]bool{
	"ratelimited": true, "internal_error": true, "fatal_error": true,
	"service_unavailable": true, "request_timeout": true,
}

// IsTransient reports whether err is worth retrying later: network failures,
// 5xx, exhausted rate limits, and Slack's own transient error codes. Auth and
// not-found errors are permanent and should fail fast.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return transientCodes[apiErr.Code]
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Status >= 500
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	return true // network errors, timeouts, rate limits
}

type envelope struct {
	OK               bool   `json:"ok"`
	Error            string `json:"error"`
	ResponseMetadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

// Call POSTs params to method and decodes the JSON response into out (which
// may be nil). It returns the pagination cursor for list methods.
func (c *Client) Call(ctx context.Context, method string, params url.Values, out any) (string, error) {
	if params == nil {
		params = url.Values{}
	}
	body := params.Encode()
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL()+method, strings.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer "+c.Token)
		resp, err := c.httpClient().Do(req)
		if err != nil {
			return "", fmt.Errorf("slack %s: %w", method, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			wait := retryAfter(resp.Header.Get("Retry-After"))
			if attempt >= RateLimitRetries {
				return "", &RateLimitError{Method: method, RetryAfter: wait}
			}
			if err := c.sleep(ctx, wait); err != nil {
				return "", err
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return "", &HTTPError{Method: method, Status: resp.StatusCode}
		}
		if readErr != nil {
			return "", fmt.Errorf("slack %s: %w", method, readErr)
		}
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			return "", fmt.Errorf("slack %s: bad JSON: %w", method, err)
		}
		if !env.OK {
			code := env.Error
			if code == "" {
				code = "unknown_error"
			}
			return "", &APIError{Method: method, Code: code}
		}
		if out != nil {
			if err := json.Unmarshal(data, out); err != nil {
				return "", fmt.Errorf("slack %s: bad JSON: %w", method, err)
			}
		}
		return env.ResponseMetadata.NextCursor, nil
	}
}

// retryAfter honours the advertised delay, clamped to 1..60 seconds.
func retryAfter(header string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || secs < 1 {
		secs = 1
	}
	if secs > 60 {
		secs = 60
	}
	return time.Duration(secs) * time.Second
}

func (c *Client) baseURL() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	if !strings.HasSuffix(c.BaseURL, "/") {
		return c.BaseURL + "/"
	}
	return c.BaseURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP == nil {
		return http.DefaultClient
	}
	return c.HTTP
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep == nil {
		return sleepCtx(ctx, d)
	}
	return c.Sleep(ctx, d)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

// Message is a Slack message as conversations.replies/history return it.
type Message struct {
	TS         string `json:"ts"`
	ThreadTS   string `json:"thread_ts"`
	User       string `json:"user"`
	Username   string `json:"username"`
	Text       string `json:"text"`
	BotProfile *struct {
		Name string `json:"name"`
	} `json:"bot_profile"`
	Edited      json.RawMessage `json:"edited"`
	Reactions   []Reaction      `json:"reactions"`
	Files       []File          `json:"files"`
	Blocks      []Block         `json:"blocks"`
	Attachments []Attachment    `json:"attachments"`
}

// IsEdited reports whether Slack marked the message as edited.
func (m Message) IsEdited() bool {
	s := strings.TrimSpace(string(m.Edited))
	return s != "" && s != "null"
}

type Reaction struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type File struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Mimetype           string `json:"mimetype"`
	Size               int64  `json:"size"`
	URLPrivate         string `json:"url_private"`
	URLPrivateDownload string `json:"url_private_download"`
}

type Block struct {
	Type     string            `json:"type"`
	Text     *TextObject       `json:"text"`
	Elements []json.RawMessage `json:"elements"`
}

type TextObject struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type Attachment struct {
	Text     string `json:"text"`
	Fallback string `json:"fallback"`
}

type Member struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
	IsBot   bool   `json:"is_bot"`
	Profile struct {
		RealName    string `json:"real_name"`
		DisplayName string `json:"display_name"`
		Email       string `json:"email"`
	} `json:"profile"`
}

type Channel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	IsIM bool   `json:"is_im"`
	User string `json:"user"`
}

type AuthInfo struct {
	Team   string `json:"team"`
	URL    string `json:"url"`
	User   string `json:"user"`
	UserID string `json:"user_id"`
}

// ---------------------------------------------------------------------------
// Methods
// ---------------------------------------------------------------------------

func (c *Client) AuthTest(ctx context.Context) (AuthInfo, error) {
	var out AuthInfo
	_, err := c.Call(ctx, "auth.test", nil, &out)
	return out, err
}

// Replies returns a thread's messages (all pages). With oldest set, only
// messages after it are requested (Slack's oldest is exclusive), so a poll
// does not refetch the whole thread; callers still filter, since the root
// may be included either way.
func (c *Client) Replies(ctx context.Context, channel, threadTS, oldest string) ([]Message, error) {
	var all []Message
	cursor := ""
	for {
		params := url.Values{"channel": {channel}, "ts": {threadTS}, "limit": {"200"}}
		if oldest != "" {
			params.Set("oldest", oldest)
		}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var page struct {
			Messages []Message `json:"messages"`
		}
		next, err := c.Call(ctx, "conversations.replies", params, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Messages...)
		if next == "" {
			return all, nil
		}
		cursor = next
	}
}

// History returns top-level channel messages. With oldest set it paginates
// the whole window so incremental catch-up never silently drops messages;
// without it, one page of the newest `limit` messages.
func (c *Client) History(ctx context.Context, channel, oldest string, limit int) ([]Message, error) {
	var all []Message
	cursor := ""
	for {
		params := url.Values{"channel": {channel}}
		if oldest != "" {
			params.Set("oldest", oldest)
			params.Set("limit", "200")
		} else {
			params.Set("limit", strconv.Itoa(limit))
		}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var page struct {
			Messages []Message `json:"messages"`
		}
		next, err := c.Call(ctx, "conversations.history", params, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Messages...)
		if oldest == "" || next == "" {
			return all, nil
		}
		cursor = next
	}
}

func (c *Client) ChannelInfo(ctx context.Context, channel string) (Channel, error) {
	var out struct {
		Channel Channel `json:"channel"`
	}
	_, err := c.Call(ctx, "conversations.info", url.Values{"channel": {channel}}, &out)
	return out.Channel, err
}

// Users returns every workspace member (all pages).
func (c *Client) Users(ctx context.Context) ([]Member, error) {
	var all []Member
	cursor := ""
	for {
		params := url.Values{"limit": {"200"}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var page struct {
			Members []Member `json:"members"`
		}
		next, err := c.Call(ctx, "users.list", params, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Members...)
		if next == "" {
			return all, nil
		}
		cursor = next
	}
}

// UserInfo looks up one member (users.info is far cheaper than paging
// users.list just to name a message's author).
func (c *Client) UserInfo(ctx context.Context, id string) (Member, error) {
	var out struct {
		User Member `json:"user"`
	}
	_, err := c.Call(ctx, "users.info", url.Values{"user": {id}}, &out)
	return out.User, err
}

// PostParams are the chat.postMessage fields perritite uses.
type PostParams struct {
	Channel   string
	Text      string
	ThreadTS  string
	Broadcast bool
	AsUser    bool
}

func (c *Client) PostMessage(ctx context.Context, p PostParams) (channel, ts string, err error) {
	params := url.Values{"channel": {p.Channel}, "text": {p.Text}, "unfurl_links": {"false"}}
	if p.AsUser {
		params.Set("as_user", "true")
	}
	if p.ThreadTS != "" {
		params.Set("thread_ts", p.ThreadTS)
		if p.Broadcast {
			params.Set("reply_broadcast", "true")
		}
	}
	var out struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	if _, err := c.Call(ctx, "chat.postMessage", params, &out); err != nil {
		return "", "", err
	}
	return out.Channel, out.TS, nil
}

func (c *Client) Permalink(ctx context.Context, channel, ts string) (string, error) {
	var out struct {
		Permalink string `json:"permalink"`
	}
	_, err := c.Call(ctx, "chat.getPermalink", url.Values{"channel": {channel}, "message_ts": {ts}}, &out)
	return out.Permalink, err
}

// Upload is one file for UploadFiles.
type Upload struct {
	Name    string
	Content []byte
}

// UploadFiles uploads files into a channel/thread as ONE message with comment
// as its text, using Slack's external-upload flow (getUploadURLExternal, POST
// the bytes, completeUploadExternal). Returns the file ids.
func (c *Client) UploadFiles(ctx context.Context, channel, threadTS, comment string, files []Upload) ([]string, error) {
	type ref struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	refs := make([]ref, 0, len(files))
	for _, f := range files {
		var slot struct {
			UploadURL string `json:"upload_url"`
			FileID    string `json:"file_id"`
		}
		params := url.Values{"filename": {f.Name}, "length": {strconv.Itoa(len(f.Content))}}
		if _, err := c.Call(ctx, "files.getUploadURLExternal", params, &slot); err != nil {
			return nil, err
		}
		if err := c.putUpload(ctx, slot.UploadURL, f.Content); err != nil {
			return nil, err
		}
		refs = append(refs, ref{ID: slot.FileID, Title: f.Name})
	}
	encoded, err := json.Marshal(refs)
	if err != nil {
		return nil, err
	}
	params := url.Values{"files": {string(encoded)}, "channel_id": {channel}}
	if threadTS != "" {
		params.Set("thread_ts", threadTS)
	}
	if comment != "" {
		params.Set("initial_comment", comment)
	}
	if _, err := c.Call(ctx, "files.completeUploadExternal", params, nil); err != nil {
		return nil, err
	}
	ids := make([]string, len(refs))
	for i, r := range refs {
		ids[i] = r.ID
	}
	return ids, nil
}

// putUpload sends the bytes to the pre-signed upload URL. The URL carries
// its own authorization, so the token is not sent.
func (c *Client) putUpload(ctx context.Context, uploadURL string, content []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(content))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("slack upload: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &HTTPError{Method: "upload", Status: resp.StatusCode}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Downloads
// ---------------------------------------------------------------------------

// MaxRedirects bounds download redirect chains.
const MaxRedirects = 4

// SlackHost reports whether host is Slack-owned (slack.com, slack-files.com
// and their subdomains), the only hosts that may see the token.
func SlackHost(host string) bool {
	h := strings.ToLower(host)
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	for _, root := range []string{"slack.com", "slack-files.com"} {
		if h == root || strings.HasSuffix(h, "."+root) {
			return true
		}
	}
	return false
}

// Download fetches a private file. Redirects are followed by hand so the
// token is presented only to trusted hosts, never to a signed CDN URL.
func (c *Client) Download(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	// By default the token goes only to Slack hosts over https. A custom
	// TrustedHost (tests) decides on its own.
	trusted := func(u *url.URL) bool { return u.Scheme == "https" && SlackHost(u.Host) }
	if c.TrustedHost != nil {
		trusted = func(u *url.URL) bool { return c.TrustedHost(u.Host) }
	}
	base := c.httpClient()
	client := &http.Client{
		Timeout:       base.Timeout,
		Transport:     base.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	current := rawURL
	for hop := 0; hop <= MaxRedirects; hop++ {
		u, err := url.Parse(current)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		if trusted(u) {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
		resp, err := client.Do(req)
		if err != nil {
			// *url.Error embeds the URL, which may be a signed CDN link.
			var uerr *url.Error
			if errors.As(err, &uerr) {
				err = uerr.Err
			}
			return nil, err
		}
		if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
			loc := resp.Header.Get("Location")
			resp.Body.Close()
			next, err := u.Parse(loc)
			if err != nil || loc == "" {
				return nil, fmt.Errorf("bad redirect from %s", u.Host)
			}
			current = next.String()
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, fmt.Errorf("download failed (HTTP %d)", resp.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > maxBytes {
			return nil, fmt.Errorf("download exceeded %d bytes", maxBytes)
		}
		return data, nil
	}
	return nil, errors.New("too many redirects")
}
