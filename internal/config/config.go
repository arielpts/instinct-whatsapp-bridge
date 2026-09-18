// Package config loads the bridge's settings from two sources, split by kind.
//
// The environment carries deployment identity and secrets: which domain we are,
// which mailbox we log into, what signs our tokens. The file carries policy:
// who may be messaged, under what limits, in what mode.
//
// The environment may tighten and never loosen. A systemd drop-in is the
// easiest thing on a box to change by accident and the hardest to notice, so
// nothing in the environment can enable sending, add a conversation, raise a
// quota or widen the sender allowlist.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/arielpts/instinct-whatsapp-bridge/internal/phone"
)

// Mode orders from strictest to most permissive. The order is the point: it is
// what "tighten only" is measured against.
type Mode string

const (
	ModeDraftOnly     Mode = "draft-only"
	ModeApproveEach   Mode = "approve-each"
	ModeApproveExcept Mode = "approve-except"
)

var modeRank = map[Mode]int{ModeDraftOnly: 0, ModeApproveEach: 1, ModeApproveExcept: 2}

func (m Mode) valid() bool { _, ok := modeRank[m]; return ok }

// Sends reports whether this mode can put a message on WhatsApp at all.
func (m Mode) Sends() bool { return m != ModeDraftOnly }

const (
	ActionRead  = "read"
	ActionDraft = "draft"
	ActionSend  = "send"
)

// minKeyLen is a floor, not a recommendation. A short HMAC key makes every
// token forgeable, so a weak one refuses to start.
const minKeyLen = 32

type Conversation struct {
	Number  string   `toml:"number"`
	JID     string   `toml:"jid"`
	Aliases []string `toml:"aliases"`
	Label   string   `toml:"label"`
	Actions []string `toml:"actions"`
}

func (c Conversation) Can(action string) bool {
	for _, a := range c.Actions {
		if a == action {
			return true
		}
	}
	return false
}

// Instinct pins the accepted sender. Empty means accept nothing (SEC-14).
type Instinct struct {
	FromAddresses   []string `toml:"from_addresses"`
	DKIMDomains     []string `toml:"dkim_domains"`
	EnvelopeDomains []string `toml:"envelope_domains"`
	IPRanges        []string `toml:"ip_ranges"`
	// AuthservDomain names our own provider's stamping hosts: the only
	// Authentication-Results we trust, because a sender can write those
	// headers themselves.
	//
	// A domain rather than a hostname, because providers stamp with whichever
	// MX handled the message -- observed as mx13.migadu.com, with no promise
	// the next one is mx13. Pinning the hostname would reject real mail the
	// moment it arrived through a different host.
	AuthservDomain string `toml:"authserv_domain"`
}

type Limits struct {
	PerConversationPerHour int `toml:"per_conversation_per_hour"`
	PerDayTotal            int `toml:"per_day_total"`
	InboundMailPerHour     int `toml:"inbound_mail_per_hour"`
	BubblesPerCandidate    int `toml:"bubbles_per_candidate"`
	CharsPerBubble         int `toml:"chars_per_bubble"`
	BubblePauseMS          int `toml:"bubble_pause_ms"`
}

var defaultLimits = Limits{
	PerConversationPerHour: 5,
	PerDayTotal:            30,
	InboundMailPerHour:     60,
	BubblesPerCandidate:    5,
	CharsPerBubble:         4096,
	BubblePauseMS:          1200,
}

// File is the reviewable policy half.
type File struct {
	Mode          Mode           `toml:"mode"`
	AddressStyle  string         `toml:"address_style"`
	RetentionDays int            `toml:"retention_days"`
	Instinct      Instinct       `toml:"instinct"`
	Limits        Limits         `toml:"limits"`
	Conversations []Conversation `toml:"conversation"`
}

// Config is the validated result. Mode is already the tightened one.
type Config struct {
	MailDomain       string
	Mailbox          string
	AssistantAddress string
	IMAPHost         string
	IMAPUser         string
	IMAPPassword     string
	SMTPHost         string
	SMTPPort         int
	SMTPUser         string
	SMTPPassword     string
	HMACKey          []byte
	StateDir         string

	Mode          Mode
	AddressStyle  string
	RetentionDays int
	Instinct      Instinct
	Limits        Limits
	Conversations []Conversation

	byAlias map[string]int
}

