package thread_test

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chadrem/slack-thread/internal/slack"
	"github.com/chadrem/slack-thread/internal/slacktest"
	"github.com/chadrem/slack-thread/internal/thread"
)

const threadURL = "https://x.slack.com/archives/C0123ABCDEF/p1752241234567890"

func newService(t *testing.T, fake *slacktest.Server) *thread.Service {
	t.Helper()
	c := slack.New("xoxb-test")
	c.BaseURL = fake.APIURL()
	c.Sleep = func(context.Context, time.Duration) error { return nil }
	host := strings.TrimPrefix(fake.URL, "http://")
	c.TrustedHost = func(h string) bool { return h == host }
	fake.Members(slacktest.DefaultMembers())
	return &thread.Service{API: c, DownloadRoot: t.TempDir(), AsUser: true, Location: time.UTC}
}

// ---------------------------------------------------------------------------
// ParseURL
// ---------------------------------------------------------------------------

func TestParseURLExtractsChannelAndConvertsThePTimestamp(t *testing.T) {
	ref, err := thread.ParseURL(threadURL)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Channel != "C0123ABCDEF" || ref.TS != "1752241234.567890" || ref.ThreadTS != "1752241234.567890" {
		t.Fatalf("ref = %+v", ref)
	}
}

func TestParseURLPrefersThreadTSForReplyLinks(t *testing.T) {
	ref, _ := thread.ParseURL(threadURL + "?thread_ts=1752240000.111222&cid=C0123ABCDEF")
	if ref.TS != "1752241234.567890" || ref.ThreadTS != "1752240000.111222" {
		t.Fatalf("ref = %+v", ref)
	}
}

func TestParseURLPreservesLeadingZerosInTheFraction(t *testing.T) {
	ref, _ := thread.ParseURL("https://x.slack.com/archives/C0123ABCDEF/p1752241234012345")
	if ref.TS != "1752241234.012345" {
		t.Fatalf("ts = %q", ref.TS)
	}
}

func TestParseURLHandlesChannelOnlyAndDMURLs(t *testing.T) {
	ref, err := thread.ParseURL("https://x.slack.com/archives/C0123ABCDEF")
	if err != nil || ref.Channel != "C0123ABCDEF" || ref.TS != "" || ref.ThreadTS != "" {
		t.Fatalf("ref=%+v err=%v", ref, err)
	}
	dm, _ := thread.ParseURL("https://x.slack.com/archives/D024BE91LDM/p1752241234567890")
	if dm.Channel != "D024BE91LDM" {
		t.Fatalf("dm = %+v", dm)
	}
}

func TestParseURLRejectsNonSlackAndMalformedURLs(t *testing.T) {
	for _, bad := range []string{
		"https://evil.com/archives/C0123ABCDEF/p1752241234567890",
		"https://slack.com.evil.io/archives/C0123ABCDEF/p1752241234567890",
		"https://x.slack.com/messages/C0123ABCDEF",
		"https://x.slack.com/archives/notachannel",
		"<https://x.slack.com/archives/C0123ABCDEF/p1752241234567890>",
		"https://x.slack.com/archives/C0123ABCDEF/junk",
	} {
		if _, err := thread.ParseURL(bad); err == nil {
			t.Errorf("ParseURL(%q) should fail", bad)
		}
	}
}

