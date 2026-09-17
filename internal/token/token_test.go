package token

import "testing"

var key = []byte("test key, not the real one")

func binding() Binding {
	return Binding{Conversation: "c_7f3a91", Message: "m_0192bd4c",
		Domain: "wa.example.com", IssuedUnix: 1758139511}
}

func TestIssueIsDeterministicAndOpaque(t *testing.T) {
	b := binding()
	a1, a2 := Issue(key, b), Issue(key, b)
	if a1 != a2 {
		t.Fatalf("not deterministic: %q vs %q", a1, a2)
	}
	if len(a1) != 16 {
		t.Errorf("token length %d, want 16", len(a1))
	}
	// A token must leak neither the number nor the conversation it stands for.
	for _, secret := range []string{"5511987654321", "c_7f3a91", "m_0192bd4c", "example"} {
		if contains(a1, secret) {
			t.Errorf("token %q leaks %q", a1, secret)
		}
	}
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}

func TestVerifyRejectsTamperedBindings(t *testing.T) {
	b := binding()
	good := Issue(key, b)
	if !Verify(key, good, b) {
		t.Fatal("a freshly issued token must verify")
	}
	// Every field is covered by the MAC.
	mutations := map[string]Binding{
		"conversation": {Conversation: "c_other", Message: b.Message, Domain: b.Domain, IssuedUnix: b.IssuedUnix},
		"message":      {Conversation: b.Conversation, Message: "m_other", Domain: b.Domain, IssuedUnix: b.IssuedUnix},
		"domain":       {Conversation: b.Conversation, Message: b.Message, Domain: "evil.example", IssuedUnix: b.IssuedUnix},
		"timestamp":    {Conversation: b.Conversation, Message: b.Message, Domain: b.Domain, IssuedUnix: b.IssuedUnix + 1},
	}
	for field, m := range mutations {
		if Verify(key, good, m) {
			t.Errorf("token verified against a different %s", field)
		}
	}
	if Verify([]byte("a different key"), good, b) {
		t.Error("token verified under the wrong key")
	}
}

// A token issued for one mail domain must not redeem under another (README 9.1).
func TestDomainScoping(t *testing.T) {
	b := binding()
	other := b
	other.Domain = "wa.example.net"
	if Issue(key, b) == Issue(key, other) {
		t.Error("tokens are not scoped to the mail domain")
	}
}

func TestFromSubject(t *testing.T) {
	got, ok := FromSubject("Re: [wa:abcdefgh23456722] Marina (+55 11 98765-4321)")
	if !ok || got != "abcdefgh23456722" {
		t.Errorf("FromSubject = %q, %v", got, ok)
	}
	if _, ok := FromSubject("Re: Marina"); ok {
		t.Error("found a token where there is none")
	}
}

func TestFromBodyOnlyReadsTheFirstLine(t *testing.T) {
	got, ok := FromBody("\n\n[wa:abcdefgh23456722]\nConsigo sim.\n")
	if !ok || got != "abcdefgh23456722" {
		t.Errorf("FromBody = %q, %v", got, ok)
	}
	// A token further down is not a binding. It is a leak, and treating it as a
	// binding is how quoted contact text would get to steer routing (SEC-12).
	if _, ok := FromBody("Consigo sim.\n[wa:abcdefgh23456722]\n"); ok {
		t.Error("a token below the first line must not bind")
	}
	if _, ok := FromBody("> [wa:abcdefgh23456722]\n"); ok {
		t.Error("a quoted token must not bind")
	}
}

func TestStripLeadingLine(t *testing.T) {
	got := StripLeadingLine("[wa:abcdefgh23456722]\nConsigo sim.")
	if got != "Consigo sim." {
		t.Errorf("StripLeadingLine = %q", got)
	}
	// Nothing to strip leaves the body alone.
	if got := StripLeadingLine("Consigo sim."); got != "Consigo sim." {
		t.Errorf("StripLeadingLine mangled a plain body: %q", got)
	}
}
