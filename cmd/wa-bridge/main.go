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
	"sync"
	"syscall"
	"time"

	"github.com/arielpts/instinct-whatsapp-bridge/internal/control"
	"github.com/arielpts/instinct-whatsapp-bridge/internal/mail"
	"github.com/arielpts/instinct-whatsapp-bridge/internal/phone"
	"github.com/arielpts/instinct-whatsapp-bridge/internal/reply"

	"go.mau.fi/whatsmeow/types"

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

// held is the running configuration, swappable without a restart.
//
// Control mail can add conversations, so the allowlist changes while the
// bridge is running. Reloading through one guarded pointer keeps every reader
// on a consistent view rather than a half-updated one.
type held struct {
	mu sync.RWMutex
	c  *config.Config
}

func (h *held) get() *config.Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.c
}

func (h *held) reload() error {
	fresh, err := config.Load()
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.c = fresh
	h.mu.Unlock()
	return nil
}

// Allowed satisfies wa.Allower against whatever the current allowlist is.
func (h *held) Allowed(jid string) bool { return h.get().Allowed(jid) }

// run is the forward path: allow-listed WhatsApp messages become email.
//
// The reply half is not wired yet, so this is M1 rather than M2: it reads and
// forwards and sends nothing to WhatsApp, whatever the mode says.
func run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	h := &held{c: cfg}
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

	// Forwarding happens off the event handler.
	//
	// whatsmeow dispatches events synchronously, so an SMTP round-trip inside
	// the handler stalls the socket's node processing -- observed as "node
	// handling is taking long" while a blocked submission port swallowed the
	// connection. Queue instead, and let one worker send.
	queue := make(chan wa.Inbound, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for in := range queue {
			forward(ctx, st, h.get(), sender, in)
		}
	}()

	client.OnMessage(h, func(in wa.Inbound) {
		select {
		case queue <- in:
		default:
			// Dropping is better than blocking the socket. The message stays
			// unrecorded, so WhatsApp redelivering it is a second chance.
			log.Printf("forward queue full; %s not taken", in.MessageID)
		}
	})

	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("connecting: %w", err)
	}

	allowed := cfg.AllowedJIDs()
	log.Printf("%s", cfg.Summary())
	log.Printf("forwarding %d conversation(s) to %s; polling %s for replies",
		len(allowed), cfg.AssistantAddress, cfg.IMAPHost)
	switch {
	case cfg.Mode == config.ModeApproveExcept:
		log.Printf("mode=%s: approved replies are sent without asking; "+
			"`touch %s/%s` stops sending immediately",
			cfg.Mode, cfg.StateDir, store.PanicFile)
	default:
		log.Printf("mode=%s: nothing is sent to WhatsApp", cfg.Mode)
	}

	inbox := mail.Inbox{
		Host: cfg.IMAPHost, Port: cfg.IMAPPort,
		User: cfg.IMAPUser, Password: cfg.IMAPPassword,
		Mailbox: "INBOX",
	}
	// The trace carries credentials, so it is opt-in and goes to the journal
	// where the operator already is.
	if os.Getenv("WA_BRIDGE_IMAP_DEBUG") != "" {
		inbox.Debug = log.Writer()
		log.Printf("IMAP protocol tracing is on; it includes the password")
	}
	poll := time.NewTicker(30 * time.Second)
	defer poll.Stop()

	// A failed send leaves the message recorded but unforwarded. Retry it, or
	// a transient SMTP failure is a silent deletion: WhatsApp will not send it
	// again, and deduplication would refuse it if it did.
	retry := time.NewTicker(time.Minute)
	defer retry.Stop()

	prune := time.NewTicker(5 * time.Minute)
	defer prune.Stop()
	retention := time.NewTicker(time.Hour)
	defer retention.Stop()

	for {
		select {
		case <-poll.C:
			if err := pollOnce(ctx, st, h, client, sender, inbox); err != nil {
				log.Printf("polling: %v", err)
			}

		case <-retry.C:
			pending, err := st.Pending(ctx, time.Now().Add(-30*time.Second), 20)
			if err != nil {
				log.Printf("looking for unforwarded messages: %v", err)
				break
			}
			for _, p := range pending {
				resend(ctx, st, h.get(), sender, p)
			}

		case <-prune.C:
			if n, err := st.PruneContacts(ctx, h.get().AllowedJIDs()); err == nil && n > 0 {
				log.Printf("pruned %d contact names", n)
			}
		case <-retention.C:
			cutoff := time.Now().AddDate(0, 0, -h.get().RetentionDays)
			if n, err := st.PurgeBodies(ctx, cutoff); err == nil && n > 0 {
				log.Printf("purged %d message bodies past retention", n)
			}
		case <-ctx.Done():
			close(queue)
			<-done // let an in-flight send finish rather than truncating it
			log.Printf("stopped; %d history syncs discarded", client.DroppedHistorySyncs())
			return nil
		}
	}
}

