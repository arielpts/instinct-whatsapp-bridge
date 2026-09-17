// Package mailbody turns a reply email into the text that will actually be
// sent, and splits it into bubbles (SEC-12, FR-12).
//
// An email body is not just what someone typed. Mailers append quoted history
// and signatures, so a bridge that forwards a body wholesale eventually quotes
// a contact's own message back at them with our authenticator attached. Every
// rule here exists to stop that, and ambiguity rejects rather than guesses.
package mailbody

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/arielpts/instinct-whatsapp-bridge/internal/token"
)

const (
	ReplyMarker = "--- reply above this line ---"
	BubbleSep   = "---bubble---"
)

var (
	ErrEmpty       = errors.New("mailbody: nothing left after extraction")
	ErrTooMany     = errors.New("mailbody: too many bubbles")
	ErrTooLong     = errors.New("mailbody: bubble too long")
	ErrEmptyBubble = errors.New("mailbody: empty bubble")
)

// Attribution lines a mailer puts above a quote, in both languages this
// mailbox will see.
var attribution = regexp.MustCompile(`(?i)^\s*(on .*wrote:|em .*escreveu:|.* <[^>]+> wrote:)\s*$`)

// Extract reduces a raw text/plain body to the words the assistant wrote.
func Extract(raw string) (string, error) {
	s := strings.ReplaceAll(raw, "\r\n", "\n")

	// Everything from the reply marker down is ours, not theirs.
	if i := strings.Index(s, ReplyMarker); i >= 0 {
		s = s[:i]
	}
	s = token.StripLeadingLine(s)

	var kept []string
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimRight(line, " \t")
		// Signature delimiter: everything below is a signature.
		if trimmed == "--" || trimmed == "-- " || strings.TrimSpace(trimmed) == "--" {
			break
		}
		if strings.HasPrefix(strings.TrimSpace(line), ">") {
			continue
		}
		if attribution.MatchString(line) {
			continue
		}
		kept = append(kept, trimmed)
	}

	out := strings.TrimSpace(strings.Join(kept, "\n"))
	if out == "" {
		return "", ErrEmpty
	}
	return out, nil
}

// SplitBubbles divides extracted text on its own separator lines. No separator
// means a single bubble. Exceeding a cap rejects the whole candidate rather
// than truncating it -- a truncated message is worse than none.
func SplitBubbles(text string, maxCount, maxChars int) ([]string, error) {
	var parts []string
	var cur []string
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == BubbleSep {
			parts = append(parts, strings.Join(cur, "\n"))
			cur = nil
			continue
		}
		cur = append(cur, line)
	}
	parts = append(parts, strings.Join(cur, "\n"))

	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			// A leading separator is harmless; an empty bubble between two is not.
			if len(out) == 0 && len(parts) > 1 {
				continue
			}
			return nil, ErrEmptyBubble
		}
		if maxChars > 0 && len([]rune(p)) > maxChars {
			return nil, fmt.Errorf("%w: %d > %d", ErrTooLong, len([]rune(p)), maxChars)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, ErrEmpty
	}
	if maxCount > 0 && len(out) > maxCount {
		return nil, fmt.Errorf("%w: %d > %d", ErrTooMany, len(out), maxCount)
	}
	return out, nil
}

// ScanLeaks rejects text carrying our own identifiers. Quoting a forward back
// at the contact would hand them the authenticator that signs the next one.
func ScanLeaks(text, mailDomain string) error {
	if t := token.FindAll(text); len(t) > 0 {
		return fmt.Errorf("mailbody: text carries our token %q", t[0])
	}
	if mailDomain != "" && strings.Contains(strings.ToLower(text), "@"+strings.ToLower(mailDomain)) {
		return fmt.Errorf("mailbody: text carries a bridge address at %s", mailDomain)
	}
	for _, needle := range []string{"x-wa-", "message-id:", "in-reply-to:"} {
		if strings.Contains(strings.ToLower(text), needle) {
			return fmt.Errorf("mailbody: text carries header %q", needle)
		}
	}
	return nil
}
