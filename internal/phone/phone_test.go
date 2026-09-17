package phone

import (
	"errors"
	"reflect"
	"testing"
)

func TestCandidates(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"sao paulo mobile, punctuated, ninth digit first",
			"+55 (11) 98765-4321", []string{"5511987654321", "551187654321"}},
		{"sao paulo mobile without the ninth digit, same pair",
			"551187654321", []string{"5511987654321", "551187654321"}},
		{"belo horizonte prefers the shorter form first",
			"+55 31 98765-4321", []string{"553187654321", "5531987654321"}},
		{"landline never had a ninth digit",
			"+55 11 3333-4444", []string{"551133334444"}},
		{"non-brazilian numbers are left alone",
			"+1 415 555 0123", []string{"14155550123"}},
		{"unrecognised length passes through untouched",
			"5511987", []string{"5511987"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Candidates(c.in)
			if err != nil {
				t.Fatalf("Candidates(%q): %v", c.in, err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Candidates(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// Both spellings of one person must produce the same candidate set, or the
// allowlist would hold two entries for one human (FR-11).
func TestCandidatesAgreeAcrossSpellings(t *testing.T) {
	with, err := Candidates("5511987654321")
	if err != nil {
		t.Fatal(err)
	}
	without, err := Candidates("551187654321")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(with, without) {
		t.Errorf("ninth-digit variants disagree: %v vs %v", with, without)
	}
}

func TestCandidatesEmpty(t *testing.T) {
	if _, err := Candidates("no digits here"); !errors.Is(err, ErrEmpty) {
		t.Errorf("want ErrEmpty, got %v", err)
	}
}

func TestParseJID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"5511987654321@s.whatsapp.net", "5511987654321@s.whatsapp.net"},
		{"5511987654321@c.us", "5511987654321@s.whatsapp.net"}, // legacy server normalises
		{"5511987654321:12@s.whatsapp.net", "5511987654321@s.whatsapp.net"}, // device is not identity
		{"5511987654321", "5511987654321@s.whatsapp.net"},
		{"192837465@lid", "192837465@lid"},
	}
	for _, c := range cases {
		got, err := ParseJID(c.in)
		if err != nil {
			t.Fatalf("ParseJID(%q): %v", c.in, err)
		}
		if got.String() != c.want {
			t.Errorf("ParseJID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseJIDRejectsGroups(t *testing.T) {
	if _, err := ParseJID("120363000000000000@g.us"); !errors.Is(err, ErrGroup) {
		t.Errorf("groups must be rejected, got %v", err)
	}
}
