// Package slacktest is a scriptable fake of the Slack Web API for tests:
// real HTTP over httptest, so the production client code path is exercised
// end to end.
package slacktest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// Call is one recorded API request.
type Call struct {
	Method string
	Params url.Values
	Auth   string
}

// Response is what a handler returns. Body is JSON-encoded; a nil Body means
// {"ok":true}.
type Response struct {
	Status  int
	Body    any
	Headers map[string]string
}

// Handler answers one Web API method.
type Handler func(params url.Values) Response

// Server is a fake Slack.
type Server struct {
	*httptest.Server
	t        testing.TB
	mu       sync.Mutex
	calls    []Call
	handlers map[string]Handler
	raw      map[string]http.HandlerFunc
}

// New starts a fake Slack that is closed when the test ends.
func New(t testing.TB) *Server {
	s := &Server{t: t, handlers: map[string]Handler{}, raw: map[string]http.HandlerFunc{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// APIURL is the base URL to give slack.Client.
func (s *Server) APIURL() string { return s.URL + "/api/" }

// On registers a handler for a Web API method such as "chat.postMessage".
func (s *Server) On(method string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = h
}

// OK registers a fixed successful response; fields are merged with ok:true.
func (s *Server) OK(method string, fields map[string]any) {
	s.On(method, func(url.Values) Response { return Response{Body: okBody(fields)} })
}

// Raw registers a plain HTTP handler for a non-API path (downloads, uploads).
func (s *Server) Raw(path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.raw[path] = h
}

// Calls returns the recorded calls to method (all methods if empty).
func (s *Server) Calls(method string) []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Call
	for _, c := range s.calls {
		if method == "" || c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func okBody(fields map[string]any) map[string]any {
	body := map[string]any{"ok": true}
	for k, v := range fields {
		body[k] = v
	}
	return body
}

// Err builds an ok:false response with a Slack error code.
func Err(code string) Response {
	return Response{Body: map[string]any{"ok": false, "error": code}}
}

// Body builds an ok:true response with fields.
func Body(fields map[string]any) Response { return Response{Body: okBody(fields)} }

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		s.mu.Lock()
		h := s.raw[r.URL.Path]
		s.mu.Unlock()
		if h == nil {
			http.NotFound(w, r)
			return
		}
		h(w, r)
		return
	}
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	data, _ := io.ReadAll(r.Body)
	params, _ := url.ParseQuery(string(data))
	s.mu.Lock()
	s.calls = append(s.calls, Call{Method: method, Params: params, Auth: r.Header.Get("Authorization")})
	h := s.handlers[method]
	s.mu.Unlock()
	if h == nil {
		s.t.Errorf("slacktest: unexpected call to %s", method)
		writeJSON(w, http.StatusOK, nil, map[string]any{"ok": false, "error": "unknown_method"})
		return
	}
	resp := h(params)
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	body := resp.Body
	if body == nil {
		body = map[string]any{"ok": true}
	}
	writeJSON(w, status, resp.Headers, body)
}

func writeJSON(w http.ResponseWriter, status int, headers map[string]string, body any) {
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		panic(fmt.Sprintf("slacktest: encode: %v", err))
	}
}

// Msg is shorthand for a wire message map.
func Msg(ts, user, text string) map[string]any {
	m := map[string]any{"ts": ts, "text": text}
	if user != "" {
		m["user"] = user
	}
	return m
}

// Pages serves list-method pages in order, chaining them with cursors.
func Pages(key string, pages ...[]map[string]any) Handler {
	return func(params url.Values) Response {
		index := 0
		if c := params.Get("cursor"); c != "" {
			fmt.Sscanf(c, "page-%d", &index)
		}
		fields := map[string]any{key: pages[index]}
		if index+1 < len(pages) {
			fields["response_metadata"] = map[string]any{"next_cursor": fmt.Sprintf("page-%d", index+1)}
		}
		return Body(fields)
	}
}

// Members serves members through both users.list (paged once) and
// users.info (by id; unknown ids get user_not_found).
func (s *Server) Members(members []map[string]any) {
	s.On("users.list", Pages("members", members))
	s.On("users.info", func(params url.Values) Response {
		for _, m := range members {
			if m["id"] == params.Get("user") {
				return Body(map[string]any{"user": m})
			}
		}
		return Err("user_not_found")
	})
}

// DefaultMembers is the workspace most tests use.
func DefaultMembers() []map[string]any {
	member := func(id, name, real, display, email string, deleted, bot bool) map[string]any {
		return map[string]any{"id": id, "name": name, "deleted": deleted, "is_bot": bot,
			"profile": map[string]any{"real_name": real, "display_name": display, "email": email}}
	}
	return []map[string]any{
		member("U1", "dave", "Dave Lindenbaum", "dave", "dave@x.com", false, false),
		member("U2", "abby", "Abby Holtz", "", "abby@x.com", false, false),
		member("U3", "dsmith", "Dave Smith", "daves", "daves@x.com", false, false),
		member("U4", "olddave", "Dave Gone", "", "", true, false),
		member("U5", "qabot", "Q&A Bot", "", "", false, true),
		// Starts with "u": a 1-char prefix fallback would make @{u} ping her.
		member("U6", "ursula", "Ursula Major", "", "ursula@x.com", false, false),
	}
}