// forward turns one inbound message into one email.
func forward(ctx context.Context, st *store.Store, cfg *config.Config, sender mail.Config, in wa.Inbound) {
	// Record first. A message recorded but not forwarded can be chased up; one
	// forwarded but not recorded is forwarded again on the next delivery (FR-9).
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
	// The address is the contact's number. Deriving it from the conversation
	// JID would put a LID in the local part -- an identifier that is not a
	// phone number and means nothing to a human reading the mailbox (6.3).
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
	if err := sender.Send(mail.Forward{
		Number: number, DisplayName: name, Token: tok,
		MessageID: in.MessageID, HMAC: tok,
		Timestamp: in.Timestamp, Body: in.Text,
	}); err != nil {
		log.Printf("forwarding %s: %v", in.MessageID, err)
		return
	}
	if err := st.MarkForwarded(ctx, in.MessageID); err != nil {
		log.Printf("marking %s forwarded: %v", in.MessageID, err)
	}
	log.Printf("forwarded %s from %s as [wa:%s]", in.MessageID, name, tok)
}

// pollOnce reads the mailbox and turns accepted replies into candidates.
//
// Rejections are marked seen as well as accepted messages. A rejected reply
// that stayed unread would be re-examined forever, and SEC-11 wants a count
// rather than a retry: the reasons are logged, the contents are not.
func pollOnce(ctx context.Context, st *store.Store, h *held, client *wa.Client, sender mail.Config, inbox mail.Inbox) error {
	return inbox.Poll(20, func(m mail.Message) bool {
		return handle(ctx, st, h, client, sender, m)
	})
}

// handle reports whether the message reached a terminal outcome. A transient
// failure returns false, leaving it unread to be tried again.
func handle(ctx context.Context, st *store.Store, h *held, client *wa.Client, sender mail.Config, m mail.Message) bool {
	cfg := h.get()
	r, err := mail.Parse(m.Raw)
	if err != nil {
		log.Printf("mail %d: unreadable: %v", m.UID, err)
		return true
	}
	// The address decides what this message is. Commands arrive here and
	// nowhere else; conversation mail can never be read as an instruction.
	if control.IsControlAddress(r.EnvelopeTo, cfg.MailDomain) {
		return handleControl(ctx, h, client, sender, r, m.UID)
	}
	v, err := reply.Verify(cfg, r)
	if err != nil {
		// The reason, never the words. A rejected message is one we have no
		// business quoting into a log.
		log.Printf("mail %d from %s: rejected: %v", m.UID, r.From, err)
		return true
	}
	binding, err := st.RedeemToken(ctx, cfg.HMACKey, v.Token)
	if err != nil {
		log.Printf("mail %d: token [wa:%s]: %v", m.UID, v.Token, err)
		return true
	}
	// SEC-13 completed: the token and the address must name one conversation.
	if err := v.Agree(binding, cfg); err != nil {
		log.Printf("mail %d: %v", m.UID, err)
		return true
	}
	id, err := st.CreateCandidate(ctx, r.MessageID, v.Token, v.Conversation.JID, v.Bubbles)
	if errors.Is(err, store.ErrDuplicate) {
		return true
	}
	if err != nil {
		log.Printf("mail %d: queueing: %v", m.UID, err)
		return false // transient; try again rather than lose the reply
	}
	log.Printf("candidate %d for %s: %d bubble(s) [mode %s]",
		id, v.Conversation.Label, len(v.Bubbles), cfg.Mode)

	// Standing authorization: the conversation must grant send, and the mode
	// must be the one that does not ask. Everything else waits.
	if cfg.Mode == config.ModeApproveExcept && v.Conversation.Can(config.ActionSend) {
		deliver(ctx, st, cfg, client, id, v.Conversation)
	} else {
		log.Printf("candidate %d awaits approval: mode=%s send=%v",
			id, cfg.Mode, v.Conversation.Can(config.ActionSend))
	}
	return true
}

// maxManaged caps how many conversations control mail may add.
//
// The cap is the difference between delegating the allowlist and delegating
// the account. A runaway, a loop, or a misunderstanding stops here instead of
// mirroring every chat the owner has.
const maxManaged = 50

