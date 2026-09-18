package mail

import (
	"errors"
	"strings"
	"testing"
)

// Headers copied from a real delivery through the catch-all: Gmail to a
// conversation address, rewritten into the bridge mailbox by the provider.
const realDelivery = "Delivered-To: bridge@ariel.example.com\r\n" +
	"Received: from mizu0.migadu.com ([51.38.57.138])\r\n" +
	"\tby soraStorage1.migadu.com with LMTP id nkwjfkrqjf6vv2adayra\r\n" +
	"X-Envelope-To: 5541996616614@ariel.example.com\r\n" +
	"Authentication-Results: mx13.migadu.com;\r\n" +
	"\tdkim=pass header.d=igual.example header.s=google header.b=hJtDcuHz;\r\n" +
	"\tarc=pass (\"google.com:s=arc-20260327:i=1\");\r\n" +
	"\tspf=pass (mx13.migadu.com: domain of ariel@igual.example designates ...) smtp.mailfrom=ariel@igual.example;\r\n" +
	"From: Ariel Patschiki <ariel@igual.example>\r\n" +
	"Message-ID: <CAB4yn=OuKAWWRruRaSKGo@mail.gmail.com>\r\n" +
	"Subject: Re: [wa:abcdefgh23456722] Business (teste)\r\n" +
	"To: 5541996616614@ariel.example.com\r\n" +
	"Content-Type: multipart/alternative; boundary=\"000000000000dd46ea065bb7c062\"\r\n" +
	"\r\n" +
	"--000000000000dd46ea065bb7c062\r\n" +
	"Content-Type: text/plain; charset=\"UTF-8\"\r\n" +
	"\r\n" +
	"Consigo sim.\r\n" +
	"--000000000000dd46ea065bb7c062\r\n" +
	"Content-Type: text/html; charset=\"UTF-8\"\r\n" +
	"\r\n" +
	"<div dir=\"ltr\">Consigo sim.</div>\r\n" +
	"--000000000000dd46ea065bb7c062--\r\n"

// The catch-all rewrites Delivered-To to the mailbox, so only X-Envelope-To
// says which conversation the mail was for. Reading the wrong header would
// route every reply to whichever contact the mailbox is named after.
func TestEnvelopeRecipientSurvivesTheCatchAll(t *testing.T) {
	r, err := Parse([]byte(realDelivery))
	if err != nil {
		t.Fatal(err)
	}
	if r.EnvelopeTo != "5541996616614@ariel.example.com" {
		t.Errorf("EnvelopeTo = %q, want the address the mail was sent to", r.EnvelopeTo)
	}
	if r.From != "ariel@igual.example" {
		t.Errorf("From = %q", r.From)
	}
	if !strings.Contains(r.Subject, "[wa:abcdefgh23456722]") {
		t.Errorf("Subject lost the token: %q", r.Subject)
	}
	if r.Text != "Consigo sim." && r.Text != "Consigo sim.\r\n" {
		t.Errorf("Text = %q, want the text/plain part", r.Text)
	}
}

func TestAuthenticationResults(t *testing.T) {
	r, err := Parse([]byte(realDelivery))
	if err != nil {
		t.Fatal(err)
	}
	if r.AuthservID != "mx13.migadu.com" {
		t.Errorf("AuthservID = %q", r.AuthservID)
	}
	if got := r.Verdict("dkim"); got != "pass" {
		t.Errorf("dkim verdict = %q", got)
	}
	if got := r.Verdict("spf"); got != "pass" {
		t.Errorf("spf verdict = %q", got)
	}
	if got := r.DKIMDomain(); got != "igual.example" {
		t.Errorf("DKIMDomain = %q", got)
	}
}

// A sender can write these headers themselves. The provider prepends its own
// on delivery, so the first occurrence is the trustworthy one.
func TestForgedHeadersBelowTheProvidersAreIgnored(t *testing.T) {
	forged := "Delivered-To: bridge@ariel.example.com\r\n" +
		"X-Envelope-To: 5541996616614@ariel.example.com\r\n" +
		"Authentication-Results: mx13.migadu.com; dkim=pass header.d=igual.example;\r\n" +
		"X-Envelope-To: 5500000000000@ariel.example.com\r\n" +
		"Authentication-Results: evil.test; dkim=pass header.d=attacker.test;\r\n" +
		"From: Someone <someone@elsewhere.test>\r\n" +
		"Subject: hello\r\n" +
		"Content-Type: text/plain\r\n\r\nhi\r\n"
	r, err := Parse([]byte(forged))
	if err != nil {
		t.Fatal(err)
	}
	if r.EnvelopeTo != "5541996616614@ariel.example.com" {
		t.Errorf("a forged X-Envelope-To won: %q", r.EnvelopeTo)
	}
	if r.AuthservID != "mx13.migadu.com" {
		t.Errorf("a forged Authentication-Results won: %q", r.AuthservID)
	}
	if got := r.DKIMDomain(); got != "igual.example" {
		t.Errorf("DKIMDomain came from the forged stamp: %q", got)
	}
}

// SEC-12: what cannot be read plainly is rejected, not converted.
func TestHTMLOnlyIsRejected(t *testing.T) {
	htmlOnly := "X-Envelope-To: 5541996616614@ariel.example.com\r\n" +
		"From: a@b.test\r\nSubject: x\r\n" +
		"Content-Type: text/html; charset=\"UTF-8\"\r\n\r\n<div>oi</div>\r\n"
	if _, err := Parse([]byte(htmlOnly)); !errors.Is(err, ErrNoText) {
		t.Errorf("want ErrNoText, got %v", err)
	}
}

func TestEncodedWordSubjectIsDecoded(t *testing.T) {
	msg := "X-Envelope-To: 5541996616614@ariel.example.com\r\n" +
		"From: a@b.test\r\n" +
		"Subject: =?utf-8?q?=5Bwa=3Aabcdefgh23456722=5D_Jo=C3=A3o?=\r\n" +
		"Content-Type: text/plain\r\n\r\noi\r\n"
	r, err := Parse([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Subject, "[wa:abcdefgh23456722]") || !strings.Contains(r.Subject, "João") {
		t.Errorf("Subject = %q", r.Subject)
	}
}

func TestMissingRecipientIsRejected(t *testing.T) {
	msg := "From: a@b.test\r\nSubject: x\r\nContent-Type: text/plain\r\n\r\noi\r\n"
	if _, err := Parse([]byte(msg)); !errors.Is(err, ErrNoRecipient) {
		t.Errorf("want ErrNoRecipient, got %v", err)
	}
}
