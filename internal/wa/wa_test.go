package wa

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestExtractText(t *testing.T) {
	cases := map[string]struct {
		msg  *waE2E.Message
		want string
	}{
		"plain conversation": {
			&waE2E.Message{Conversation: proto.String("oi, tudo bem?")}, "oi, tudo bem?"},
		"extended text": {
			&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String("com link")}}, "com link"},
		// Media is out of scope for v1: skipped, never guessed at.
		"image with caption": {
			&waE2E.Message{ImageMessage: &waE2E.ImageMessage{
				Caption: proto.String("legenda")}}, ""},
		"nil message":   {nil, ""},
		"empty message": {&waE2E.Message{}, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := extractText(c.msg); got != c.want {
				t.Errorf("extractText = %q, want %q", got, c.want)
			}
		})
	}
}

// WhatsApp rejects a pairing request whose display name is not a common
// browser, with a 400 that says nothing about why. Keep the shape honest.
func TestPairDisplayNameLooksLikeABrowser(t *testing.T) {
	want := "Chrome (Linux)"
	if pairDisplayName != want {
		t.Errorf("pairDisplayName = %q; WhatsApp validates this against common "+
			"browsers and 400s otherwise, so it cannot be the product name", pairDisplayName)
	}
}
