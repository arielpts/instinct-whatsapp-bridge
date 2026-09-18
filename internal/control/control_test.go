package control

import (
	"errors"
	"testing"
)

func TestParse(t *testing.T) {
	cases := map[string]Command{
		"allowlist +5541999064782":             {Verb: Allowlist, Number: "+5541999064782"},
		"allowlist +5541999064782 Maria":       {Verb: Allowlist, Number: "+5541999064782", Label: "Maria"},
		"allowlist +5541999064782 Maria Silva": {Verb: Allowlist, Number: "+5541999064782", Label: "Maria Silva"},
		"ALLOWLIST +5541999064782":             {Verb: Allowlist, Number: "+5541999064782"},
		"remove +5541999064782":                {Verb: Remove, Number: "+5541999064782"},
		"list":                                 {Verb: List},
		"\n\n  list  \n":                       {Verb: List},
		"> quoted line\nlist":                  {Verb: List},
	}
	for body, want := range cases {
		got, err := Parse(body)
		if err != nil {
			t.Errorf("Parse(%q): %v", body, err)
			continue
		}
		if *got != want {
			t.Errorf("Parse(%q) = %+v, want %+v", body, *got, want)
		}
	}
}

func TestParseRejections(t *testing.T) {
	for body, want := range map[string]error{
		"":                    ErrNoCommand,
		"oi, tudo bem?":       ErrUnknown,
		"allowlist":           ErrArguments,
		"remove":              ErrArguments,
		"send +5541999064782": ErrUnknown, // there is no send verb, deliberately
	} {
		if _, err := Parse(body); !errors.Is(err, want) {
			t.Errorf("Parse(%q) = %v, want %v", body, err, want)
		}
	}
}

// The address decides whether a message is a command. Conversation text can
// never reach the parser, whatever it says.
func TestIsControlAddress(t *testing.T) {
	for addr, want := range map[string]bool{
		"control@wa.example.com":       true,
		"CONTROL@WA.EXAMPLE.COM":       true,
		"5541999064782@wa.example.com": false,
		"control@attacker.test":        false,
		"control":                      false,
	} {
		if got := IsControlAddress(addr, "wa.example.com"); got != want {
			t.Errorf("IsControlAddress(%q) = %v, want %v", addr, got, want)
		}
	}
}
