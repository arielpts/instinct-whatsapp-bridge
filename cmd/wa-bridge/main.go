// Command wa-bridge is the daemon and its operator commands.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/arielpts/instinct-whatsapp-bridge/internal/mail"
	"github.com/arielpts/instinct-whatsapp-bridge/internal/phone"

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
		err = run(ctx)
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

// run is the forward path: allow-listed WhatsApp messages become email.
//
// The reply half is not wired yet, so this is M1 rather than M2: it reads and
// forwards and sends nothing to WhatsApp, whatever the mode says.
func run(ctx context.Context) error {
	cfg, err := config.Load()
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

	sender := mail.Config{
		Host: cfg.SMTPHost, Port: cfg.SMTPPort,
		User: cfg.SMTPUser, Password: cfg.SMTPPassword,
		Domain: cfg.MailDomain, Assistant: cfg.AssistantAddress,
	}

	client.OnMessage(cfg, func(in wa.Inbound) {
		// Record first. A message recorded but not forwarded can be chased up;
		// one forwarded but not recorded is forwarded again on the next
		// delivery (FR-9).
		if err := st.RecordForward(ctx, in.MessageID, in.Conversation, in.SenderJID, in.Text); err != nil {
			if !errors.Is(err, store.ErrDuplicate) {
				log.Printf("recording %s: %v", in.MessageID, err)
			}
			return
		}
		tok, err := st.IssueToken(ctx, cfg.HMACKey, in.Conversation, in.MessageID)
		if err != nil {
			log.Printf("issuing a token for %s: %v", in.MessageID, err)
			return
		}
		// The address is the contact's number. Deriving it from the
		// conversation JID would put a LID in the local part -- an identifier
		// that is not a phone number and means nothing to a human reading the
		// mailbox (README 6.3).
		conv, ok := cfg.Lookup(in.Conversation)
		if !ok {
			log.Printf("no conversation for %s; not forwarding", in.Conversation)
			return
		}
		number := phone.Digits(conv.Number)
		if number == "" {
			log.Printf("conversation %q has no number to address mail from", conv.Label)
			return
		}
		name := in.SenderName
		if name == "" {
			name = conv.Label
		}
		if name == "" {
			name = "+" + number
		}
		f := mail.Forward{
			Number: number, DisplayName: name, Token: tok,
			MessageID: in.MessageID, HMAC: tok,
			Timestamp: in.Timestamp, Body: in.Text,
		}
		if err := sender.Send(f); err != nil {
			log.Printf("forwarding %s: %v", in.MessageID, err)
			return
		}
		if err := st.MarkForwarded(ctx, in.MessageID); err != nil {
			log.Printf("marking %s forwarded: %v", in.MessageID, err)
		}
		log.Printf("forwarded %s from %s as [wa:%s]", in.MessageID, name, tok)
	})

	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("connecting: %w", err)
	}

	allowed := cfg.AllowedJIDs()
	log.Printf("%s", cfg.Summary())
	log.Printf("forwarding %d conversation(s) to %s; nothing is sent to WhatsApp",
		len(allowed), cfg.AssistantAddress)

	prune := time.NewTicker(5 * time.Minute)
	defer prune.Stop()
	retention := time.NewTicker(time.Hour)
	defer retention.Stop()

	for {
		select {
		case <-prune.C:
			if n, err := st.PruneContacts(ctx, allowed); err == nil && n > 0 {
				log.Printf("pruned %d contact names", n)
			}
		case <-retention.C:
			cutoff := time.Now().AddDate(0, 0, -cfg.RetentionDays)
			if n, err := st.PurgeBodies(ctx, cutoff); err == nil && n > 0 {
				log.Printf("purged %d message bodies past retention", n)
			}
		case <-ctx.Done():
			log.Printf("stopped; %d history syncs discarded", client.DroppedHistorySyncs())
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

	// WhatsApp increasingly answers with a LID rather than a phone-number JID.
	// A LID identifies the account but says nothing about the number, so the
	// allowlist has to match on both: messages may arrive addressed either way,
	// and the email address is built from the number, never from this.
	aliases := []string{jid.User}
	if candidates, cerr := phone.Candidates(number); cerr == nil {
		aliases = append(aliases, candidates...)
	}

	fmt.Printf(`
  %s resolves to %s

  Paste into config.toml:

[[conversation]]
number  = "%s"
jid     = "%s"
aliases = [%s]
label   = "..."
actions = ["read", "draft"]

  The aliases carry both the account identifier and both spellings of the
  number, because a chat may be addressed either way. The email address is
  built from number, not from jid.

`, number, jid.String(), "+"+phone.Digits(number), jid.String(), quoteList(aliases))
	return nil
}

func quoteList(items []string) string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = strconv.Quote(s)
	}
	return strings.Join(out, ", ")
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
