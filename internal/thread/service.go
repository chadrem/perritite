package thread

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/chadrem/slack-thread/internal/frame"
	"github.com/chadrem/slack-thread/internal/slack"
)

// MaxDownloadBytes caps attachment downloads.
const MaxDownloadBytes = 25 << 20

// Service reads and writes Slack threads.
type Service struct {
	API *slack.Client
	// DownloadRoot is where attachments land, under <channel>/<thread>/.
	DownloadRoot string
	// AsUser sends as_user=true on posts (needed by legacy and user tokens
	// to post as the token's own identity).
	AsUser   bool
	Location *time.Location

	members []slack.Member
	loaded  bool
	names   map[string]string // id -> display name, misses and failures included
	self    *slack.AuthInfo
}

// Message is a normalized message, the shape `read --json` prints.
type Message struct {
	TS        string           `json:"ts"`
	Time      string           `json:"time"`
	Author    string           `json:"author"`
	UserID    string           `json:"user_id,omitempty"`
	Text      string           `json:"text"`
	Edited    bool             `json:"edited"`
	Reactions []slack.Reaction `json:"reactions"`
	Files     []FileEntry      `json:"files"`
}

// FileEntry is an attachment: Path when downloaded, otherwise Note says why not.
type FileEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Mimetype string `json:"mimetype"`
	Size     int64  `json:"size"`
	Path     string `json:"path,omitempty"`
	Note     string `json:"note,omitempty"`
}

// ReadResult is a thread (or recent channel history) transcript.
type ReadResult struct {
	Channel      string    `json:"channel"`
	ChannelLabel string    `json:"channel_label"`
	ThreadTS     string    `json:"thread_ts,omitempty"`
	DownloadDir  string    `json:"download_dir,omitempty"`
	Messages     []Message `json:"messages"`
}

// ReadOptions narrow a read.
type ReadOptions struct {
	After    string // only messages strictly after this ts
	Limit    int    // 0 = no limit
	Download bool
}

// Read returns the thread oldest-first. A channel-only ref returns recent
// channel history instead (50 messages unless Limit says otherwise).
func (s *Service) Read(ctx context.Context, target string, opts ReadOptions) (ReadResult, error) {
	ref, err := ParseURL(target)
	if err != nil {
		return ReadResult{}, err
	}
	if opts.After != "" && !ValidTS(opts.After) {
		return ReadResult{}, fmt.Errorf("--after must be a dotted Slack ts like 1752241234.567890, not %q", opts.After)
	}
	var raw []slack.Message
	if ref.ThreadTS != "" {
		raw, err = s.API.Replies(ctx, ref.Channel, ref.ThreadTS, opts.After)
	} else {
		limit := opts.Limit
		if limit == 0 {
			limit = 50
		}
		raw, err = s.API.History(ctx, ref.Channel, opts.After, limit)
	}
	if err != nil {
		return ReadResult{}, err
	}
	raw = SortUnique(raw)
	if opts.After != "" {
		raw = Newer(raw, opts.After)
	}
	if opts.Limit > 0 && len(raw) > opts.Limit {
		// With an After cursor take the oldest N (no silent gap in
		// incremental catch-up); without one, the newest N.
		if opts.After != "" {
			raw = raw[:opts.Limit]
		} else {
			raw = raw[len(raw)-opts.Limit:]
		}
	}
	result := ReadResult{Channel: ref.Channel, ChannelLabel: s.channelLabel(ctx, ref.Channel), ThreadTS: ref.ThreadTS}
	if opts.Download {
		result.DownloadDir = s.downloadDir(ref)
	}
	result.Messages = make([]Message, 0, len(raw))
	for _, m := range raw {
		result.Messages = append(result.Messages, s.Normalize(ctx, m, ref, opts.Download))
	}
	return result, nil
}

// SortUnique drops duplicate timestamps (page-boundary repeats) and sorts
// oldest first.
func SortUnique(msgs []slack.Message) []slack.Message {
	seen := map[string]bool{}
	out := make([]slack.Message, 0, len(msgs))
	for _, m := range msgs {
		if !seen[m.TS] {
			seen[m.TS] = true
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return CompareTS(out[i].TS, out[j].TS) < 0 })
	return out
}

// Newer keeps messages strictly after cursor.
func Newer(msgs []slack.Message, cursor string) []slack.Message {
	var out []slack.Message
	for _, m := range msgs {
		if After(m.TS, cursor) {
			out = append(out, m)
		}
	}
	return out
}

// PostOptions shape a post.
type PostOptions struct {
	Files     []string
	Raw       bool // send verbatim: no escaping, no mention resolution
	Broadcast bool // also show a thread reply in the channel
}

