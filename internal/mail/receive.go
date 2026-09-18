package mail

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
)

var (
	ErrNoRecipient = errors.New("mail: no envelope recipient")
	ErrNoText      = errors.New("mail: no text/plain part")
)

// Received is a reply, reduced to the parts the bridge reasons about.
type Received struct {
	From       string // the address in From:, lowercased
	EnvelopeTo string // who the mail was actually delivered for
	Subject    string
	MessageID  string
	InReplyTo  string
	AuthservID string // the stamping host, for the SEC-14 trust check
	AuthResult string // that stamp's contents
	Text       string // the text/plain part
}

// Parse reduces a raw message.
//
// Header choice matters more than it looks. The catch-all delivers every
// conversation's address into one mailbox, so Delivered-To says only which
// mailbox it landed in; X-Envelope-To carries the address the mail was
// actually sent to. That distinction is what gives SEC-13 a second channel to
// check the token against -- and unlike To:, the sender does not write it.
func Parse(raw []byte) (*Received, error) {
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("mail: parsing: %w", err)
	}

	var r Received
	dec := new(mime.WordDecoder)
	decode := func(s string) string {
		if out, err := dec.DecodeHeader(s); err == nil {
			return out
		}
		return s
	}

	// Get returns the first occurrence, which is what we want: the provider
	// prepends its headers on delivery, so a copy the sender wrote themselves
	// sits below and is ignored.
	r.EnvelopeTo = addressOnly(msg.Header.Get("X-Envelope-To"))
	if r.EnvelopeTo == "" {
		r.EnvelopeTo = addressOnly(msg.Header.Get("Delivered-To"))
	}
	if r.EnvelopeTo == "" {
		r.EnvelopeTo = addressOnly(msg.Header.Get("To"))
	}
	if r.EnvelopeTo == "" {
		return nil, ErrNoRecipient
	}

	r.From = addressOnly(msg.Header.Get("From"))
	r.Subject = decode(msg.Header.Get("Subject"))
	r.MessageID = strings.TrimSpace(msg.Header.Get("Message-ID"))
	r.InReplyTo = strings.TrimSpace(msg.Header.Get("In-Reply-To"))

	if ar := msg.Header.Get("Authentication-Results"); ar != "" {
		r.AuthservID, r.AuthResult = splitAuthResults(ar)
	}

	text, err := textPart(msg.Header.Get("Content-Type"),
		msg.Header.Get("Content-Transfer-Encoding"), msg.Body)
	if err != nil {
		return nil, err
	}
	r.Text = text
	return &r, nil
}

// addressOnly reduces a header to a bare lowercase address.
func addressOnly(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	if addr, err := mail.ParseAddress(header); err == nil {
		return strings.ToLower(addr.Address)
	}
	// Delivered-To and X-Envelope-To are usually bare addresses, which
	// ParseAddress accepts, but be forgiving about stray angle brackets.
	header = strings.Trim(header, "<> \t")
	if strings.Contains(header, "@") {
		return strings.ToLower(header)
	}
	return ""
}

// splitAuthResults separates the stamping host from its verdicts.
func splitAuthResults(ar string) (authserv, rest string) {
	authserv, rest, found := strings.Cut(ar, ";")
	if !found {
		return strings.TrimSpace(ar), ""
	}
	return strings.TrimSpace(authserv), strings.TrimSpace(rest)
}

// Verdict reports whether the stamp asserts a passing result for a mechanism.
func (r *Received) Verdict(mechanism string) string {
	for _, field := range strings.Split(r.AuthResult, ";") {
		field = strings.TrimSpace(field)
		if after, ok := strings.CutPrefix(field, mechanism+"="); ok {
			value, _, _ := strings.Cut(after, " ")
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// DKIMDomain returns the d= of the stamped DKIM result, which is what SEC-14
// pins rather than the display address.
func (r *Received) DKIMDomain() string {
	for _, field := range strings.Split(r.AuthResult, ";") {
		for _, part := range strings.Fields(field) {
			if after, ok := strings.CutPrefix(part, "header.d="); ok {
				return strings.ToLower(strings.TrimSpace(after))
			}
		}
	}
	return ""
}

// decoded wraps a body reader according to its transfer encoding.
//
// Real senders use quoted-printable as a matter of course, and an undecoded
// body turns "n=C3=A3o" into the message. Pure ASCII survives the mistake
// intact, which is exactly why it would have shipped: the first test message
// looks perfect and every accented one after it is corrupted.
func decoded(encoding string, r io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, r)
	default:
		return r
	}
}

// textPart walks a message for its text/plain content.
//
// HTML-only messages return ErrNoText rather than being converted: SEC-12
// rejects what it cannot read plainly instead of guessing at the words.
func textPart(contentType, encoding string, body io.Reader) (string, error) {
	if contentType == "" {
		contentType = "text/plain"
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", fmt.Errorf("mail: content type %q: %w", contentType, err)
	}

	if !strings.HasPrefix(mediaType, "multipart/") {
		if mediaType != "text/plain" {
			return "", ErrNoText
		}
		b, err := io.ReadAll(decoded(encoding, body))
		if err != nil {
			return "", err
		}
		return string(b), nil
	}

	boundary := params["boundary"]
	if boundary == "" {
		return "", errors.New("mail: multipart without a boundary")
	}
	mr := multipart.NewReader(body, boundary)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return "", ErrNoText
		}
		if err != nil {
			return "", err
		}
		partType := part.Header.Get("Content-Type")
		// Go hides quoted-printable on multipart parts and decodes them as
		// they are read; base64 it leaves to us.
		partEncoding := part.Header.Get("Content-Transfer-Encoding")
		media, _, _ := mime.ParseMediaType(partType)
		switch {
		case strings.HasPrefix(media, "multipart/"):
			// Nested, as multipart/mixed wrapping multipart/alternative.
			if inner, err := textPart(partType, partEncoding, part); err == nil {
				return inner, nil
			}
		case media == "text/plain" || partType == "":
			b, err := io.ReadAll(decoded(partEncoding, part))
			if err != nil {
				return "", err
			}
			return string(b), nil
		}
	}
}