// AcceptsInboundMail reports whether the sender allowlist has been filled in.
// While it is empty nothing is accepted, by design: the allowlist is meant to
// be populated from a real observed message, not guessed at.
func (c *Config) AcceptsInboundMail() bool {
	return len(c.Instinct.FromAddresses) > 0 && len(c.Instinct.DKIMDomains) > 0
}

// Allowed reports whether a chat may be read at all, satisfying wa.Allower.
// Anything not granted is denied, which on an empty allowlist means everything.
func (c *Config) Allowed(conversationJID string) bool {
	conv, ok := c.Lookup(conversationJID)
	return ok && conv.Can(ActionRead)
}

// AllowedJIDs lists the conversations that may be read, for pruning.
func (c *Config) AllowedJIDs() []string {
	out := make([]string, 0, len(c.Conversations))
	for _, conv := range c.Conversations {
		out = append(out, conv.JID)
	}
	return out
}

// TrustsAuthserv reports whether an Authentication-Results stamp came from our
// own provider. Anything else in the message was written by the sender.
func (c *Config) TrustsAuthserv(authservID string) bool {
	d := strings.ToLower(strings.TrimSpace(c.Instinct.AuthservDomain))
	if d == "" {
		return false // unset means trust nothing, as with the rest of SEC-14
	}
	id := strings.ToLower(strings.TrimSpace(authservID))
	return id == d || strings.HasSuffix(id, "."+d)
}

// Lookup resolves any alias of a conversation to its entry (FR-11).
func (c *Config) Lookup(alias string) (Conversation, bool) {
	i, ok := c.byAlias[phone.Digits(alias)]
	if !ok {
		return Conversation{}, false
	}
	return c.Conversations[i], true
}

func env(key string) string { return strings.TrimSpace(os.Getenv(key)) }