// handleControl executes a command sent to the control address.
//
// Identity is checked exactly as for a reply: the same From allowlist, the
// same signature verified against DNS. A command is more dangerous than a
// message, so it gets no weaker a gate.
func handleControl(ctx context.Context, h *held, client *wa.Client, sender mail.Config, r *mail.Received, uid uint32) bool {
	cfg := h.get()

	if !cfg.AcceptsInboundMail() ||
		!containsFold(cfg.Instinct.FromAddresses, r.From) ||
		!r.SignedBy(cfg.Instinct.DKIMDomains) {
		log.Printf("control %d from %s: rejected: not an allow-listed, signed sender", uid, r.From)
		return true
	}

	cmd, err := control.Parse(r.Text)
	if err != nil {
		log.Printf("control %d: %v", uid, err)
		notify(sender, "control: not understood", fmt.Sprintf("%v\n\nCommands: allowlist <number> [label] | remove <number> | list", err))
		return true
	}

	switch cmd.Verb {
	case control.List:
		var b strings.Builder
		for _, c := range cfg.Conversations {
			origin := "config"
			if c.Managed {
				origin = "added by control"
			}
			fmt.Fprintf(&b, "%-20s %-24s %v  (%s)\n", c.Number, c.Label, c.Actions, origin)
		}
		if b.Len() == 0 {
			b.WriteString("nothing is allow-listed\n")
		}
		notify(sender, "control: allowlist", b.String())
		log.Printf("control %d: listed %d conversation(s)", uid, len(cfg.Conversations))

	case control.Allowlist:
		managed := 0
		for _, c := range cfg.Conversations {
			if c.Managed {
				managed++
			}
		}
		if managed >= maxManaged {
			log.Printf("control %d: refused: %d managed conversations is the cap", uid, managed)
			notify(sender, "control: refused", fmt.Sprintf("%d conversations already added by control; the cap is %d.", managed, maxManaged))
			return true
		}

		// Ask WhatsApp which account the number is, rather than trusting the
		// digits in the message (FR-10).
		jid, err := client.Resolve(ctx, cmd.Number)
		if err != nil {
			log.Printf("control %d: resolving %s: %v", uid, cmd.Number, err)
			notify(sender, "control: not added", fmt.Sprintf("%s: %v", cmd.Number, err))
			return true
		}
		label := cmd.Label
		if label == "" {
			label = cmd.Number
		}
		aliases := []string{jid.User}
		if candidates, cerr := phone.Candidates(cmd.Number); cerr == nil {
			aliases = append(aliases, candidates...)
		}
		entry := config.Conversation{
			Number: "+" + phone.Digits(cmd.Number), JID: jid.String(),
			Aliases: aliases, Label: label, Actions: config.ManagedActions,
		}
		if err := config.AppendManaged(cfg.StateDir, entry); err != nil {
			log.Printf("control %d: %v", uid, err)
			notify(sender, "control: not added", err.Error())
			return true
		}
		if err := h.reload(); err != nil {
			log.Printf("control %d: added %s but reload failed: %v", uid, entry.Number, err)
			notify(sender, "control: added, restart needed", err.Error())
			return true
		}
		log.Printf("control %d: allow-listed %s (%s) as %s, read+draft only",
			uid, entry.Number, label, jid)
		notify(sender, "control: added "+label,
			fmt.Sprintf("%s (%s) is allow-listed for read and draft.\n\nSending to this conversation is not granted; that stays a hand edit.", entry.Number, label))

	case control.Remove:
		jid, err := client.Resolve(ctx, cmd.Number)
		if err != nil {
			notify(sender, "control: not removed", fmt.Sprintf("%s: %v", cmd.Number, err))
			return true
		}
		if err := config.RemoveManaged(cfg.StateDir, jid.String()); err != nil {
			log.Printf("control %d: %v", uid, err)
			notify(sender, "control: not removed", err.Error())
			return true
		}
		if err := h.reload(); err != nil {
			log.Printf("control %d: removed but reload failed: %v", uid, err)
		}
		log.Printf("control %d: removed %s", uid, cmd.Number)
		notify(sender, "control: removed", cmd.Number+" is no longer allow-listed.")
	}
	return true
}

// notify answers a control command, and never fails the command if it cannot.
func notify(sender mail.Config, subject, body string) {
	if err := sender.SendNotice(subject, body); err != nil {
		log.Printf("control: could not reply: %v", err)
	}
}

func containsFold(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), want) {
			return true
		}
	}
	return false
}

