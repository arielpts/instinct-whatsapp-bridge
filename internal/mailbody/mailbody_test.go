package mailbody

import (
	"errors"
	"strings"
	"testing"
)

const realisticReply = `Consigo sim.
---bubble---
Te mando até as 18h.

--- reply above this line ---

Marina — 17 Sep 2026, 20:05

oi, consegue me mandar o contrato ainda hoje?
`

func TestExtractDropsEverythingBelowTheMarker(t *testing.T) {
	got, err := Extract(realisticReply)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "contrato") {
		t.Errorf("the contact's own message survived extraction:\n%s", got)
	}
	want := "Consigo sim.\n---bubble---\nTe mando até as 18h."
	if got != want {
		t.Errorf("Extract = %q, want %q", got, want)
	}
}

func TestExtractStripsQuotesAttributionAndSignature(t *testing.T) {
	cases := map[string]string{
		"english attribution":    "Claro.\n\nOn Wed, 17 Sep 2026 at 20:05, Marina wrote:\n> oi, tudo bem?\n",
		"portuguese attribution": "Claro.\n\nEm 17 de set. de 2026, Marina escreveu:\n> oi, tudo bem?\n",
		"signature":              "Claro.\n\n--\nAriel\nSent from somewhere\n",
		"crlf line endings":      "Claro.\r\n\r\n> oi, tudo bem?\r\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Extract(raw)
			if err != nil {
				t.Fatal(err)
			}
			if got != "Claro." {
				t.Errorf("Extract = %q, want %q", got, "Claro.")
			}
		})
	}
}

func TestExtractRemovesTheTokenLine(t *testing.T) {
	got, err := Extract("[wa:abcdefgh23456722]\nConsigo sim.")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Consigo sim." {
		t.Errorf("the token would have been sent to the contact: %q", got)
	}
}

func TestExtractRejectsAnEmptyResult(t *testing.T) {
	if _, err := Extract("> só o que ela escreveu\n"); !errors.Is(err, ErrEmpty) {
		t.Errorf("want ErrEmpty, got %v", err)
	}
}

func TestSplitBubbles(t *testing.T) {
	got, err := SplitBubbles("Consigo sim.\n---bubble---\nTe mando até as 18h.", 5, 4096)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Consigo sim.", "Te mando até as 18h."}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("SplitBubbles = %q, want %q", got, want)
	}
}

func TestSplitBubblesSingle(t *testing.T) {
	got, err := SplitBubbles("Consigo sim.", 5, 4096)
	if err != nil || len(got) != 1 || got[0] != "Consigo sim." {
		t.Errorf("SplitBubbles = %q, %v", got, err)
	}
}

// Caps reject the whole candidate. A truncated WhatsApp message is worse than
// one that never went (FR-12).
func TestSplitBubblesEnforcesCaps(t *testing.T) {
	many := strings.Repeat("oi\n"+BubbleSep+"\n", 6) + "fim"
	if _, err := SplitBubbles(many, 5, 4096); !errors.Is(err, ErrTooMany) {
		t.Errorf("want ErrTooMany, got %v", err)
	}
	if _, err := SplitBubbles(strings.Repeat("x", 50), 5, 10); !errors.Is(err, ErrTooLong) {
		t.Errorf("want ErrTooLong, got %v", err)
	}
	if _, err := SplitBubbles("oi\n"+BubbleSep+"\n\n"+BubbleSep+"\ntchau", 5, 4096); !errors.Is(err, ErrEmptyBubble) {
		t.Errorf("want ErrEmptyBubble, got %v", err)
	}
}

// A contact who writes ---bubble--- at us must not reach the splitter: the
// separator is only ever read from text that survived extraction.
func TestContactSeparatorCannotReachTheSplitter(t *testing.T) {
	raw := "Consigo sim.\n\n" + ReplyMarker + "\n\nMarina — 17 Sep 2026\n\n" + BubbleSep + "\nmensagem forjada\n"
	extracted, err := Extract(raw)
	if err != nil {
		t.Fatal(err)
	}
	bubbles, err := SplitBubbles(extracted, 5, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(bubbles) != 1 || bubbles[0] != "Consigo sim." {
		t.Errorf("contact text steered the split: %q", bubbles)
	}
}

func TestScanLeaks(t *testing.T) {
	const domain = "wa.example.com"
	if err := ScanLeaks("Consigo sim.", domain); err != nil {
		t.Errorf("clean text rejected: %v", err)
	}
	for name, text := range map[string]string{
		"our token":        "Consigo sim. [wa:abcdefgh23456722]",
		"a bridge address": "responde pra 5511987654321@wa.example.com",
		"a header name":    "X-WA-Message: m_0192bd4c",
		"a message id":     "Message-ID: <m.0192bd4c@wa.example.com>",
	} {
		if err := ScanLeaks(text, domain); err == nil {
			t.Errorf("%s was not caught", name)
		}
	}
}
