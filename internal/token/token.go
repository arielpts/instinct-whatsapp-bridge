// Package token issues and verifies the opaque [wa:...] subject tokens that
// bind a reply to a conversation and message (FR-13).
//
// The assistant cannot set arbitrary headers, so the binding has to survive in
// a subject line: short, opaque, and easy to echo. A token carries no phone
// number and no message content -- it means nothing off this box.
package token

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"regexp"
	"strconv"
	"strings"
)

// Lowercase base32 without padding: case-insensitive mailers cannot corrupt it
// and it survives subject rewriting.
var enc = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

var re = regexp.MustCompile(`\[wa:([a-z2-7]{8,32})\]`)

// Binding is what a token stands for. Domain is included because addresses and
// tokens are scoped to a mail domain; carrying one across a domain change is a
// replay (README 9.1).
type Binding struct {
	Conversation string
	Message      string
	Domain       string
	IssuedUnix   int64
}

func (b Binding) canonical() string {
	return strings.Join([]string{b.Conversation, b.Message, b.Domain,
		strconv.FormatInt(b.IssuedUnix, 10)}, "\x00")
}

// Issue derives the token for a binding. Deterministic, so verification is a
// re-derivation rather than a lookup that could be poisoned.
func Issue(key []byte, b Binding) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(b.canonical()))
	return enc.EncodeToString(mac.Sum(nil)[:10])
}

// Format wraps a token for a subject line.
func Format(t string) string { return "[wa:" + t + "]" }

// Verify recomputes the token for the stored binding and compares in constant
// time. A token is never trusted because it was found in a map (SEC-2).
func Verify(key []byte, presented string, b Binding) bool {
	return hmac.Equal([]byte(presented), []byte(Issue(key, b)))
}

// FromSubject returns the token in a subject line, if any.
func FromSubject(subject string) (string, bool) {
	if m := re.FindStringSubmatch(subject); m != nil {
		return m[1], true
	}
	return "", false
}

// FromBody returns the token if the first non-empty line of the body is one.
// A token further down is not a binding -- it is a leak, and ScanLeaks treats
// it as such (SEC-12).
func FromBody(body string) (string, bool) {
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if m := re.FindStringSubmatch(line); m != nil && m[0] == line {
			return m[1], true
		}
		return "", false // the first line that has content is not a token
	}
	return "", false
}

// FindAll returns every token-shaped string in the text, for leak scanning.
func FindAll(text string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}

// StripLeadingLine removes a leading token line from a body, which must never
// reach the contact.
func StripLeadingLine(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if m := re.FindStringSubmatch(t); m != nil && m[0] == t {
			return strings.Join(lines[i+1:], "\n")
		}
		break
	}
	return body
}
