// Package phone resolves the several ways one WhatsApp account can be written
// down: E.164 with or without punctuation, Brazil's optional ninth digit, and
// the two JID servers in circulation.
//
// Nothing here decides which form is real. It produces candidates for
// IsOnWhatsApp to answer (FR-10); guessing is how you message a stranger.
package phone

import (
	"errors"
	"fmt"
	"strings"
)

const (
	ServerUser   = "s.whatsapp.net" // whatsmeow, multi-device
	ServerLegacy = "c.us"           // whatsapp-web.js, WAHA's WEBJS
	ServerLID    = "lid"
	ServerGroup  = "g.us"
)

var (
	ErrEmpty  = errors.New("phone: no digits")
	ErrGroup  = errors.New("phone: group JIDs are out of scope")
	ErrServer = errors.New("phone: unknown JID server")
)

// JID is a WhatsApp address in canonical form.
type JID struct {
	User   string
	Server string
}

func (j JID) String() string { return j.User + "@" + j.Server }

// IsUser reports whether the JID names an individual rather than a LID.
func (j JID) IsUser() bool { return j.Server == ServerUser }

// Digits keeps only 0-9, discarding +, spaces, parentheses and dashes.
func Digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteByte(byte(r))
		}
	}
	return b.String()
}

// ParseJID accepts either JID server, with or without a device suffix, and
// returns the canonical form. Bare digits are taken as a user JID.
func ParseJID(s string) (JID, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return JID{}, ErrEmpty
	}
	user, server, found := strings.Cut(s, "@")
	if !found {
		d := Digits(s)
		if d == "" {
			return JID{}, ErrEmpty
		}
		return JID{User: d, Server: ServerUser}, nil
	}
	// whatsmeow writes user:device for linked devices; the device is not identity.
	if u, _, ok := strings.Cut(user, ":"); ok {
		user = u
	}
	switch server {
	case ServerGroup:
		return JID{}, ErrGroup
	case ServerLegacy, ServerUser:
		d := Digits(user)
		if d == "" {
			return JID{}, ErrEmpty
		}
		return JID{User: d, Server: ServerUser}, nil
	case ServerLID:
		if user == "" {
			return JID{}, ErrEmpty
		}
		return JID{User: user, Server: ServerLID}, nil
	default:
		return JID{}, fmt.Errorf("%w: %q", ErrServer, server)
	}
}

// Candidates returns the numbers that could name this account, most likely
// first, in E.164 digits.
//
// Brazilian mobiles registered before the ninth-digit rollout are still
// addressed without it, so the same person may be 5511987654321 or
// 551187654321 and only one of those is the real JID. Both are returned.
//
// The ordering follows the widely repeated rule that DDDs up to 30 keep the
// ninth digit and those above drop it. That rule is folklore, and it is used
// here for ordering alone -- never to pick a winner.
func Candidates(input string) ([]string, error) {
	d := Digits(input)
	if d == "" {
		return nil, ErrEmpty
	}
	// Only Brazil has this problem.
	if !strings.HasPrefix(d, "55") {
		return []string{d}, nil
	}
	rest := d[2:]
	if len(rest) != 10 && len(rest) != 11 {
		return []string{d}, nil // not a shape we recognise; pass it through untouched
	}
	ddd, sub := rest[:2], rest[2:]

	var with, without string
	switch len(sub) {
	case 9:
		if sub[0] != '9' {
			return []string{d}, nil // nine digits not starting in 9: not ours to reshape
		}
		with, without = d, "55"+ddd+sub[1:]
	case 8:
		if sub[0] < '6' {
			return []string{d}, nil // 2-5 is a landline; it never had a ninth digit
		}
		with, without = "55"+ddd+"9"+sub, d
	}

	if ddd <= "30" && len(ddd) == 2 {
		return []string{with, without}, nil
	}
	return []string{without, with}, nil
}

// NinthDigitVariant returns the other Brazilian spelling of a number -- the
// one with the ninth digit if it lacks one, or without if it has one -- or
// empty when the number has no such counterpart.
func NinthDigitVariant(input string) string {
	candidates, err := Candidates(input)
	if err != nil || len(candidates) != 2 {
		return ""
	}
	d := Digits(input)
	for _, c := range candidates {
		if c != d {
			return c
		}
	}
	return ""
}
