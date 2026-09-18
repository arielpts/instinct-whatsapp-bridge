// Package reply decides whether an email may become a WhatsApp message.
//
// Every check that stands between a mailbox and someone's phone lives here,
// and all of them fail closed.
package reply

import (
	"errors"
	"fmt"
	"strings"

	"github.com/arielpts/instinct-whatsapp-bridge/internal/config"
	"github.com/arielpts/instinct-whatsapp-bridge/internal/mail"
	"github.com/arielpts/instinct-whatsapp-bridge/internal/mailbody"
	"github.com/arielpts/instinct-whatsapp-bridge/internal/token"
)

var (
	ErrNotAllowlisted = errors.New("reply: sender is not on the allowlist")
	ErrUntrustedStamp = errors.New("reply: authentication results are not from our provider")
	ErrDKIM           = errors.New("reply: DKIM did not pass for an allow-listed domain")
	ErrNoToken        = errors.New("reply: no binding token")
	ErrUnknownChat    = errors.New("reply: recipient is not an allow-listed conversation")
	ErrNotDraftable   = errors.New("reply: conversation does not grant draft")
	ErrDisagree       = errors.New("reply: token and recipient name different conversations")
)

// Verified is a reply that passed everything except token redemption, which
// needs the store and is the caller's to do.
type Verified struct {
	Token        string              // presented, not yet redeemed
	Conversation config.Conversation // resolved from the envelope recipient
	Bubbles      []string
}

// Verify applies SEC-2, SEC-13 and SEC-14 to a parsed message.
//
// The order matters: identity before content. Nothing reads the body until the
// sender has been established, so a message from anyone else is discarded
// without its words ever being parsed.
func Verify(cfg *config.Config, r *mail.Received) (*Verified, error) {
	if !cfg.AcceptsInboundMail() {
		return nil, fmt.Errorf("%w: the allowlist is empty", ErrNotAllowlisted)
	}

	// SEC-14, in three parts. The From address, the provider that vouched for
	// it, and what that provider actually said.
	if !containsFold(cfg.Instinct.FromAddresses, r.From) {
		return nil, fmt.Errorf("%w: %s", ErrNotAllowlisted, r.From)
	}
	if !cfg.TrustsAuthserv(r.AuthservID) {
		return nil, fmt.Errorf("%w: stamped by %q", ErrUntrustedStamp, r.AuthservID)
	}
	if r.Verdict("dkim") != "pass" {
		return nil, fmt.Errorf("%w: dkim=%q", ErrDKIM, r.Verdict("dkim"))
	}
	if !containsFold(cfg.Instinct.DKIMDomains, r.DKIMDomain()) {
		return nil, fmt.Errorf("%w: d=%s", ErrDKIM, r.DKIMDomain())
	}

	// SEC-13, first channel: the address the mail was delivered for.
	local, domain, ok := strings.Cut(r.EnvelopeTo, "@")
	if !ok || !strings.EqualFold(domain, cfg.MailDomain) {
		return nil, fmt.Errorf("%w: %s", ErrUnknownChat, r.EnvelopeTo)
	}
	conv, ok := cfg.Lookup(local)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownChat, r.EnvelopeTo)
	}
	if !conv.Can(config.ActionDraft) {
		return nil, fmt.Errorf("%w: %s", ErrNotDraftable, conv.Label)
	}

	// SEC-13, second channel: the token the reply echoes.
	tok, ok := token.FromSubject(r.Subject)
	if !ok {
		tok, ok = token.FromBody(r.Text)
	}
	if !ok {
		return nil, ErrNoToken
	}

	// SEC-12: what actually gets sent.
	text, err := mailbody.Extract(r.Text)
	if err != nil {
		return nil, err
	}
	if err := mailbody.ScanLeaks(text, cfg.MailDomain); err != nil {
		return nil, err
	}
	bubbles, err := mailbody.SplitBubbles(text, cfg.Limits.BubblesPerCandidate, cfg.Limits.CharsPerBubble)
	if err != nil {
		return nil, err
	}

	return &Verified{Token: tok, Conversation: conv, Bubbles: bubbles}, nil
}

// Agree completes SEC-13 once the token has been redeemed: the conversation it
// was issued for must be the one the address resolved to.
//
// Two independent failures are needed to reach the wrong person, which is the
// whole point of carrying the destination twice.
func (v *Verified) Agree(binding token.Binding, cfg *config.Config) error {
	bound, ok := cfg.Lookup(binding.Conversation)
	if !ok {
		return fmt.Errorf("%w: token names %s, which is not allow-listed", ErrDisagree, binding.Conversation)
	}
	if !strings.EqualFold(bound.JID, v.Conversation.JID) {
		return fmt.Errorf("%w: token says %s, address says %s",
			ErrDisagree, bound.Label, v.Conversation.Label)
	}
	return nil
}

func containsFold(list []string, want string) bool {
	if want == "" {
		return false
	}
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), want) {
			return true
		}
	}
	return false
}
