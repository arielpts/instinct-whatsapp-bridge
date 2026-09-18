package config

import (
	"strings"
	"testing"
)

func base() *Config {
	return &Config{
		MailDomain:       "wa.example.com",
		Mailbox:          "bridge@wa.example.com",
		AssistantAddress: "assistant@mail.instinct.com",
		IMAPHost:         "imap.provider.net",
		SMTPHost:         "smtp.provider.net",
		HMACKey:          []byte(strings.Repeat("k", 32)),
		StateDir:         "/var/lib/wa-bridge",
	}
}

func file() File {
	return File{
		Mode: ModeApproveEach,
		Conversations: []Conversation{{
			Number:  "+55 11 98765-4321",
			JID:     "5511987654321@s.whatsapp.net",
			Aliases: []string{"551187654321"},
			Label:   "Marina",
			Actions: []string{ActionRead, ActionDraft, ActionSend},
		}},
	}
}

func TestEnvironmentMayTighten(t *testing.T) {
	c, err := build(base(), file(), string(ModeDraftOnly))
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != ModeDraftOnly {
		t.Errorf("mode = %s, want draft-only", c.Mode)
	}
	// Tightening to draft-only must also strip the send grant, or the mode
	// would be advisory rather than a lock.
	if c.Conversations[0].Can(ActionSend) {
		t.Error("draft-only left a send action in place")
	}
}

func TestEnvironmentMayNotLoosen(t *testing.T) {
	f := file()
	f.Mode = ModeDraftOnly
	_, err := build(base(), f, string(ModeApproveExcept))
	if err == nil {
		t.Fatal("the environment was allowed to enable sending")
	}
	if !strings.Contains(err.Error(), "only tighten") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestIdentityFailsClosed(t *testing.T) {
	cases := map[string]func(*Config){
		"missing domain":           func(c *Config) { c.MailDomain = "" },
		"domain is an address":     func(c *Config) { c.MailDomain = "bridge@wa.example.com" },
		"mailbox off-domain":       func(c *Config) { c.Mailbox = "bridge@elsewhere.net" },
		"assistant not an address": func(c *Config) { c.AssistantAddress = "instinct" },
		"short hmac key":           func(c *Config) { c.HMACKey = []byte("too short") },
		"no state dir":             func(c *Config) { c.StateDir = "" },
		"no imap host":             func(c *Config) { c.IMAPHost = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := base()
			mutate(c)
			if _, err := build(c, file(), ""); err == nil {
				t.Errorf("%s started anyway", name)
			}
		})
	}
}

func TestAliasesCollapseToOneConversation(t *testing.T) {
	c, err := build(base(), file(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, spelling := range []string{
		"5511987654321", "551187654321", "+55 (11) 98765-4321", "5511987654321@c.us",
	} {
		conv, ok := c.Lookup(spelling)
		if !ok {
			t.Errorf("%q resolved to nothing", spelling)
			continue
		}
		if conv.Label != "Marina" {
			t.Errorf("%q resolved to %q", spelling, conv.Label)
		}
	}
}

// Two entries claiming one spelling is an ambiguous recipient, which is the
// failure this whole project exists to prevent.
func TestAliasCollisionRefusesToStart(t *testing.T) {
	f := file()
	f.Conversations = append(f.Conversations, Conversation{
		JID:     "551187654321@s.whatsapp.net",
		Label:   "Someone else",
		Actions: []string{ActionRead},
	})
	_, err := build(base(), f, "")
	if err == nil || !strings.Contains(err.Error(), "claimed by both") {
		t.Fatalf("collision not caught: %v", err)
	}
}

func TestGroupJIDRefusesToStart(t *testing.T) {
	f := file()
	f.Conversations[0].JID = "120363000000000000@g.us"
	if _, err := build(base(), f, ""); err == nil {
		t.Fatal("a group conversation was accepted")
	}
}

func TestSenderAllowlistEmptyMeansAcceptNothing(t *testing.T) {
	c, err := build(base(), file(), "")
	if err != nil {
		t.Fatal(err)
	}
	if c.AcceptsInboundMail() {
		t.Error("an empty allowlist must accept nothing")
	}
	if !strings.Contains(c.Summary(), "ALLOWLIST EMPTY") {
		t.Errorf("startup line hides it: %s", c.Summary())
	}

	f := file()
	f.Instinct = Instinct{
		FromAddresses: []string{"assistant@mail.instinct.com"},
		DKIMDomains:   []string{"mail.instinct.com"},
	}
	c, err = build(base(), f, "")
	if err != nil {
		t.Fatal(err)
	}
	if !c.AcceptsInboundMail() {
		t.Error("a filled allowlist still accepts nothing")
	}
}

func TestLimitDefaults(t *testing.T) {
	c, err := build(base(), file(), "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Limits != defaultLimits {
		t.Errorf("limits = %+v, want %+v", c.Limits, defaultLimits)
	}
	f := file()
	f.Limits.BubblesPerCandidate = 2
	c, err = build(base(), f, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Limits.BubblesPerCandidate != 2 || c.Limits.PerDayTotal != defaultLimits.PerDayTotal {
		t.Errorf("partial limits not merged: %+v", c.Limits)
	}
}

// WhatsApp answers with a LID as readily as a phone JID, and a chat may be
// addressed either way, so both have to reach the same conversation.
func TestLIDAndPhoneJIDResolveToOneConversation(t *testing.T) {
	f := file()
	f.Conversations = []Conversation{{
		Number:  "+55 41 99661-6614",
		JID:     "270565893996711@lid",
		Aliases: []string{"270565893996711", "5541996616614", "554196616614"},
		Label:   "Ariel",
		Actions: []string{ActionRead, ActionDraft},
	}}
	c, err := build(base(), f, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, addressing := range []string{
		"270565893996711@lid",          // as a LID
		"5541996616614@s.whatsapp.net", // with the ninth digit
		"554196616614@s.whatsapp.net",  // without it
		"+55 41 99661-6614",            // as a human writes it
	} {
		conv, ok := c.Lookup(addressing)
		if !ok || conv.Label != "Ariel" {
			t.Errorf("%q resolved to %q, %v", addressing, conv.Label, ok)
		}
		if !c.Allowed(addressing) {
			t.Errorf("%q is not allowed", addressing)
		}
	}
	// The address is built from the number, never from the LID.
	conv, _ := c.Lookup("270565893996711@lid")
	if conv.Number == "" {
		t.Error("conversation carries no number to address mail from")
	}
}
