// Command wa-bridge is the daemon and its operator commands.
package main

import (
	"fmt"
	"os"
)

var usage = `wa-bridge -- Instinct WhatsApp bridge

  run      forward allow-listed messages and process replies
  pair     link this box to a WhatsApp account by QR
  signup   resolve a phone number to its real JID
  status   report health, mode and quota state

Configuration comes from the environment and the policy file; see README 9.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run", "pair", "signup", "status":
		fmt.Fprintf(os.Stderr, "wa-bridge: %s is not wired up yet\n", os.Args[1])
		os.Exit(1)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}
