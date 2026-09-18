// Command wa-bridge is the daemon and its operator commands.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/arielpts/instinct-whatsapp-bridge/internal/config"
	"github.com/arielpts/instinct-whatsapp-bridge/internal/store"
	"github.com/arielpts/instinct-whatsapp-bridge/internal/wa"
)

const usage = `wa-bridge -- Instinct WhatsApp bridge

  pair <number>     link this box to a WhatsApp account by typed code
  signup <number>   resolve a phone number to the JID WhatsApp really uses
  status            report what is linked, configured and queued
  run               forward allow-listed messages and process replies

Configuration comes from the environment and the policy file; see README 9.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "pair":
		err = pair(ctx, arg(2))
	case "signup":
		err = signup(ctx, arg(2))
	case "status":
		err = status(ctx)
	case "run":
		err = errors.New("run needs the mail layer, which is not built yet; pair and status work")
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "wa-bridge: %v\n", err)
		os.Exit(1)
	}
}

func arg(i int) string {
	if len(os.Args) > i {
		return os.Args[i]
	}
	return ""
}

// open prepares the shared database. Operator commands need only the state
// directory, so they work before any mail settings exist.
func open(ctx context.Context) (*store.Store, *wa.Client, error) {
	dir, err := config.StateDir()
	if err != nil {
		return nil, nil, err
	}
	// The device store predates the mail domain, so it is bound to the domain
	// only once one is configured.
	st, err := store.Open(ctx, filepath.Join(dir, "state.db"), os.Getenv("WA_BRIDGE_MAIL_DOMAIN"))
	if err != nil {
		return nil, nil, err
	}
	client, err := wa.Open(ctx, st.SQL(), "INFO")
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	return st, client, nil
}

func pair(ctx context.Context, number string) error {
	if number == "" {
		return errors.New("usage: wa-bridge pair <number>   (the phone being linked, e.g. +5511987654321)")
	}
	st, client, err := open(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	defer client.Disconnect()

	// PairCode connects for us: the login channel must be opened before the
	// socket, so the sequencing belongs with the client, not here.
	code, err := client.PairCode(ctx, number)
	if err != nil {
		return err
	}

	fmt.Printf(`
  Pairing code:  %s

  On the phone: WhatsApp -> Settings -> Linked Devices
                -> Link a device -> Link with phone number instead

  The code expires in about two minutes.

`, code)

	select {
	case <-client.LoggedIn():
		jid, _ := client.LinkedJID()
		fmt.Printf("  Linked: %s\n\n", jid)
		return nil
	case <-time.After(3 * time.Minute):
		return errors.New("the code expired before the phone confirmed; run pair again")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func signup(ctx context.Context, number string) error {
	if number == "" {
		return errors.New("usage: wa-bridge signup <number>")
	}
	st, client, err := open(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	defer client.Disconnect()

	if _, err := client.LinkedJID(); err != nil {
		return err
	}
	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("connecting: %w", err)
	}
	time.Sleep(2 * time.Second)

	jid, err := client.Resolve(ctx, number)
	if err != nil {
		return err
	}
	fmt.Printf(`
  %s resolves to:

    jid     = "%s"
    aliases = ["%s"]

  Copy that into a [[conversation]] block in config.toml. It is the account
  WhatsApp actually has -- not a number reshaped by guesswork.

`, number, jid.String(), jid.User)
	return nil
}

func status(ctx context.Context) error {
	st, client, err := open(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	fmt.Println()
	if jid, err := client.LinkedJID(); err != nil {
		fmt.Printf("  whatsapp   not linked -- run `wa-bridge pair <number>`\n")
	} else {
		fmt.Printf("  whatsapp   linked as %s\n", jid)
	}

	// The policy file is optional here on purpose: status should still report
	// something useful on a box that is only half set up.
	if cfg, err := config.Load(); err != nil {
		fmt.Printf("  config     not usable yet: %v\n", err)
	} else {
		fmt.Printf("  config     %s\n", cfg.Summary())
		hour := time.Now().Add(-time.Hour)
		day := time.Now().Add(-24 * time.Hour)
		perHour, _ := st.SendsSince(ctx, "", hour)
		perDay, _ := st.SendsSince(ctx, "", day)
		fmt.Printf("  quota      %d sent in the last hour, %d in the last day (caps %d/%d)\n",
			perHour, perDay, cfg.Limits.PerConversationPerHour, cfg.Limits.PerDayTotal)
	}
	fmt.Println()
	return nil
}