// envInt falls back rather than failing: a submission port is a detail with a
// sane default, not a decision worth blocking startup over.
func envInt(key string, fallback int) int {
	if v := env(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

// StateDir is the one setting the operator commands need.
//
// Pairing and status must work before any mail exists: a box that cannot link
// its WhatsApp account until an IMAP host is configured would make the first
// step wait on the last one.
func StateDir() (string, error) {
	dir := env("WA_BRIDGE_STATE_DIR")
	if dir == "" {
		return "", errors.New("config: WA_BRIDGE_STATE_DIR is unset")
	}
	return dir, nil
}

// LoadPolicy reads the policy file and the state directory, without requiring
// the mail settings.
//
// Reading WhatsApp and sending email are separable, and the mail half is the
// part still waiting on DNS. A command that only reads should not be blocked by
// an IMAP host it will never dial.
func LoadPolicy() (*Config, error) {
	c := &Config{
		MailDomain: env("WA_BRIDGE_MAIL_DOMAIN"),
		StateDir:   env("WA_BRIDGE_STATE_DIR"),
	}
	if c.StateDir == "" {
		return nil, errors.New("config: WA_BRIDGE_STATE_DIR is unset")
	}
	f, err := readFile()
	if err != nil {
		return nil, err
	}
	return buildPolicy(c, f, env("WA_BRIDGE_MODE"))
}

func readFile() (File, error) {
	path := env("WA_BRIDGE_CONFIG")
	if path == "" {
		return File{}, errors.New("config: WA_BRIDGE_CONFIG is unset")
	}
	var f File
	if _, err := toml.DecodeFile(path, &f); err != nil {
		return File{}, fmt.Errorf("config: reading %s: %w", path, err)
	}
	return f, nil
}

// Load reads the environment, then the policy file it points at, and returns a
// validated configuration or an explanation of why the bridge will not start.
func Load() (*Config, error) {
	c := &Config{
		MailDomain:       env("WA_BRIDGE_MAIL_DOMAIN"),
		Mailbox:          env("WA_BRIDGE_MAILBOX"),
		AssistantAddress: env("WA_BRIDGE_ASSISTANT_ADDRESS"),
		IMAPHost:         env("WA_BRIDGE_IMAP_HOST"),
		IMAPUser:         env("WA_BRIDGE_IMAP_USER"),
		IMAPPassword:     os.Getenv("WA_BRIDGE_IMAP_PASSWORD"),
		SMTPHost:         env("WA_BRIDGE_SMTP_HOST"),
		SMTPPort:         envInt("WA_BRIDGE_SMTP_PORT", 587),
		SMTPUser:         env("WA_BRIDGE_SMTP_USER"),
		SMTPPassword:     os.Getenv("WA_BRIDGE_SMTP_PASSWORD"),
		HMACKey:          []byte(os.Getenv("WA_BRIDGE_HMAC_KEY")),
		StateDir:         env("WA_BRIDGE_STATE_DIR"),
	}

	f, err := readFile()
	if err != nil {
		return nil, err
	}
	return build(c, f, env("WA_BRIDGE_MODE"))
}

func build(c *Config, f File, modeOverride string) (*Config, error) {
	if !f.Mode.valid() {
		return nil, fmt.Errorf("config: mode %q is not one of draft-only, approve-each, approve-except", f.Mode)
	}
	c.Mode = f.Mode

	// Tighten only. Loosening from the environment is an error rather than a
	// silent no-op, so a misconfiguration is visible at startup.
	if modeOverride != "" {
		m := Mode(modeOverride)
		if !m.valid() {
			return nil, fmt.Errorf("config: WA_BRIDGE_MODE %q is not a mode", modeOverride)
		}
		if modeRank[m] > modeRank[f.Mode] {
			return nil, fmt.Errorf("config: WA_BRIDGE_MODE=%s would loosen %s; the environment may only tighten", m, f.Mode)
		}
		c.Mode = m
	}

	switch f.AddressStyle {
	case "", "number":
		c.AddressStyle = "number"
	case "opaque":
		c.AddressStyle = "opaque"
	default:
		return nil, fmt.Errorf("config: address_style %q is not number or opaque", f.AddressStyle)
	}

	c.RetentionDays = f.RetentionDays
	if c.RetentionDays <= 0 {
		c.RetentionDays = 7
	}
	c.Instinct = f.Instinct
	c.Limits = withDefaults(f.Limits)

	if err := c.validateIdentity(); err != nil {
		return nil, err
	}
	if err := c.indexConversations(f.Conversations); err != nil {
		return nil, err
	}
	return c, nil
}

// buildPolicy is build without the mail half.
func buildPolicy(c *Config, f File, modeOverride string) (*Config, error) {
	full, err := build(&Config{
		MailDomain: "policy.invalid", Mailbox: "x@policy.invalid",
		AssistantAddress: "x@policy.invalid", IMAPHost: "x", SMTPHost: "x",
		HMACKey: make([]byte, minKeyLen), StateDir: c.StateDir,
	}, f, modeOverride)
	if err != nil {
		return nil, err
	}
	full.MailDomain, full.Mailbox, full.AssistantAddress = c.MailDomain, "", ""
	full.IMAPHost, full.SMTPHost, full.HMACKey = "", "", nil
	return full, nil
}

func withDefaults(l Limits) Limits {
	d := defaultLimits
	if l.PerConversationPerHour > 0 {
		d.PerConversationPerHour = l.PerConversationPerHour
	}
	if l.PerDayTotal > 0 {
		d.PerDayTotal = l.PerDayTotal
	}
	if l.InboundMailPerHour > 0 {
		d.InboundMailPerHour = l.InboundMailPerHour
	}
	if l.BubblesPerCandidate > 0 {
		d.BubblesPerCandidate = l.BubblesPerCandidate
	}
	if l.CharsPerBubble > 0 {
		d.CharsPerBubble = l.CharsPerBubble
	}
	if l.BubblePauseMS > 0 {
		d.BubblePauseMS = l.BubblePauseMS
	}
	return d
}

func (c *Config) validateIdentity() error {
	if c.MailDomain == "" || strings.Contains(c.MailDomain, "@") || !strings.Contains(c.MailDomain, ".") {
		return fmt.Errorf("config: WA_BRIDGE_MAIL_DOMAIN %q is not a domain", c.MailDomain)
	}
	if _, domain, ok := strings.Cut(c.Mailbox, "@"); !ok || !strings.EqualFold(domain, c.MailDomain) {
		return fmt.Errorf("config: WA_BRIDGE_MAILBOX %q must be an address at %s", c.Mailbox, c.MailDomain)
	}
	if !strings.Contains(c.AssistantAddress, "@") {
		return fmt.Errorf("config: WA_BRIDGE_ASSISTANT_ADDRESS %q is not an address", c.AssistantAddress)
	}
	if len(c.HMACKey) < minKeyLen {
		return fmt.Errorf("config: WA_BRIDGE_HMAC_KEY is %d bytes, need at least %d", len(c.HMACKey), minKeyLen)
	}
	if c.StateDir == "" {
		return errors.New("config: WA_BRIDGE_STATE_DIR is unset")
	}
	for _, host := range []struct{ name, val string }{
		{"WA_BRIDGE_IMAP_HOST", c.IMAPHost},
		{"WA_BRIDGE_SMTP_HOST", c.SMTPHost},
	} {
		if host.val == "" {
			return fmt.Errorf("config: %s is unset", host.name)
		}
	}
	return nil
}

func (c *Config) indexConversations(in []Conversation) error {
	c.byAlias = make(map[string]int)
	for i, conv := range in {
		jid, err := phone.ParseJID(conv.JID)
		if err != nil {
			return fmt.Errorf("config: conversation %q: jid %q: %w", conv.Label, conv.JID, err)
		}
		conv.JID = jid.String()

		for _, a := range conv.Actions {
			if a != ActionRead && a != ActionDraft && a != ActionSend {
				return fmt.Errorf("config: conversation %q: unknown action %q", conv.Label, a)
			}
		}
		// The mode tightens the grant; it never widens it.
		if !c.Mode.Sends() {
			conv.Actions = without(conv.Actions, ActionSend)
		}

		// Every spelling of the number resolves to this one entry, and no two
		// entries may claim the same spelling -- an ambiguous alias is an
		// ambiguous recipient.
		aliases := append([]string{jid.User}, conv.Aliases...)
		if conv.Number != "" {
			aliases = append(aliases, conv.Number)
		}
		for _, a := range aliases {
			d := phone.Digits(a)
			if d == "" {
				continue
			}
			if j, taken := c.byAlias[d]; taken && j != i {
				return fmt.Errorf("config: alias %s is claimed by both %q and %q", d, in[j].Label, conv.Label)
			}
			c.byAlias[d] = i
		}
		in[i] = conv
	}
	c.Conversations = in
	return nil
}

func without(list []string, drop string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v != drop {
			out = append(out, v)
		}
	}
	return out
}

// Summary is the single startup line. It names the domain and mode so a
// misconfiguration shows up in the first line of the journal rather than in a
// message that went somewhere unexpected.
func (c *Config) Summary() string {
	return fmt.Sprintf("domain=%s mode=%s conversations=%s inbound=%s retention=%dd",
		c.MailDomain, c.Mode, strconv.Itoa(len(c.Conversations)),
		map[bool]string{true: "accepting", false: "ALLOWLIST EMPTY, accepting nothing"}[c.AcceptsInboundMail()],
		c.RetentionDays)
}

// TestOptions builds a configuration in tests without a file or environment.
type TestOptions struct {
	MailDomain    string
	Instinct      Instinct
	Conversations []Conversation
	Mode          Mode
	Limits        Limits
}

// ForTest is exported for tests in other packages, which need a validated
// configuration without inventing an environment for it.
func ForTest(o TestOptions) (*Config, error) {
	mode := o.Mode
	if mode == "" {
		mode = ModeApproveEach
	}
	c := &Config{
		MailDomain: o.MailDomain, Mailbox: "bridge@" + o.MailDomain,
		AssistantAddress: "assistant@example.test",
		IMAPHost:         "imap.example.test", SMTPHost: "smtp.example.test",
		HMACKey: make([]byte, minKeyLen), StateDir: "/tmp",
	}
	return build(c, File{
		Mode: mode, Instinct: o.Instinct,
		Limits: o.Limits, Conversations: o.Conversations,
	}, "")
}