func TestCompareTSIsExactAndPadsFractions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.5", "2.10", 1}, // .500000 > .100000
		{"1752241234.567890", "1752241234.567891", -1},
		{"10.0", "9.999999", 1},
		{"3.000001", "3.000001", 0},
	}
	for _, c := range cases {
		if got := thread.CompareTS(c.a, c.b); got != c.want {
			t.Errorf("CompareTS(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Mentions
// ---------------------------------------------------------------------------

func resolve(t *testing.T, text string) (string, []string, error) {
	t.Helper()
	s := newService(t, slacktest.New(t))
	return s.ResolveMentions(context.Background(), text)
}

func TestStrictMentionResolvesAUniqueFullNameMatch(t *testing.T) {
	out, warnings, err := resolve(t, "ping @{Abby Holtz} please")
	if err != nil || out != "ping <@U2> please" || len(warnings) != 0 {
		t.Fatalf("out=%q warnings=%v err=%v", out, warnings, err)
	}
}

func TestStrictMentionErrorsWithCandidatesWhenAmbiguous(t *testing.T) {
	_, _, err := resolve(t, "ping @{Dav}")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") ||
		!strings.Contains(err.Error(), "Dave Lindenbaum") || !strings.Contains(err.Error(), "Dave Smith") {
		t.Fatalf("err = %v", err)
	}
}

func TestStrictMentionErrorsWhenNothingMatchesAndIgnoresDeletedUsers(t *testing.T) {
	for _, text := range []string{"ping @{Nobody Real}", "ping @{Dave Gone}"} {
		if _, _, err := resolve(t, text); err == nil {
			t.Errorf("%q should fail", text)
		}
	}
}

func TestBareMentionSubstitutesOnlyOnAUniqueMatch(t *testing.T) {
	out, warnings, err := resolve(t, "cc @abby and @dav too")
	if err != nil || out != "cc <@U2> and @dav too" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "@dav") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestBareMentionLeavesEmailAddressesUntouched(t *testing.T) {
	out, warnings, _ := resolve(t, "mail dave@x.com directly")
	if out != "mail dave@x.com directly" || len(warnings) != 0 {
		t.Fatalf("out=%q warnings=%v", out, warnings)
	}
}

func TestHereAndChannelBecomeSpecialMentions(t *testing.T) {
	out, _, _ := resolve(t, "heads up @here and (@channel)")
	if out != "heads up <!here> and (<!channel>)" {
		t.Fatalf("out = %q", out)
	}
}

func TestBareMentionKeepsSentencePunctuationOutsideTheToken(t *testing.T) {
	out, warnings, _ := resolve(t, "thanks @abby.")
	if out != "thanks <@U2>." || len(warnings) != 0 {
		t.Fatalf("out=%q warnings=%v", out, warnings)
	}
}

func TestAdjacentMentionsSeparatedBySingleSpacesAllResolve(t *testing.T) {
	out, _, err := resolve(t, "@{Abby Holtz} @{Dave Smith} @abby")
	if err != nil || out != "<@U2> <@U3> <@U2>" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestGitRevisionSyntaxIsNotSilentlyResolved(t *testing.T) {
	out, warnings, err := resolve(t, "run git stash@{0} now")
	if err != nil || out != "run git stash@{0} now" || len(warnings) != 0 {
		t.Fatalf("out=%q warnings=%v err=%v", out, warnings, err)
	}
	// A standalone @{u} must fail LOUDLY (no 1-char prefix fallback).
	if _, _, err := resolve(t, "compare @{u}..HEAD"); err == nil {
		t.Fatal("@{u} should fail")
	}
}

func TestStrictMentionsMatchNamesContainingEscapedEntities(t *testing.T) {
	out, _, err := resolve(t, "ask @{Q&amp;A Bot} first")
	if err != nil || out != "ask <@U5> first" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

// ---------------------------------------------------------------------------
// Post
// ---------------------------------------------------------------------------

func stubPost(fake *slacktest.Server) {
	fake.OK("chat.postMessage", map[string]any{"channel": "C0123ABCDEF", "ts": "1752241300.000100"})
	fake.OK("chat.getPermalink", map[string]any{"permalink": "https://x.slack.com/archives/C0123ABCDEF/p1752241300000100"})
}

func TestPostEscapesResolvesMentionsAndRepliesInThread(t *testing.T) {
	fake := slacktest.New(t)
	s := newService(t, fake)
	stubPost(fake)

	res, err := s.Post(context.Background(), threadURL, "if x < 3 & y > 2 ask @{Abby Holtz}\n", thread.PostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := fake.Calls("chat.postMessage")[0].Params
	if p.Get("text") != "if x &lt; 3 &amp; y &gt; 2 ask <@U2>" {
		t.Fatalf("text = %q", p.Get("text"))
	}
	if p.Get("thread_ts") != "1752241234.567890" || p.Get("channel") != "C0123ABCDEF" ||
		p.Get("as_user") != "true" || p.Get("unfurl_links") != "false" || p.Has("reply_broadcast") {
		t.Fatalf("params = %v", p)
	}
	if res.TS != "1752241300.000100" || !strings.Contains(res.Permalink, "p1752241300000100") {
		t.Fatalf("res = %+v", res)
	}
}

func TestPostRawSkipsEscapingAndMentionResolution(t *testing.T) {
	fake := slacktest.New(t)
	s := newService(t, fake)
	stubPost(fake)
	if _, err := s.Post(context.Background(), threadURL, "<@U999> & <https://a.b|link>", thread.PostOptions{Raw: true}); err != nil {
		t.Fatal(err)
	}
	if got := fake.Calls("chat.postMessage")[0].Params.Get("text"); got != "<@U999> & <https://a.b|link>" {
		t.Fatalf("text = %q", got)
	}
}

func TestPostTopOfChannelForChannelURLAndBroadcastInThreads(t *testing.T) {
	fake := slacktest.New(t)
	s := newService(t, fake)
	stubPost(fake)
	ctx := context.Background()

	if _, err := s.Post(ctx, "https://x.slack.com/archives/C0123ABCDEF", "top level", thread.PostOptions{Broadcast: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Post(ctx, threadURL, "in thread", thread.PostOptions{Broadcast: true}); err != nil {
		t.Fatal(err)
	}
	calls := fake.Calls("chat.postMessage")
	if calls[0].Params.Has("thread_ts") || calls[0].Params.Has("reply_broadcast") {
		t.Fatalf("top-level params = %v", calls[0].Params)
	}
	if calls[1].Params.Get("thread_ts") != "1752241234.567890" || calls[1].Params.Get("reply_broadcast") != "true" {
		t.Fatalf("thread params = %v", calls[1].Params)
	}
}

func TestPostUploadsFilesWithTheTextAsComment(t *testing.T) {
	fake := slacktest.New(t)
	s := newService(t, fake)
	fake.Raw("/upload/F123", func(http.ResponseWriter, *http.Request) {})
	fake.OK("files.getUploadURLExternal", map[string]any{"upload_url": fake.URL + "/upload/F123", "file_id": "F123"})
	fake.OK("files.completeUploadExternal", nil)
	path := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(path, []byte("PNGBYTES"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := s.Post(context.Background(), threadURL, "see screenshot", thread.PostOptions{Files: []string{path}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0] != "F123" {
		t.Fatalf("files = %v", res.Files)
	}
	p := fake.Calls("files.completeUploadExternal")[0].Params
	if p.Get("thread_ts") != "1752241234.567890" || p.Get("initial_comment") != "see screenshot" {
		t.Fatalf("params = %v", p)
	}
}

func TestPostRejectsAMissingUploadPathAndAnEmptyPost(t *testing.T) {
	s := newService(t, slacktest.New(t))
	ctx := context.Background()
	if _, err := s.Post(ctx, threadURL, "text", thread.PostOptions{Files: []string{"/nope/missing.png"}}); err == nil {
		t.Error("missing file should fail")
	}
	if _, err := s.Post(ctx, threadURL, "   ", thread.PostOptions{}); err == nil {
		t.Error("empty post should fail")
	}
}

// ---------------------------------------------------------------------------
// Read
// ---------------------------------------------------------------------------

func channelInfo(fake *slacktest.Server, name string) {
	fake.OK("conversations.info", map[string]any{"channel": map[string]any{"name": name}})
}

func TestReadNormalizesMessagesAndHumanizesMarkup(t *testing.T) {
	fake := slacktest.New(t)
	s := newService(t, fake)
	channelInfo(fake, "eng-feature")
	fake.On("conversations.replies", slacktest.Pages("messages",
		[]map[string]any{
			slacktest.Msg("1752241234.567890", "U1", "kick off <@U2> see <https://a.b/x|the doc> and <https://plain.url>"),
			{"ts": "1752241300.000100", "user": "U2", "text": "a &lt;tag&gt; &amp; more",
				"reactions": []map[string]any{{"name": "+1", "count": 3, "users": []string{"U1"}}}},
		},
		[]map[string]any{
			slacktest.Msg("1752241300.000100", "U2", "a &lt;tag&gt; &amp; more"),
			{"ts": "1752241400.000200", "username": "GitHub", "text": "",
				"blocks": []map[string]any{{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "from a block"}}}},
			slacktest.Msg("1752241500.000300", "U1", "&amp;lt;b&amp;gt; &amp;amp; ok"),
			{"ts": "1752241600.000400", "bot_profile": map[string]any{"name": "Deploy Bot"}, "text": "",
				"attachments": []map[string]any{{"fallback": "deployed v2"}}},
		}))

	res, err := s.Read(context.Background(), threadURL, thread.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ChannelLabel != "#eng-feature" {
		t.Fatalf("label = %q", res.ChannelLabel)
	}
	if len(res.Messages) != 5 {
		t.Fatalf("got %d messages; the page-boundary duplicate should be dropped", len(res.Messages))
	}
	m := res.Messages
	if m[0].Author != "Dave Lindenbaum" || m[0].Text != "kick off @Abby Holtz see the doc (https://a.b/x) and https://plain.url" {
		t.Errorf("first = %+v", m[0])
	}
	if m[1].Text != "a <tag> & more" || len(m[1].Reactions) != 1 || m[1].Reactions[0].Count != 3 {
		t.Errorf("second = %+v", m[1])
	}
	if m[2].Author != "GitHub" || m[2].Text != "from a block" {
		t.Errorf("third = %+v", m[2])
	}
	// The user literally typed '&lt;b&gt; &amp; ok': decode exactly once.
	if m[3].Text != "&lt;b&gt; &amp; ok" {
		t.Errorf("fourth text = %q", m[3].Text)
	}
	if m[4].Author != "Deploy Bot" || m[4].Text != "deployed v2" {
		t.Errorf("fifth = %+v", m[4])
	}
	if m[0].Time != "2025-07-11 13:40:34 UTC" {
		t.Errorf("time = %q", m[0].Time)
	}
}

func TestReadFiltersWithAfterAndDownloadsFilesOnce(t *testing.T) {
	fake := slacktest.New(t)
	s := newService(t, fake)
	channelInfo(fake, "eng-feature")
	downloads := 0
	fake.Raw("/files/shot.png", func(w http.ResponseWriter, r *http.Request) {
		downloads++
		if r.Header.Get("Authorization") != "Bearer xoxb-test" {
			t.Errorf("download auth = %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte("PNGBYTES"))
	})
	file := map[string]any{"id": "FTEST1", "name": "shot one.png", "mimetype": "image/png", "size": 8,
		"url_private": fake.URL + "/files/viewer", "url_private_download": fake.URL + "/files/shot.png"}
	fake.On("conversations.replies", slacktest.Pages("messages", []map[string]any{
		slacktest.Msg("1752241234.567890", "U1", "old"),
		{"ts": "1752241300.000100", "user": "U2", "text": "new", "files": []map[string]any{file}},
	}))
	ctx := context.Background()
	opts := thread.ReadOptions{After: "1752241234.567890", Download: true}

	res, err := s.Read(ctx, threadURL, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 || res.Messages[0].Text != "new" {
		t.Fatalf("messages = %+v", res.Messages)
	}
	entry := res.Messages[0].Files[0]
	if !strings.HasSuffix(entry.Path, "FTEST1-shot_one.png") || entry.Size != 8 {
		t.Fatalf("entry = %+v", entry)
	}
	if data, _ := os.ReadFile(entry.Path); string(data) != "PNGBYTES" {
		t.Fatalf("file content = %q", data)
	}
	if _, err := s.Read(ctx, threadURL, opts); err != nil {
		t.Fatal(err)
	}
	if downloads != 1 {
		t.Fatalf("downloads = %d; a second read should reuse the file", downloads)
	}
}

func TestReadNotesOversizedUndownloadableAndFailedFiles(t *testing.T) {
	fake := slacktest.New(t)
	s := newService(t, fake)
	channelInfo(fake, "c")
	fake.Raw("/files/err.png", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	fake.On("conversations.replies", slacktest.Pages("messages", []map[string]any{
		{"ts": "1.1", "user": "U1", "text": "files", "files": []map[string]any{
			{"id": "FBIG", "name": "huge.mov", "size": thread.MaxDownloadBytes + 1, "url_private": fake.URL + "/files/huge"},
			{"id": "FGONE", "name": "tombstone", "size": 0},
			{"id": "FERR", "name": "err.png", "size": 5, "url_private": fake.URL + "/files/err.png"},
		}},
	}))
	res, err := s.Read(context.Background(), threadURL, thread.ReadOptions{Download: true})
	if err != nil {
		t.Fatal(err)
	}
	files := res.Messages[0].Files
	if files[0].Path != "" || !strings.Contains(files[0].Note, "skipped") {
		t.Errorf("big = %+v", files[0])
	}
	if files[1].Note != "no downloadable content" {
		t.Errorf("tombstone = %+v", files[1])
	}
	if files[2].Path != "" || !strings.HasPrefix(files[2].Note, "download failed:") {
		t.Errorf("failed = %+v", files[2])
	}
}

func TestAfterWithLimitReturnsTheOldestMatchesSoCatchUpNeverGaps(t *testing.T) {
	fake := slacktest.New(t)
	s := newService(t, fake)
	channelInfo(fake, "c")
	fake.On("conversations.replies", slacktest.Pages("messages", []map[string]any{
		slacktest.Msg("1.0", "U1", "seen"), slacktest.Msg("2.0", "U1", "first-unseen"), slacktest.Msg("3.0", "U2", "second-unseen"),
	}))
	res, _ := s.Read(context.Background(), threadURL, thread.ReadOptions{After: "1.0", Limit: 1})
	if len(res.Messages) != 1 || res.Messages[0].Text != "first-unseen" {
		t.Fatalf("messages = %+v", res.Messages)
	}
}

func TestReadUsesChannelHistoryOldestFirstForChannelRefs(t *testing.T) {
	fake := slacktest.New(t)
	s := newService(t, fake)
	channelInfo(fake, "eng-feature")
	fake.On("conversations.history", slacktest.Pages("messages", []map[string]any{
		slacktest.Msg("1752241300.000100", "U2", "newest"), slacktest.Msg("1752241234.567890", "U1", "older"),
	}))
	res, _ := s.Read(context.Background(), "https://x.slack.com/archives/C0123ABCDEF", thread.ReadOptions{})
	if len(res.Messages) != 2 || res.Messages[0].Text != "older" || res.Messages[1].Text != "newest" {
		t.Fatalf("messages = %+v", res.Messages)
	}
	if fake.Calls("conversations.history")[0].Params.Get("limit") != "50" {
		t.Fatal("default channel read is one page of 50")
	}
}

func TestChannelReadsWithAfterPaginateTheWindowViaOldest(t *testing.T) {
	fake := slacktest.New(t)
	s := newService(t, fake)
	channelInfo(fake, "c")
	fake.On("conversations.history", slacktest.Pages("messages",
		[]map[string]any{slacktest.Msg("3.0", "U1", "c"), slacktest.Msg("2.5", "U2", "b")},
		[]map[string]any{slacktest.Msg("2.1", "U1", "a")}))
	res, _ := s.Read(context.Background(), "https://x.slack.com/archives/C0123ABCDEF", thread.ReadOptions{After: "2.0"})
	var texts []string
	for _, m := range res.Messages {
		texts = append(texts, m.Text)
	}
	if strings.Join(texts, ",") != "a,b,c" {
		t.Fatalf("texts = %v", texts)
	}
	if fake.Calls("conversations.history")[0].Params.Get("oldest") != "2.0" {
		t.Fatal("oldest must be passed through")
	}
}

func TestReadRejectsAMalformedAfterCursor(t *testing.T) {
	s := newService(t, slacktest.New(t))
	if _, err := s.Read(context.Background(), threadURL, thread.ReadOptions{After: "1752241234567890"}); err == nil {
		t.Fatal("p-digits without a dot must be rejected")
	}
}

func TestChannelLabelFallsBackToTheIDAndNamesDMs(t *testing.T) {
	fake := slacktest.New(t)
	s := newService(t, fake)
	fake.On("conversations.replies", slacktest.Pages("messages", []map[string]any{}))
	fake.On("conversations.info", func(url.Values) slacktest.Response {
		return slacktest.Body(map[string]any{"channel": map[string]any{"is_im": true, "user": "U2"}})
	})
	res, _ := s.Read(context.Background(), threadURL, thread.ReadOptions{})
	if res.ChannelLabel != "DM with @Abby Holtz" {
		t.Fatalf("label = %q", res.ChannelLabel)
	}
	fake.On("conversations.info", func(url.Values) slacktest.Response { return slacktest.Err("channel_not_found") })
	res, _ = s.Read(context.Background(), threadURL, thread.ReadOptions{})
	if res.ChannelLabel != "C0123ABCDEF" {
		t.Fatalf("label = %q", res.ChannelLabel)
	}
}

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

func TestSearchUsersMatchesNameHandleAndEmailCaseInsensitively(t *testing.T) {
	s := newService(t, slacktest.New(t))
	ctx := context.Background()
	ids := func(q string) string {
		ms, err := s.SearchUsers(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range ms {
			out = append(out, m.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids("LINDEN"); got != "U1" {
		t.Errorf("LINDEN -> %s", got)
	}
	if got := ids("daves@x.com"); got != "U3" {
		t.Errorf("email -> %s", got)
	}
	if got := ids("dave"); !strings.Contains(got, "U4") {
		t.Errorf("deleted users stay findable: %s", got)
	}
}

// ---------------------------------------------------------------------------
// Review follow-ups
// ---------------------------------------------------------------------------

func TestBareHandlesNeverPrefixMatch(t *testing.T) {
	out, warnings, err := resolve(t, "ping @ab and @dave")
	// @ab is a prefix of abby only: left alone. @dave is a handle/first name: resolved.
	if err != nil || out != "ping @ab and <@U1>" || len(warnings) != 1 {
		t.Fatalf("out=%q warnings=%v err=%v", out, warnings, err)
	}
}

func TestMentionsInsideCodeAreLeftAlone(t *testing.T) {
	out, warnings, err := resolve(t, "see `@channel` and\n```\ngit diff @{u}\nping @abby\n```\nthen @abby")
	if err != nil || len(warnings) != 0 {
		t.Fatalf("err=%v warnings=%v", err, warnings)
	}
	if out != "see `@channel` and\n```\ngit diff @{u}\nping @abby\n```\nthen <@U2>" {
		t.Fatalf("out = %q", out)
	}
}

func TestReadNamesAuthorsWithUsersInfoNotAWorkspaceCrawl(t *testing.T) {
	fake := slacktest.New(t)
	s := newService(t, fake)
	channelInfo(fake, "c")
	fake.On("conversations.replies", slacktest.Pages("messages", []map[string]any{
		slacktest.Msg("1.0", "U1", "a"), slacktest.Msg("2.0", "U1", "b"),
		slacktest.Msg("3.0", "UGHOST", "c"), slacktest.Msg("4.0", "UGHOST", "d"),
	}))
	res, err := s.Read(context.Background(), threadURL, thread.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Messages[0].Author != "Dave Lindenbaum" || res.Messages[2].Author != "UGHOST" {
		t.Fatalf("authors = %q, %q", res.Messages[0].Author, res.Messages[2].Author)
	}
	if n := len(fake.Calls("users.list")); n != 0 {
		t.Fatalf("users.list called %d times; reads must not crawl the workspace", n)
	}
	if n := len(fake.Calls("users.info")); n != 2 {
		t.Fatalf("users.info called %d times, want 2 (hits and misses cached)", n)
	}
}

func TestReadWithAfterAsksSlackOnlyForNewerReplies(t *testing.T) {
	fake := slacktest.New(t)
	s := newService(t, fake)
	channelInfo(fake, "c")
	fake.On("conversations.replies", slacktest.Pages("messages", []map[string]any{}))
	_, _ = s.Read(context.Background(), threadURL, thread.ReadOptions{After: "1752241300.000100"})
	if got := fake.Calls("conversations.replies")[0].Params.Get("oldest"); got != "1752241300.000100" {
		t.Fatalf("oldest = %q", got)
	}
}

func TestPostRejectsBroadcastWithFilesAndEmptyFiles(t *testing.T) {
	s := newService(t, slacktest.New(t))
	dir := t.TempDir()
	full, empty := filepath.Join(dir, "a.png"), filepath.Join(dir, "empty.png")
	_ = os.WriteFile(full, []byte("x"), 0o644)
	_ = os.WriteFile(empty, nil, 0o644)
	ctx := context.Background()
	if _, err := s.Post(ctx, threadURL, "x", thread.PostOptions{Files: []string{full}, Broadcast: true}); err == nil {
		t.Error("--broadcast with --file should be refused")
	}
	if _, err := s.Post(ctx, threadURL, "x", thread.PostOptions{Files: []string{empty}}); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty file: %v", err)
	}
}

func TestParseURLRejectsAMalformedThreadTSParam(t *testing.T) {
	if _, err := thread.ParseURL(threadURL + "?thread_ts=../../etc"); err == nil {
		t.Fatal("want an error")
	}
}

func TestDownloadsArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	fake := slacktest.New(t)
	s := newService(t, fake)
	channelInfo(fake, "c")
	fake.Raw("/f/x.png", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("x")) })
	fake.On("conversations.replies", slacktest.Pages("messages", []map[string]any{
		{"ts": "1.0", "user": "U1", "text": "f", "files": []map[string]any{{"id": "F1", "name": "x.png", "size": 1, "url_private": fake.URL + "/f/x.png"}}},
	}))
	res, _ := s.Read(context.Background(), threadURL, thread.ReadOptions{Download: true})
	info, err := os.Stat(res.Messages[0].Files[0].Path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v err=%v", info.Mode().Perm(), err)
	}
	dirInfo, _ := os.Stat(filepath.Dir(res.Messages[0].Files[0].Path))
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v", dirInfo.Mode().Perm())
	}
}

func TestMentionsInsideOrRightAfterCodeAreLeftAlone(t *testing.T) {
	out, _, err := resolve(t, "run `cc @abby here` then `x`@dave and @abby")
	if err != nil || out != "run `cc @abby here` then `x`@dave and <@U2>" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestAuthorsMissingFromALoadedMemberListFallBackToUsersInfo(t *testing.T) {
	fake := slacktest.New(t)
	s := newService(t, fake)
	channelInfo(fake, "c")
	s.SetMembers(nil) // a list that lacks the author (e.g. Slack Connect)
	fake.On("conversations.replies", slacktest.Pages("messages", []map[string]any{slacktest.Msg("1.0", "U2", "hi")}))
	res, _ := s.Read(context.Background(), threadURL, thread.ReadOptions{})
	if res.Messages[0].Author != "Abby Holtz" {
		t.Fatalf("author = %q", res.Messages[0].Author)
	}
}
