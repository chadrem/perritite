package thread

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/chadrem/slack-thread/internal/slack"
)

// Escape makes text safe for Slack: &, <, > become entities. It runs BEFORE
// mention resolution so the substituted <@U…> markup survives.
func Escape(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}

// Unescape decodes Slack's three entities in a single pass, so
// double-encoded text like &amp;lt; decodes exactly once (to &lt;).
func Unescape(text string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(text)
}

// Go's RE2 has no lookbehind, so "preceded by start, whitespace or (" is a
// captured prefix that the replacement puts back.
var (
	strictMention = regexp.MustCompile(`(^|[\s(])@\{([^{}]+)\}`)
	// Trailing . _ - stay outside the token so "@abby." pings; interior
	// dots still match dotted handles like "@john.smith".
	bareMention = regexp.MustCompile(`(^|[\s(])@([A-Za-z](?:[A-Za-z0-9._-]*[A-Za-z0-9_])?)`)
)

// ResolveMentions turns @{Full Name} (strict: must match exactly one active
// user or it errors) and @handle (best effort: only a unique match is
// substituted, otherwise a warning) into Slack mention markup. @here,
// @channel and @everyone become special mentions. Text must already be
// escaped.
//
// Text inside `code` spans and ```fenced``` blocks is left alone, so pasted
// logs, git revision syntax and the like never become pings.
func (s *Service) ResolveMentions(ctx context.Context, text string) (string, []string, error) {
	var warnings []string
	var b strings.Builder
	for _, seg := range splitCode(text) {
		if seg.code {
			b.WriteString(seg.text)
			continue
		}
		out, w, err := s.resolveProse(ctx, seg.text)
		if err != nil {
			return "", nil, err
		}
		b.WriteString(out)
		warnings = append(warnings, w...)
	}
	return b.String(), warnings, nil
}

type segment struct {
	text string
	code bool
}

// codeSpans matches Slack/Markdown code: fenced blocks first, then inline.
var codeSpans = regexp.MustCompile("(?s)```.*?```|`[^`\n]+`")

func splitCode(text string) []segment {
	var out []segment
	last := 0
	for _, m := range codeSpans.FindAllStringIndex(text, -1) {
		if m[0] > last {
			out = append(out, segment{text: text[last:m[0]]})
		}
		out = append(out, segment{text: text[m[0]:m[1]], code: true})
		last = m[1]
	}
	if last < len(text) {
		out = append(out, segment{text: text[last:]})
	}
	return out
}

func (s *Service) resolveProse(ctx context.Context, text string) (string, []string, error) {
	var resolveErr error
	out := replaceWithPrefix(strictMention, text, func(name string) string {
		if resolveErr != nil {
			return ""
		}
		// The name passed through Escape first; undo it before matching
		// (a display name may contain '&').
		name = Unescape(name)
		matches, err := s.matchMembers(ctx, name, true)
		if err != nil {
			resolveErr = err
			return ""
		}
		switch len(matches) {
		case 1:
			return "<@" + matches[0].ID + ">"
		case 0:
			first := strings.Fields(name)
			hint := name
			if len(first) > 0 {
				hint = first[0]
			}
			resolveErr = fmt.Errorf("no Slack user matches @{%s}; try `slack-thread users %s` (or --raw if this is literal @{...} syntax)", name, hint)
		default:
			names := make([]string, len(matches))
			for i, m := range matches {
				names[i] = fmt.Sprintf("%s (%s)", fullName(m), m.ID)
			}
			resolveErr = fmt.Errorf("ambiguous mention @{%s}: %s", name, strings.Join(names, ", "))
		}
		return ""
	})
	if resolveErr != nil {
		return "", nil, resolveErr
	}

	var warnings []string
	out = replaceWithPrefix(bareMention, out, func(word string) string {
		if resolveErr != nil {
			return "@" + word
		}
		switch strings.ToLower(word) {
		case "here", "channel", "everyone":
			return "<!" + strings.ToLower(word) + ">"
		}
		// Bare handles never prefix-match: @ab must not ping Abby.
		matches, err := s.matchMembers(ctx, word, false)
		if err != nil {
			resolveErr = err
			return "@" + word
		}
		if len(matches) == 1 {
			return "<@" + matches[0].ID + ">"
		}
		warnings = append(warnings, fmt.Sprintf("left '@%s' as plain text (%d user matches)", word, len(matches)))
		return "@" + word
	})
	if resolveErr != nil {
		return "", nil, resolveErr
	}
	return out, warnings, nil
}

// replaceWithPrefix replaces every match of re (group 1 = prefix to keep,
// group 2 = the token) with prefix + fn(token).
func replaceWithPrefix(re *regexp.Regexp, text string, fn func(string) string) string {
	var b strings.Builder
	last := 0
	for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
		b.WriteString(text[last:m[0]])
		b.WriteString(text[m[2]:m[3]])
		b.WriteString(fn(text[m[4]:m[5]]))
		last = m[1]
	}
	b.WriteString(text[last:])
	return b.String()
}

var (
	userMarkup    = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|[^>]*)?>`)
	channelMarkup = regexp.MustCompile(`<#([CDG][A-Z0-9]+)\|([^>]*)>`)
	specialMarkup = regexp.MustCompile(`<!(here|channel|everyone)(?:\|[^>]*)?>`)
	labeledLink   = regexp.MustCompile(`<(https?:[^>|]+)\|([^>]+)>`)
	bareLink      = regexp.MustCompile(`<(https?:[^>]+)>`)
)

// humanize unwraps Slack markup into readable text: mentions become @Name,
// links become "label (url)", entities are decoded.
func (s *Service) humanize(ctx context.Context, text string) string {
	text = userMarkup.ReplaceAllStringFunc(text, func(m string) string {
		return "@" + s.memberName(ctx, userMarkup.FindStringSubmatch(m)[1])
	})
	text = channelMarkup.ReplaceAllStringFunc(text, func(m string) string {
		sub := channelMarkup.FindStringSubmatch(m)
		if sub[2] != "" {
			return "#" + sub[2]
		}
		return "#(" + sub[1] + ")"
	})
	text = specialMarkup.ReplaceAllString(text, "@$1")
	text = labeledLink.ReplaceAllString(text, "$2 ($1)")
	text = bareLink.ReplaceAllString(text, "$1")
	return Unescape(text)
}

// readableText falls back to blocks, then attachments, for text-less bot
// messages.
func (s *Service) readableText(ctx context.Context, m slack.Message) string {
	text := m.Text
	if text == "" {
		text = textFromBlocks(m.Blocks)
	}
	if text == "" {
		text = textFromAttachments(m.Attachments)
	}
	return s.humanize(ctx, text)
}

func textFromBlocks(blocks []slack.Block) string {
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "section", "header":
			if b.Text != nil && b.Text.Text != "" {
				parts = append(parts, b.Text.Text)
			}
		case "context":
			var texts []string
			for _, raw := range b.Elements {
				var el struct {
					Text json.RawMessage `json:"text"`
				}
				var str string
				if json.Unmarshal(raw, &el) == nil && json.Unmarshal(el.Text, &str) == nil && str != "" {
					texts = append(texts, str)
				}
			}
			if len(texts) > 0 {
				parts = append(parts, strings.Join(texts, " "))
			}
		}
	}
	return strings.Join(parts, "\n")
}

func textFromAttachments(attachments []slack.Attachment) string {
	var parts []string
	for _, a := range attachments {
		if a.Text != "" {
			parts = append(parts, a.Text)
		} else if a.Fallback != "" {
			parts = append(parts, a.Fallback)
		}
	}
	return strings.Join(parts, "\n")
}
