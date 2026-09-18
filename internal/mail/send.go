// Package mail sends the forwards described in README section 7.1.
//
// One inbound WhatsApp message becomes one email. The sender address is the
// conversation, the subject carries the binding token, and the body carries
// the message under a marker that tells the extractor where a reply ends.
package mail

import (
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// Config is the sending half of the mail settings.
type Config struct {
	Host     string // e.g. smtp.provider.net
	Port     int    // 465 implicit TLS, 587 STARTTLS
	User     string
	Password string

	Domain    string // ours; the local part is the conversation
	Assistant string // the only address we send to
}

func (c Config) addr() string { return net.JoinHostPort(c.Host, fmt.Sprint(c.Port)) }

// IsRateLimited reports whether an error is the provider refusing on volume
// rather than on the message.
//
// Retrying into a rate limit every minute is how an account gets flagged for
// abuse. The condition clears with time, not with persistence.
func IsRateLimited(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, marker := range []string{
		"outgoing limits",
		"rate limit",
		"too many messages",
		"4.7.0",
		"5.7.1",
		"452 ",
		"421 ",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// Forward is one message to hand to the assistant.
type Forward struct {
	Number      string // E.164 digits, the conversation's address local part
	DisplayName string // the contact's name, or the number if unknown
	Token       string // [wa:...] binding, from the store
	MessageID   string // opaque, ours
	HMAC        string // signs the Message-ID
	Timestamp   time.Time
	Body        string
}

const replyMarker = "--- reply above this line ---"

// Build renders the message. Kept separate from sending so the format can be
// tested without a network or a mailbox.
func (c Config) Build(f Forward) []byte {
	from := fmt.Sprintf("%s <%s@%s>", encodeWord(f.DisplayName), f.Number, c.Domain)
	subject := fmt.Sprintf("[wa:%s] %s", f.Token, f.DisplayName)

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", c.Assistant)
	fmt.Fprintf(&b, "Subject: %s\r\n", encodeWord(subject))
	fmt.Fprintf(&b, "Message-ID: <m.%s.%s@%s>\r\n", f.MessageID, f.HMAC, c.Domain)
	fmt.Fprintf(&b, "Date: %s\r\n", f.Timestamp.Format(time.RFC1123Z))
	// Convenience only; the assistant cannot set headers, so nothing depends
	// on these coming back (README 7.1).
	fmt.Fprintf(&b, "X-WA-Message: %s\r\n", f.MessageID)
	fmt.Fprintf(&b, "X-WA-Timestamp: %s\r\n", f.Timestamp.UTC().Format(time.RFC3339))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")

	fmt.Fprintf(&b, "%s\r\n\r\n", replyMarker)
	fmt.Fprintf(&b, "%s  +%s\r\n", f.DisplayName, f.Number)
	fmt.Fprintf(&b, "%s\r\n\r\n", f.Timestamp.Format("2 Jan 2006, 15:04"))
	// The contact's words, last so that a mailer quoting the whole thing puts
	// them below the marker where extraction discards them.
	b.WriteString(normalizeNewlines(f.Body))
	b.WriteString("\r\n")
	return []byte(b.String())
}

// Send delivers one forward.
func (c Config) Send(f Forward) error {
	msg := c.Build(f)
	sender := fmt.Sprintf("%s@%s", f.Number, c.Domain)

	client, err := c.dial()
	if err != nil {
		return err
	}
	defer client.Quit()

	auth := smtp.PlainAuth("", c.User, c.Password, c.Host)
	if err := client.Auth(auth); err != nil {
		return fmt.Errorf("mail: authenticating as %s: %w", c.User, err)
	}
	// The envelope sender is the conversation's address, which the provider
	// must permit -- Migadu calls it wildcard sending.
	if err := client.Mail(sender); err != nil {
		return fmt.Errorf("mail: sender %s refused: %w", sender, err)
	}
	if err := client.Rcpt(c.Assistant); err != nil {
		return fmt.Errorf("mail: recipient %s refused: %w", c.Assistant, err)
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	return w.Close()
}

// dialTimeout bounds the connection attempt.
//
// A blocked submission port does not refuse, it swallows: the connection sits
// there until something gives up. Hosts commonly block 25 and 465 outbound --
// Hetzner does, on new accounts -- so this fails in seconds with a message
// naming the port instead of hanging indefinitely.
const dialTimeout = 20 * time.Second

// dial handles both submission styles: implicit TLS on 465, STARTTLS on 587.
func (c Config) dial() (*smtp.Client, error) {
	dialer := &net.Dialer{Timeout: dialTimeout}

	if c.Port == 465 {
		conn, err := tls.DialWithDialer(dialer, "tcp", c.addr(), &tls.Config{ServerName: c.Host})
		if err != nil {
			return nil, fmt.Errorf("mail: connecting to %s: %w (many hosts block 465 outbound; try 587)", c.addr(), err)
		}
		return smtp.NewClient(conn, c.Host)
	}
	conn, err := dialer.Dial("tcp", c.addr())
	if err != nil {
		return nil, fmt.Errorf("mail: connecting to %s: %w", c.addr(), err)
	}
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("mail: greeting from %s: %w", c.Host, err)
	}
	if err := client.StartTLS(&tls.Config{ServerName: c.Host}); err != nil {
		client.Close()
		return nil, fmt.Errorf("mail: starting TLS with %s: %w", c.Host, err)
	}
	return client, nil
}

// encodeWord makes a header value safe for non-ASCII, which Brazilian names
// reliably are.
func encodeWord(s string) string {
	if isASCII(s) {
		return s
	}
	return mime.QEncoding.Encode("utf-8", s)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

// SendNotice sends a plain message from the control address to the assistant.
//
// Used to answer control commands. It is not a forward: no conversation, no
// token, nothing that could be mistaken for a message from a contact.
func (c Config) SendNotice(subject, body string) error {
	sender := "control@" + c.Domain

	var b strings.Builder
	fmt.Fprintf(&b, "From: wa-bridge <%s>\r\n", sender)
	fmt.Fprintf(&b, "To: %s\r\n", c.Assistant)
	fmt.Fprintf(&b, "Subject: %s\r\n", encodeWord(subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("Auto-Submitted: auto-replied\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(normalizeNewlines(body))
	b.WriteString("\r\n")

	client, err := c.dial()
	if err != nil {
		return err
	}
	defer client.Quit()

	if err := client.Auth(smtp.PlainAuth("", c.User, c.Password, c.Host)); err != nil {
		return fmt.Errorf("mail: authenticating as %s: %w", c.User, err)
	}
	if err := client.Mail(sender); err != nil {
		return fmt.Errorf("mail: sender %s refused: %w", sender, err)
	}
	if err := client.Rcpt(c.Assistant); err != nil {
		return fmt.Errorf("mail: recipient %s refused: %w", c.Assistant, err)
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(b.String())); err != nil {
		return err
	}
	return w.Close()
}
