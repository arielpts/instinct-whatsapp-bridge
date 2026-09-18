package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var key = []byte(strings.Repeat("k", 32))

func open(t *testing.T, domain string) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"), domain)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTokensAreSingleUse(t *testing.T) {
	ctx := context.Background()
	s := open(t, "wa.example.com")

	tok, err := s.IssueToken(ctx, key, "c_7f3a91", "m_0192bd4c")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.RedeemToken(ctx, key, tok)
	if err != nil {
		t.Fatal(err)
	}
	if b.Conversation != "c_7f3a91" || b.Message != "m_0192bd4c" {
		t.Errorf("binding = %+v", b)
	}
	// The replay: the same token presented twice.
	if _, err := s.RedeemToken(ctx, key, tok); !errors.Is(err, ErrTokenRedeemed) {
		t.Errorf("a spent token was redeemed again: %v", err)
	}
}

func TestRedeemRejectsForgeries(t *testing.T) {
	ctx := context.Background()
	s := open(t, "wa.example.com")
	if _, err := s.RedeemToken(ctx, key, "neverissued2345"); !errors.Is(err, ErrUnknownToken) {
		t.Errorf("want ErrUnknownToken, got %v", err)
	}
	tok, err := s.IssueToken(ctx, key, "c_7f3a91", "m_0192bd4c")
	if err != nil {
		t.Fatal(err)
	}
	// A row in the table is not authority; the MAC is.
	if _, err := s.RedeemToken(ctx, []byte(strings.Repeat("x", 32)), tok); !errors.Is(err, ErrUnknownToken) {
		t.Errorf("token redeemed under the wrong key: %v", err)
	}
}

// State carries the domain it was built under, so old tokens cannot redeem
// against addresses at a new one (README 9.1).
func TestStateIsBoundToItsMailDomain(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")

	s, err := Open(ctx, path, "wa.example.com")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	if _, err := Open(ctx, path, "other.example.net"); !errors.Is(err, ErrWrongDomain) {
		t.Fatalf("state opened under a different domain: %v", err)
	}
	s, err = Open(ctx, path, "wa.example.com")
	if err != nil {
		t.Fatalf("reopening under the same domain failed: %v", err)
	}
	s.Close()
}

func TestForwardDeduplication(t *testing.T) {
	ctx := context.Background()
	s := open(t, "wa.example.com")
	if err := s.RecordForward(ctx, "m_1", "c_1", "s_1", "oi"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordForward(ctx, "m_1", "c_1", "s_1", "oi"); !errors.Is(err, ErrDuplicate) {
		t.Errorf("a redelivered message was forwarded twice: %v", err)
	}
}

// A retried reply email must not queue a second WhatsApp message (FR-9).
func TestCandidateDeduplicationByMailMessageID(t *testing.T) {
	ctx := context.Background()
	s := open(t, "wa.example.com")
	bubbles := []string{"Consigo sim.", "Te mando até as 18h."}

	id, err := s.CreateCandidate(ctx, "<reply@mail.instinct.com>", "tok", "c_1", bubbles)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCandidate(ctx, "<reply@mail.instinct.com>", "tok", "c_1", bubbles); !errors.Is(err, ErrDuplicate) {
		t.Errorf("the same email queued twice: %v", err)
	}
	got, err := s.Bubbles(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Body != bubbles[0] || got[1].Body != bubbles[1] || got[0].Sent {
		t.Errorf("bubbles = %+v", got)
	}
}

// After a partial delivery the sent bubbles stay marked, so resuming cannot
// repeat what already arrived (FR-12).
func TestPartialDeliveryIsResumable(t *testing.T) {
	ctx := context.Background()
	s := open(t, "wa.example.com")
	id, err := s.CreateCandidate(ctx, "<r1>", "tok", "c_1", []string{"um", "dois", "três"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.MarkBubbleSent(ctx, id, i, "c_1", "wa_x"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Resolve(ctx, id, StatePartial, "bubble 3 failed"); err != nil {
		t.Fatal(err)
	}

	got, err := s.Bubbles(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var unsent []string
	for _, b := range got {
		if !b.Sent {
			unsent = append(unsent, b.Body)
		}
	}
	if len(unsent) != 1 || unsent[0] != "três" {
		t.Errorf("unsent = %v, want only the third", unsent)
	}
	if state, _ := s.State(ctx, id); state != StatePartial {
		t.Errorf("state = %q", state)
	}
}

// Quotas count bubbles that actually reached someone, not candidates (SEC-6).
func TestQuotaCountsBubbles(t *testing.T) {
	ctx := context.Background()
	s := open(t, "wa.example.com")
	id, err := s.CreateCandidate(ctx, "<r1>", "tok", "c_1", []string{"um", "dois", "três"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.MarkBubbleSent(ctx, id, i, "c_1", "wa_x"); err != nil {
			t.Fatal(err)
		}
	}
	hour := time.Now().Add(-time.Hour)
	if n, _ := s.SendsSince(ctx, "c_1", hour); n != 3 {
		t.Errorf("per-conversation count = %d, want 3", n)
	}
	if n, _ := s.SendsSince(ctx, "", hour); n != 3 {
		t.Errorf("total count = %d, want 3", n)
	}
	if n, _ := s.SendsSince(ctx, "c_other", hour); n != 0 {
		t.Errorf("another conversation was charged %d", n)
	}
}

// Content goes; the identifier stays, so an old message redelivered after the
// purge is still recognised as a duplicate (SEC-7).
func TestPurgeDropsBodiesAndKeepsIdentifiers(t *testing.T) {
	ctx := context.Background()
	s := open(t, "wa.example.com")
	s.now = func() time.Time { return time.Now().Add(-30 * 24 * time.Hour) }
	if err := s.RecordForward(ctx, "m_old", "c_1", "s_1", "conteúdo antigo"); err != nil {
		t.Fatal(err)
	}
	s.now = time.Now
	if err := s.RecordForward(ctx, "m_new", "c_1", "s_1", "recente"); err != nil {
		t.Fatal(err)
	}

	n, err := s.PurgeBodies(ctx, time.Now().Add(-7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("purged %d rows, want 1", n)
	}
	if err := s.RecordForward(ctx, "m_old", "c_1", "s_1", "conteúdo antigo"); !errors.Is(err, ErrDuplicate) {
		t.Error("the purged message is no longer recognised as a duplicate")
	}
	var body *string
	if err := s.db.QueryRow(`SELECT body FROM forwards WHERE message_id = 'm_new'`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if body == nil || *body != "recente" {
		t.Error("the recent message was purged too")
	}
}

func TestExpirePending(t *testing.T) {
	ctx := context.Background()
	s := open(t, "wa.example.com")
	s.now = func() time.Time { return time.Now().Add(-time.Hour) }
	id, err := s.CreateCandidate(ctx, "<r1>", "tok", "c_1", []string{"oi"})
	if err != nil {
		t.Fatal(err)
	}
	s.now = time.Now
	if n, err := s.ExpirePending(ctx, time.Now().Add(-15*time.Minute)); err != nil || n != 1 {
		t.Fatalf("expired %d rows: %v", n, err)
	}
	if state, _ := s.State(ctx, id); state != StateExpired {
		t.Errorf("state = %q, want expired", state)
	}
}
