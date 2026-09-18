package mail

import (
	"strings"
	"testing"
	"time"
)

func cfg() Config {
	return Config{
		Host: "smtp.example.net", Port: 465,
		User: "bridge@wa.example.com", Password: "x",
		Domain: "wa.example.com", Assistant: "assistant@mail.instinct.com",
	}
}

func fwd() Forward {
	return Forward{
		Number: "5511987654321", DisplayName: "Marina",
		Token: "abcdefgh23456722", MessageID: "m_0192bd4c", HMAC: "9f2ca817e3b4",
		Timestamp: time.Date(2026, 9, 17, 20, 5, 11, 0, time.UTC),
		Body:      "oi, consegue me mandar o contrato ainda hoje?",
	}
}

func TestBuildAddressing(t *testing.T) {
	msg := string(cfg().Build(fwd()))
	for _, want := range []string{
		"From: Marina <5511987654321@wa.example.com>",
		"To: assistant@mail.instinct.com",
		"Subject: [wa:abcdefgh23456722] Marina",
		"Message-ID: <m.m_0192bd4c.9f2ca817e3b4@wa.example.com>",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
}

// The token has to survive in the subject, because the assistant cannot set
// headers and this is what a reply echoes back (FR-13).
func TestSubjectCarriesTheToken(t *testing.T) {
	msg := string(cfg().Build(fwd()))
	subject := headerOf(msg, "Subject")
	if !strings.Contains(subject, "[wa:abcdefgh23456722]") {
		t.Errorf("subject does not carry the token: %q", subject)
	}
}

// The contact's words sit below the marker, so a mailer that quotes the whole
// message still leaves them where extraction discards them (SEC-12).
func TestBodyPutsContactTextBelowTheMarker(t *testing.T) {
	msg := string(cfg().Build(fwd()))
	marker := strings.Index(msg, replyMarker)
	body := strings.Index(msg, "contrato")
	if marker < 0 || body < 0 {
		t.Fatalf("marker or body missing:\n%s", msg)
	}
	if body < marker {
		t.Error("the contact's text is above the reply marker")
	}
}

// Brazilian names are reliably non-ASCII; an unencoded header is a malformed
// message, not a cosmetic problem.
func TestNonASCIIDisplayNameIsEncoded(t *testing.T) {
	f := fwd()
	f.DisplayName = "João Gonçalves"
	msg := string(cfg().Build(f))
	from := headerOf(msg, "From")
	if strings.Contains(from, "João") {
		t.Errorf("From holds raw non-ASCII: %q", from)
	}
	if !strings.Contains(from, "=?utf-8?") {
		t.Errorf("From is not encoded-word: %q", from)
	}
	for _, line := range strings.Split(msg, "\r\n") {
		if line == "" {
			break // end of headers
		}
		for i := 0; i < len(line); i++ {
			if line[i] > 127 {
				t.Fatalf("raw non-ASCII in header line: %q", line)
			}
		}
	}
}

// A body carrying bare newlines produces a malformed message; SMTP wants CRLF.
func TestBodyNewlinesAreNormalised(t *testing.T) {
	f := fwd()
	f.Body = "primeira linha\nsegunda linha"
	msg := string(cfg().Build(f))
	if strings.Contains(strings.ReplaceAll(msg, "\r\n", ""), "\n") {
		t.Error("message contains a bare newline")
	}
}

func headerOf(msg, name string) string {
	for _, line := range strings.Split(msg, "\r\n") {
		if line == "" {
			return ""
		}
		if strings.HasPrefix(line, name+": ") {
			return strings.TrimPrefix(line, name+": ")
		}
	}
	return ""
}