// deliver puts an approved candidate on WhatsApp, one bubble at a time.
//
// The checks are per bubble, not per candidate. A kill switch engaged halfway
// through a five-bubble reply should stop it halfway, and a quota reached on
// the third bubble should send two.
func deliver(ctx context.Context, st *store.Store, cfg *config.Config, client *wa.Client, id int64, conv config.Conversation) {
	to, err := types.ParseJID(conv.JID)
	if err != nil {
		log.Printf("candidate %d: unusable jid %q: %v", id, conv.JID, err)
		_ = st.Resolve(ctx, id, store.StateFailed, "unusable jid")
		return
	}
	bubbles, err := st.Bubbles(ctx, id)
	if err != nil {
		log.Printf("candidate %d: reading bubbles: %v", id, err)
		return
	}

	sent := 0
	for _, b := range bubbles {
		if b.Sent {
			continue // a resumed delivery never repeats what arrived
		}
		if err := store.CheckHalt(cfg.StateDir); err != nil {
			log.Printf("candidate %d: stopped at bubble %d: %v", id, b.Index+1, err)
			_ = st.Resolve(ctx, id, store.StatePartial, err.Error())
			return
		}
		if err := withinQuota(ctx, st, cfg, conv.JID); err != nil {
			log.Printf("candidate %d: stopped at bubble %d: %v", id, b.Index+1, err)
			_ = st.Resolve(ctx, id, store.StatePartial, err.Error())
			return
		}

		waID, err := client.SendText(ctx, to, b.Body, nil)
		if err != nil {
			log.Printf("candidate %d: bubble %d failed: %v", id, b.Index+1, err)
			_ = st.Resolve(ctx, id, store.StatePartial, err.Error())
			return
		}
		if err := st.MarkBubbleSent(ctx, id, b.Index, conv.JID, waID); err != nil {
			// Sent but unrecorded. Stop rather than risk resending it.
			log.Printf("candidate %d: bubble %d sent but not recorded: %v", id, b.Index+1, err)
			_ = st.Resolve(ctx, id, store.StateFailed, "sent but not recorded")
			return
		}
		sent++

		// A reply should not land as a burst.
		select {
		case <-time.After(time.Duration(cfg.Limits.BubblePauseMS) * time.Millisecond):
		case <-ctx.Done():
			_ = st.Resolve(ctx, id, store.StatePartial, "shutting down")
			return
		}
	}
	_ = st.Resolve(ctx, id, store.StateSent, "")
	log.Printf("candidate %d: sent %d bubble(s) to %s", id, sent, conv.Label)
}

// withinQuota enforces both caps, counted in bubbles (SEC-6).
func withinQuota(ctx context.Context, st *store.Store, cfg *config.Config, conversation string) error {
	hour, err := st.SendsSince(ctx, conversation, time.Now().Add(-time.Hour))
	if err != nil {
		return fmt.Errorf("quota unreadable: %w", err) // fail closed
	}
	if hour >= cfg.Limits.PerConversationPerHour {
		return fmt.Errorf("per-conversation cap reached: %d in the last hour", hour)
	}
	day, err := st.SendsSince(ctx, "", time.Now().Add(-24*time.Hour))
	if err != nil {
		return fmt.Errorf("quota unreadable: %w", err)
	}
	if day >= cfg.Limits.PerDayTotal {
		return fmt.Errorf("daily cap reached: %d in the last day", day)
	}
	return nil
}

// resend retries a message that was recorded but never forwarded. The token is
// derived from the binding, so re-issuing yields the one already promised.
func resend(ctx context.Context, st *store.Store, cfg *config.Config, sender mail.Config, p store.Pending) {
	tok, err := st.IssueToken(ctx, cfg.HMACKey, p.Conversation, p.MessageID)
	if err != nil {
		log.Printf("retry %s: issuing a token: %v", p.MessageID, err)
		return
	}
	conv, ok := cfg.Lookup(p.Conversation)
	if !ok {
		return // no longer allow-listed; leave it unforwarded
	}
	number := phone.Digits(conv.Number)
	name := conv.Label
	if name == "" {
		name = "+" + number
	}
	if err := sender.Send(mail.Forward{
		Number: number, DisplayName: name, Token: tok,
		MessageID: p.MessageID, HMAC: tok,
		Timestamp: p.Received, Body: p.Body,
	}); err != nil {
		log.Printf("retry %s: %v", p.MessageID, err)
		return
	}
	if err := st.MarkForwarded(ctx, p.MessageID); err != nil {
		log.Printf("retry %s: marking forwarded: %v", p.MessageID, err)
	}
	log.Printf("forwarded %s (retry) from %s as [wa:%s]", p.MessageID, name, tok)
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