// PostResult is what a post returns.
type PostResult struct {
	Channel   string   `json:"channel"`
	TS        string   `json:"ts,omitempty"`
	Permalink string   `json:"permalink,omitempty"`
	Files     []string `json:"files,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

// Post replies in the thread (or top-of-channel for a channel-only ref).
// With files, they upload as one message with the text as its comment.
func (s *Service) Post(ctx context.Context, target, text string, opts PostOptions) (PostResult, error) {
	ref, err := ParseURL(target)
	if err != nil {
		return PostResult{}, err
	}
	body := text
	var warnings []string
	if !opts.Raw {
		body, warnings, err = s.ResolveMentions(ctx, Escape(strings.TrimRight(text, " \t\r\n")))
		if err != nil {
			return PostResult{}, err
		}
	}
	if strings.TrimSpace(body) == "" && len(opts.Files) == 0 {
		return PostResult{}, errors.New("nothing to post: message and files are both empty")
	}
	if len(opts.Files) > 0 && opts.Broadcast {
		return PostResult{}, errors.New("--broadcast cannot be combined with --file (Slack's upload API has no broadcast)")
	}
	if len(opts.Files) > 0 {
		uploads := make([]slack.Upload, 0, len(opts.Files))
		for _, path := range opts.Files {
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				return PostResult{}, fmt.Errorf("no such file: %s", path)
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return PostResult{}, err
			}
			if len(content) == 0 {
				return PostResult{}, fmt.Errorf("file is empty: %s", path)
			}
			uploads = append(uploads, slack.Upload{Name: filepath.Base(path), Content: content})
		}
		ids, err := s.API.UploadFiles(ctx, ref.Channel, ref.ThreadTS, body, uploads)
		if err != nil {
			return PostResult{}, err
		}
		return PostResult{Channel: ref.Channel, Files: ids, Warnings: warnings}, nil
	}
	channel, ts, err := s.API.PostMessage(ctx, slack.PostParams{
		Channel: ref.Channel, Text: body, ThreadTS: ref.ThreadTS, Broadcast: opts.Broadcast, AsUser: s.AsUser,
	})
	if err != nil {
		return PostResult{}, err
	}
	// Decorative: a post must never look failed because the permalink
	// lookup hit a rate limit or network blip.
	permalink, _ := s.API.Permalink(ctx, channel, ts)
	return PostResult{Channel: channel, TS: ts, Permalink: permalink, Warnings: warnings}, nil
}

// Whoami returns the identity the token posts as (cached per process).
func (s *Service) Whoami(ctx context.Context) (slack.AuthInfo, error) {
	if s.self != nil {
		return *s.self, nil
	}
	info, err := s.API.AuthTest(ctx)
	if err != nil {
		return info, err
	}
	s.self = &info
	return info, nil
}

// SearchUsers matches name, handle, display name or email, case-insensitively.
// Deleted users stay findable (the CLI flags them).
func (s *Service) SearchUsers(ctx context.Context, query string) ([]slack.Member, error) {
	members, err := s.loadMembers(ctx)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(query)
	var out []slack.Member
	for _, m := range members {
		for _, name := range searchableNames(m) {
			if strings.Contains(strings.ToLower(name), q) {
				out = append(out, m)
				break
			}
		}
	}
	return out, nil
}

// SetMembers preloads the member list (tests, and callers that cache it).
func (s *Service) SetMembers(members []slack.Member) {
	s.members = members
	s.loaded = true
}

func (s *Service) loadMembers(ctx context.Context) ([]slack.Member, error) {
	if s.loaded {
		return s.members, nil
	}
	members, err := s.API.Users(ctx)
	if err != nil {
		return nil, err
	}
	s.SetMembers(members)
	return members, nil
}

// matchMembers: an exact (case-insensitive) match on handle, display, real
// name or email wins; otherwise a first-name match, so @dave finds Dave
// Lindenbaum. With prefix (the explicit @{...} form, which errors when
// ambiguous) any name prefix also counts. Deleted users never match.
func (s *Service) matchMembers(ctx context.Context, name string, prefix bool) ([]slack.Member, error) {
	members, err := s.loadMembers(ctx)
	if err != nil {
		return nil, err
	}
	needle := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(name)), "@")
	var active []slack.Member
	for _, m := range members {
		if !m.Deleted {
			active = append(active, m)
		}
	}
	var exact []slack.Member
	for _, m := range active {
		for _, v := range searchableNames(m) {
			if strings.ToLower(v) == needle {
				exact = append(exact, m)
				break
			}
		}
	}
	if len(exact) > 0 {
		return exact, nil
	}
	// No prefix fallback for 1-char needles: @{u}-style tokens (git
	// revision syntax) must fail loudly rather than ping whoever sorts first.
	if len([]rune(needle)) < 2 {
		return nil, nil
	}
	var loose []slack.Member
	for _, m := range active {
		for _, v := range searchableNames(m) {
			lower := strings.ToLower(v)
			fields := strings.Fields(lower)
			if (len(fields) > 0 && fields[0] == needle) || (prefix && strings.HasPrefix(lower, needle)) {
				loose = append(loose, m)
				break
			}
		}
	}
	return loose, nil
}

func searchableNames(m slack.Member) []string {
	var out []string
	for _, v := range []string{m.Name, m.Profile.DisplayName, m.Profile.RealName, m.Profile.Email} {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}

func fullName(m slack.Member) string {
	switch {
	case m.Profile.RealName != "":
		return m.Profile.RealName
	case m.Profile.DisplayName != "":
		return m.Profile.DisplayName
	default:
		return m.Name
	}
}

// memberName names a user id: from the member list if one is already
// loaded (a post with mentions loads it), otherwise via users.info. Misses
// and failures are cached too, so a bad id costs one call per process.
func (s *Service) memberName(ctx context.Context, id string) string {
	if s.names == nil {
		s.names = map[string]string{}
	}
	if name, ok := s.names[id]; ok {
		return name
	}
	name := id
	found := false
	if s.loaded {
		for _, m := range s.members {
			if m.ID == id {
				name, found = fullName(m), true
				break
			}
		}
	}
	if !found { // e.g. a Slack Connect user missing from the member list
		if m, err := s.API.UserInfo(ctx, id); err == nil && m.ID != "" {
			name = fullName(m)
		}
	}
	s.names[id] = name
	return name
}

// Normalize turns a wire message into a Message, downloading attachments
// when download is set.
func (s *Service) Normalize(ctx context.Context, m slack.Message, ref Ref, download bool) Message {
	out := Message{
		TS:        m.TS,
		Time:      s.formatTime(m.TS),
		Author:    s.authorName(ctx, m),
		UserID:    m.User,
		Text:      s.readableText(ctx, m),
		Edited:    m.IsEdited(),
		Reactions: m.Reactions,
		Files:     []FileEntry{},
	}
	if out.Reactions == nil {
		out.Reactions = []slack.Reaction{}
	}
	for _, f := range m.Files {
		out.Files = append(out.Files, s.fileEntry(ctx, f, ref, download))
	}
	return out
}

func (s *Service) formatTime(ts string) string {
	sec, micro := tsKey(ts)
	loc := s.Location
	if loc == nil {
		loc = time.Local
	}
	return time.Unix(sec, micro*1000).In(loc).Format("2006-01-02 15:04:05 MST")
}

func (s *Service) authorName(ctx context.Context, m slack.Message) string {
	switch {
	case m.User != "":
		return s.memberName(ctx, m.User)
	case m.Username != "":
		return m.Username
	case m.BotProfile != nil && m.BotProfile.Name != "":
		return m.BotProfile.Name
	default:
		return "unknown"
	}
}

func (s *Service) channelLabel(ctx context.Context, channel string) string {
	info, err := s.API.ChannelInfo(ctx, channel)
	switch {
	case err != nil:
		return channel
	case info.Name != "":
		return "#" + info.Name
	case info.IsIM && info.User != "":
		return "DM with @" + frame.OneLine(s.memberName(ctx, info.User))
	default:
		return channel
	}
}

func (s *Service) downloadDir(ref Ref) string {
	thread := "channel"
	if ref.ThreadTS != "" {
		thread = strings.ReplaceAll(ref.ThreadTS, ".", "")
	}
	return filepath.Join(s.DownloadRoot, ref.Channel, thread)
}

var unsafeFileChars = regexp.MustCompile(`[^\w.-]+`)

func (s *Service) fileEntry(ctx context.Context, f slack.File, ref Ref, download bool) FileEntry {
	entry := FileEntry{ID: f.ID, Name: f.Name, Mimetype: f.Mimetype, Size: f.Size}
	// url_private_download serves the bytes directly; url_private can 302
	// to an HTML viewer on slack-files.com for some types (html, pdf).
	source := f.URLPrivateDownload
	if source == "" {
		source = f.URLPrivate
	}
	switch {
	case source == "":
		entry.Note = "no downloadable content"
	case !download:
		entry.Note = "download skipped (--no-download)"
	case f.Size > MaxDownloadBytes:
		entry.Note = fmt.Sprintf("download skipped (%d bytes > %d)", f.Size, MaxDownloadBytes)
	default:
		path, err := s.downloadFile(ctx, f, source, ref)
		if err != nil {
			entry.Note = "download failed: " + err.Error()
		} else {
			entry.Path = path
		}
	}
	return entry
}

func (s *Service) downloadFile(ctx context.Context, f slack.File, source string, ref Ref) (string, error) {
	dir := s.downloadDir(ref)
	// Private attachments stay private on shared machines.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	safeID := unsafeFileChars.ReplaceAllString(f.ID, "_")
	path := filepath.Join(dir, safeID+"-"+unsafeFileChars.ReplaceAllString(f.Name, "_"))
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	data, err := s.API.Download(ctx, source, MaxDownloadBytes)
	if err != nil {
		return "", err
	}
	// Stage + rename so an interrupted download can't leave a truncated
	// file that later reads would trust.
	staging := filepath.Join(dir, fmt.Sprintf(".%s.part%d", safeID, os.Getpid()))
	if err := os.WriteFile(staging, data, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(staging, path); err != nil {
		os.Remove(staging)
		return "", err
	}
	return path, nil
}
