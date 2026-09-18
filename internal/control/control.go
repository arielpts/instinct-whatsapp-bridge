// Package control parses the commands the assistant may send the bridge.
//
// Commands arrive at a dedicated address. That is the whole safety property:
// the envelope decides whether a message is payload or instruction, so no text
// in a conversation can ever be read as a command, and no command address can
// ever put words on WhatsApp. SEC-4 survives the addition of a command channel
// because the channel is an address, not a syntax.
package control

import (
	"errors"
	"fmt"
	"strings"
)

// LocalPart is the reserved address commands are sent to.
const LocalPart = "control"

var (
	ErrNoCommand = errors.New("control: no command found")
	ErrUnknown   = errors.New("control: unknown command")
	ErrArguments = errors.New("control: wrong arguments")
)

// Verb is what a command asks for.
type Verb string

const (
	Allowlist Verb = "allowlist" // add a conversation, read and draft only
	Remove    Verb = "remove"    // drop one the bridge added
	List      Verb = "list"      // report what is allow-listed
)

// Command is one parsed instruction.
type Command struct {
	Verb   Verb
	Number string // for allowlist and remove
	Label  string // optional, for allowlist
}

// Parse reads the first command from a message body.
//
// Only the first is honoured. A body that asks for several things is more
// likely a misunderstanding than an intent, and refusing the rest is cheaper
// to recover from than performing them.
func Parse(body string) (*Command, error) {
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ">") || strings.HasPrefix(line, "--") {
			continue
		}
		fields := strings.Fields(line)
		switch Verb(strings.ToLower(fields[0])) {
		case Allowlist:
			if len(fields) < 2 {
				return nil, fmt.Errorf("%w: allowlist needs a number", ErrArguments)
			}
			return &Command{
				Verb:   Allowlist,
				Number: fields[1],
				Label:  strings.TrimSpace(strings.Join(fields[2:], " ")),
			}, nil
		case Remove:
			if len(fields) < 2 {
				return nil, fmt.Errorf("%w: remove needs a number", ErrArguments)
			}
			return &Command{Verb: Remove, Number: fields[1]}, nil
		case List:
			return &Command{Verb: List}, nil
		default:
			return nil, fmt.Errorf("%w: %q", ErrUnknown, fields[0])
		}
	}
	return nil, ErrNoCommand
}

// IsControlAddress reports whether an envelope recipient is the command address.
func IsControlAddress(envelopeTo, mailDomain string) bool {
	local, domain, ok := strings.Cut(envelopeTo, "@")
	if !ok {
		return false
	}
	return strings.EqualFold(local, LocalPart) && strings.EqualFold(domain, mailDomain)
}
