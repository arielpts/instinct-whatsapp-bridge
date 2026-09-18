package reply

import (
	"errors"
	"strings"
	"testing"

	"github.com/arielpts/instinct-whatsapp-bridge/internal/config"
	"github.com/arielpts/instinct-whatsapp-bridge/internal/mail"
	"github.com/arielpts/instinct-whatsapp-bridge/internal/token"
)

const assistant = "assistant@mail.instinct.test"

// A reply shaped like the real ones: delivered through the catch-all, stamped
// by the provider, token echoed in the subject.
func message(mutate func(*mail.Received)) *mail.Received {
	r := &mail.Received{
		From:       assistant,
		EnvelopeTo: "5541996616614@wa.example.com",
		Subject:    "Re: [wa:abcdefgh23456722] Business",
		AuthservID: "mx13.migadu.test",
		AuthResult: "dkim=pass header.d=mail.instinct.test header.s=x; spf=pass",
		Text:       "Consigo sim.\n---bubble---\nTe mando ate as 18h.\n",
	}
	if mutate != nil {
		mutate(r)
	}
	return r
}

func cfg(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.ForTest(config.TestOptions{
		MailDomain: "wa.example.com",
		Instinct: config.Instinct{
			FromAddresses:  []string{assistant},
			DKIMDomains:    []string{"mail.instinct.test"},
			AuthservDomain: "migadu.test",
		},
		Conversations: []config.Conversation{{
			Number:  "+55 41 99661-6614",
			JID:     "270565893996711@lid",
			Aliases: []string{"270565893996711", "5541996616614", "554196616614"},
			Label:   "Business",
			Actions: []string{config.ActionRead, config.ActionDraft},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestVerifyAcceptsARealReply(t *testing.T) {
	v, err := Verify(cfg(t), message(nil))
	if err != nil {
		t.Fatal(err)
	}
	if v.Token != "abcdefgh23456722" {
		t.Errorf("token = %q", v.Token)
	}
	if v.Conversation.Label != "Business" {
		t.Errorf("conversation = %q", v.Conversation.Label)
	}
	if len(v.Bubbles) != 2 {
		t.Errorf("bubbles = %q", v.Bubbles)
	}
}

func TestVerifyRejections(t *testing.T) {
	cases := map[string]struct {
		mutate func(*mail.Received)
		want   error
	}{
		"a stranger": {
			func(r *mail.Received) { r.From = "someone@elsewhere.test" }, ErrNotAllowlisted},
		"stamped by someone else": {
			func(r *mail.Received) { r.AuthservID = "mx13.attacker.test" }, ErrUntrustedStamp},
		"dkim did not pass": {
			func(r *mail.Received) { r.AuthResult = "dkim=fail header.d=mail.instinct.test" }, ErrDKIM},
		"dkim passed for another domain": {
			func(r *mail.Received) { r.AuthResult = "dkim=pass header.d=attacker.test" }, ErrDKIM},
		"recipient on another domain": {
			func(r *mail.Received) { r.EnvelopeTo = "5541996616614@attacker.test" }, ErrUnknownChat},
		"recipient not allow-listed": {
			func(r *mail.Received) { r.EnvelopeTo = "5500000000000@wa.example.com" }, ErrUnknownChat},
		"no token anywhere": {
			func(r *mail.Received) { r.Subject = "Re: Business"; r.Text = "Consigo sim." }, ErrNoToken},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Verify(cfg(t), message(c.mutate))
			if !errors.Is(err, c.want) {
				t.Errorf("got %v, want %v", err, c.want)
			}
		})
	}
}

// An empty allowlist accepts nothing, which is the state a fresh box is in.
func TestVerifyRefusesWhenTheAllowlistIsEmpty(t *testing.T) {
	c, err := config.ForTest(config.TestOptions{
		MailDomain:    "wa.example.com",
		Conversations: []config.Conversation{{JID: "270565893996711@lid", Label: "x", Actions: []string{config.ActionDraft}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(c, message(nil)); !errors.Is(err, ErrNotAllowlisted) {
		t.Errorf("got %v", err)
	}
}

// Read without draft means the assistant may see the conversation and not
// answer in it.
func TestVerifyHonoursTheDraftGrant(t *testing.T) {
	c, err := config.ForTest(config.TestOptions{
		MailDomain: "wa.example.com",
		Instinct: config.Instinct{
			FromAddresses:  []string{assistant},
			DKIMDomains:    []string{"mail.instinct.test"},
			AuthservDomain: "migadu.test",
		},
		Conversations: []config.Conversation{{
			Number: "+55 41 99661-6614", JID: "270565893996711@lid",
			Aliases: []string{"5541996616614"}, Label: "Business",
			Actions: []string{config.ActionRead},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(c, message(nil)); !errors.Is(err, ErrNotDraftable) {
		t.Errorf("got %v", err)
	}
}

// SEC-13: reaching the wrong person should take two independent failures.
func TestAgreeCatchesADisagreeingToken(t *testing.T) {
	c := cfg(t)
	v, err := Verify(c, message(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Agree(token.Binding{Conversation: "270565893996711@lid"}, c); err != nil {
		t.Errorf("a matching token was rejected: %v", err)
	}
	err = v.Agree(token.Binding{Conversation: "30765505114302@lid"}, c)
	if !errors.Is(err, ErrDisagree) {
		t.Errorf("a token for another conversation was accepted: %v", err)
	}
}

// Quoting our own forward back would hand the contact the authenticator.
func TestVerifyRejectsALeakedIdentifier(t *testing.T) {
	_, err := Verify(cfg(t), message(func(r *mail.Received) {
		r.Text = "Consigo sim. [wa:abcdefgh23456722]"
	}))
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("leaked token not caught: %v", err)
	}
}
