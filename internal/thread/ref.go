// Package thread is slack-thread's core: parse Slack URLs, read threads into
// normalized messages, post with mention resolution, download attachments.
package thread

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Ref identifies a channel, or a thread within it.
type Ref struct {
	Channel  string
	TS       string // the linked message
	ThreadTS string // the thread root ("" for a channel-only link)
}

var (
	channelIDPattern = regexp.MustCompile(`^[CDG][A-Z0-9]{6,}$`)
	pSegmentPattern  = regexp.MustCompile(`^p(\d{7,})$`)
	slackHostPattern = regexp.MustCompile(`(?i)(^|\.)slack\.com$`)
	tsPattern        = regexp.MustCompile(`^\d+\.\d+$`)
)

// ParseURL accepts any Slack archives URL:
//
//	https://x.slack.com/archives/C0123456789                    channel only
//	https://x.slack.com/archives/C0123456789/p1752241234567890  message/thread root
//	.../p1752249999000001?thread_ts=1752241234.567890&cid=...   reply link (root in thread_ts)
func ParseURL(raw string) (Ref, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !slackHostPattern.MatchString(u.Hostname()) {
		return Ref{}, fmt.Errorf("not a Slack /archives/ URL: %s", raw)
	}
	var segments []string
	for _, s := range strings.Split(u.Path, "/") {
		if s != "" {
			segments = append(segments, s)
		}
	}
	at := -1
	for i, s := range segments {
		if s == "archives" {
			at = i
			break
		}
	}
	if at < 0 {
		return Ref{}, fmt.Errorf("not a Slack /archives/ URL: %s", raw)
	}
	if at+1 >= len(segments) || !channelIDPattern.MatchString(segments[at+1]) {
		return Ref{}, fmt.Errorf("no channel id in Slack URL: %s", raw)
	}
	ref := Ref{Channel: segments[at+1]}
	if at+2 < len(segments) {
		// A present-but-unparseable segment must not silently downgrade to
		// a channel-only ref: a post would then land top-of-channel.
		m := pSegmentPattern.FindStringSubmatch(segments[at+2])
		if m == nil {
			return Ref{}, fmt.Errorf("unrecognized message reference %q in Slack URL: %s", segments[at+2], raw)
		}
		digits := m[1]
		ref.TS = digits[:len(digits)-6] + "." + digits[len(digits)-6:]
	}
	ref.ThreadTS = ref.TS
	if t := u.Query().Get("thread_ts"); t != "" {
		if !ValidTS(t) {
			return Ref{}, fmt.Errorf("bad thread_ts %q in Slack URL: %s", t, raw)
		}
		ref.ThreadTS = t
	}
	return ref, nil
}

// ValidTS reports whether s is a dotted Slack timestamp like 1752241234.567890.
func ValidTS(s string) bool { return tsPattern.MatchString(s) }

// tsKey turns a Slack ts into an exactly comparable pair. Float parsing
// would do at microsecond resolution today, but integers never round.
func tsKey(ts string) (int64, int64) {
	sec, frac, _ := strings.Cut(ts, ".")
	s, _ := strconv.ParseInt(sec, 10, 64)
	if len(frac) > 6 {
		frac = frac[:6]
	}
	frac += strings.Repeat("0", 6-len(frac))
	f, _ := strconv.ParseInt(frac, 10, 64)
	return s, f
}

// CompareTS orders two Slack timestamps (-1, 0, 1).
func CompareTS(a, b string) int {
	as, af := tsKey(a)
	bs, bf := tsKey(b)
	switch {
	case as < bs || (as == bs && af < bf):
		return -1
	case as == bs && af == bf:
		return 0
	default:
		return 1
	}
}

// After reports whether ts is strictly later than cursor.
func After(ts, cursor string) bool { return CompareTS(ts, cursor) > 0 }
