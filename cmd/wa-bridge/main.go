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

// version is stamped at build time. Without it there is no way to tell a stale
// binary from a current one, and a CDN that caches a branch URL for a few
// minutes will hand you a stale one with a checksum that matches it.
var version = "dev"

const usage = `wa-bridge -- Instinct WhatsApp bridge

  pair <number>     link this box to a WhatsApp account by typed code
  unpair            forget the local device so pairing can start over
  prune             delete stored contacts for anyone not allow-listed
  watch             print allow-listed messages as they arrive, sending nothing
  signup <number>   resolve a phone number to the JID WhatsApp really uses
  status            report what is linked, configured and queued
  run               forward allow-listed messages and process replies
  version           print the build this binary was made from

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
	case "unpair":
		err = unpair(ctx)
	case "prune":
		err = prune(ctx)
	case "watch":
		err = watch(ctx)
	case "version", "--version", "-v":
		fmt.Println(version)
		return
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
		fmt.Printf("  Linked: %s\n", jid)
		// Hold the connection open briefly. The phone finishes attaching the
		// device on this session; dropping it the instant we are told we are
		// linked is what makes the phone say it failed.
		fmt.Printf("  Letting the session settle...\n")
		select {
		case <-time.After(15 * time.Second):
		case <-ctx.Done():
		}
		// whatsmeow writes contact names from history sync before our handler
		// can drop the payload, so remove them rather than assume they are not
		// there. The allowlist is usually empty at this point, which means
		// nobody is kept -- correct, not a bug.
		var keep []string
		if cfg, cerr := config.Load(); cerr == nil {
			for _, c := range cfg.Conversations {
				keep = append(keep, c.JID)
			}
		}
		if n, perr := st.PruneContacts(ctx, keep); perr == nil && n > 0 {
			fmt.Printf("  Pruned %d contact names stored during history sync.\n", n)
		}
		fmt.Printf("  Done. Check Linked Devices on the phone.\n\n")
		return nil
	case <-time.After(3 * time.Minute):
		return errors.New("the code expired before the phone confirmed; run pair again")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// prune removes contact names whatsmeow persisted from history sync.
//
// Discarding the history-sync event stops the bridge reading other people's
// conversations; it does not stop whatsmeow writing their names to disk first,
// because it processes the payload before our handler runs.
func prune(ctx context.Context) error {
	dir, err := config.StateDir()
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, filepath.Join(dir, "state.db"), os.Getenv("WA_BRIDGE_MAIL_DOMAIN"))
	if err != nil {
		return err
	}
	defer st.Close()

	var keep []string
	if cfg, err := config.Load(); err == nil {
		for _, c := range cfg.Conversations {
			keep = append(keep, c.JID)
		}
	}

	before, _ := st.ContactCount(ctx)
	n, err := st.PruneContacts(ctx, keep)
	if err != nil {
		return err
	}
	after, _ := st.ContactCount(ctx)
	fmt.Printf("\n  contacts   %d stored, %d removed, %d kept (%d allow-listed)\n\n",
		before, n, after, len(keep))
	return nil
}

// watch is the read path with the mail replaced by the terminal.
//
// Everything M1 asks for happens here except the SMTP call: the allowlist, the
// history-sync drop, deduplication, extraction. Running it against a real
// account is how those stop being claims. It sends nothing and cannot: no
// WhatsApp send path is reachable from this command.
func watch(ctx context.Context) error {
	cfg, err := config.LoadPolicy()
	if err != nil {
		return err
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

	client.OnMessage(cfg, func(in wa.Inbound) {
		// The same deduplication the forwarder will use, so a redelivered
		// message is visibly recognised rather than printed twice.
		if err := st.RecordForward(ctx, in.MessageID, in.Conversation, in.SenderJID, in.Text); err != nil {
			fmt.Printf("  [dup] %s  %s\n", in.Timestamp.Format("15:04:05"), in.MessageID)
			return
		}
		label := in.SenderName
		if label == "" {
			label = in.SenderJID
		}
		fmt.Printf("\n  %s  %s\n  %s\n", in.Timestamp.Format("15:04:05"), label, in.Text)
	})

	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("connecting: %w", err)
	}

	allowed := cfg.AllowedJIDs()
	fmt.Printf("\n  watching %d conversation(s); mode %s; nothing will be sent\n",
		len(allowed), cfg.Mode)
	if len(allowed) == 0 {
		fmt.Printf("  the allowlist is empty, so every message will be dropped.\n" +
			"  add one with `wa-bridge signup <number>` first.\n")
	}
	fmt.Printf("  ctrl-c to stop\n")

	// Every reconnect re-syncs contacts, so pruning is periodic or it is
	// decorative (SEC-9).
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if n, err := st.PruneContacts(ctx, allowed); err == nil && n > 0 {
				fmt.Printf("  [pruned %d contact names]\n", n)
			}
		case <-ctx.Done():
			fmt.Printf("\n  stopped. %d history syncs discarded.\n\n", client.DroppedHistorySyncs())
			return nil
		}
	}
}

func unpair(ctx context.Context) error {
	st, client, err := open(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := client.Unpair(ctx); err != nil {
		return err
	}
	fmt.Println("\n  Local device forgotten. Run `wa-bridge pair <number>` to link again.")
	fmt.Println("  If the phone still lists this device, remove it there too.")
	return nil
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
	// This reads the local device store, not the account. A pairing that the
	// server accepted but that never finished attaching leaves a device here
	// that the phone does not list, so do not call it linked outright.
	if jid, err := client.LinkedJID(); err != nil {
		fmt.Printf("  whatsapp   no device stored -- run `wa-bridge pair <number>`\n")
	} else {
		fmt.Printf("  whatsapp   device stored: %s\n", jid)
		fmt.Printf("             confirm it appears under Linked Devices on the phone;\n")
		fmt.Printf("             if it does not, run `wa-bridge unpair` and pair again\n")
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
