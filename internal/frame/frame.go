// Package frame keeps thread-authored text from impersonating perritite's
// own output. Protocol and header lines start at column 0; every line of
// content is indented with a fixed prefix, and anything a reader might take
// as a line break is treated as one, so content can never begin a line.
package frame

import (
	"regexp"
	"strings"
)

var (
	// \n alone is not enough: \r, vertical tab, form feed, NEL and the
	// Unicode line/paragraph separators all read as new lines to someone.
	lineBreaks = regexp.MustCompile(`\r\n|[\n\r\v\f\x{85}\x{2028}\x{2029}]`)
	// Other C0 controls and DEL (ANSI escapes and the like) are dropped.
	controlChars = regexp.MustCompile(`[\x00-\x08\x0e-\x1f\x7f]`)
	unsafeAuthor = regexp.MustCompile(`[^\p{L}\p{M}\p{N} .'_-]`)
)

// Lines splits text on every kind of line break, after dropping control
// characters.
func Lines(text string) []string {
	return lineBreaks.Split(controlChars.ReplaceAllString(text, ""), -1)
}

// Indent prefixes every line of text with prefix.
func Indent(prefix, text string) string {
	lines := Lines(text)
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// Quote is the watch protocol's content framing: "  | " on every line.
func Quote(text string) string { return Indent("  | ", text) }

// SafeAuthor strips a display name to letters, marks, digits, spaces and
// . ' _ - so it cannot carry a key=value token or a separator.
func SafeAuthor(name string) string { return unsafeAuthor.ReplaceAllString(name, "") }

// OneLine collapses text to a single line (for error messages).
func OneLine(text string) string {
	return strings.Join(strings.Fields(strings.Join(Lines(text), " ")), " ")
}
