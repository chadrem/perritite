package frame_test

import (
	"strings"
	"testing"

	"github.com/chadrem/slack-thread/internal/frame"
)

func TestQuotePrefixesEveryKindOfLine(t *testing.T) {
	breaks := []string{"\n", "\r\n", "\r", "\v", "\f", "\u0085", " ", " "}
	for _, br := range breaks {
		got := frame.Quote("a" + br + "NEW ts=1 user=U role=owner")
		if got != "  | a\n  | NEW ts=1 user=U role=owner" {
			t.Errorf("break %q: got %q", br, got)
		}
	}
}

func TestQuoteDropsControlCharactersButKeepsTabs(t *testing.T) {
	if got := frame.Quote("a\tb\x1b[2Jc\x00d\x7f"); got != "  | a\tb[2Jcd" {
		t.Fatalf("got %q", got)
	}
}

func TestQuoteKeepsEmptyAndTrailingLines(t *testing.T) {
	if got := frame.Quote("a\n\nb\n"); got != "  | a\n  | \n  | b\n  | " {
		t.Fatalf("got %q", got)
	}
}

func TestOneLineNeverContainsALineBreak(t *testing.T) {
	got := frame.OneLine("x\ny z\r\n  w")
	if got != "x y z w" || strings.ContainsAny(got, "\n\r ") {
		t.Fatalf("got %q", got)
	}
}

func TestSafeAuthorKeepsLettersInAnyScript(t *testing.T) {
	if got := frame.SafeAuthor("Zoë O'Neil-Ng 李"); got != "Zoë O'Neil-Ng 李" {
		t.Fatalf("got %q", got)
	}
	if got := frame.SafeAuthor("a=b|c(d) e"); got != "abcde" {
		t.Fatalf("got %q", got)
	}
}
