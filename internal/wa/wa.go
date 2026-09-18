// Package wa is the WhatsApp side of the bridge: the whatsmeow client, the
// allowlist filter in front of it, and the send path behind the approval gate.
//
// Two of whatsmeow's behaviours are load-bearing here and both are handled at
// the event boundary rather than later:
//
//   - History sync arrives at pairing and carries recent messages from EVERY
//     chat, not just allow-listed ones. It is dropped unconditionally (SEC-9).
//   - Read receipts and typing indicators are never emitted, so reading leaves
//     no trace on the account (SEC-10).
package wa

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"github.com/arielpts/instinct-whatsapp-bridge/internal/phone"
)

// pairDisplayName must read as "Browser (OS)". WhatsApp validates it against a
// list of common browsers and rejects the pairing request with a bare 400 if it
// does not match -- so this is not a place to put the product name.
const pairDisplayName = "Chrome (Linux)"

var (
	ErrNotLinked  = errors.New("wa: no linked device; run `wa-bridge pair`")
	ErrNotOnWhats = errors.New("wa: no WhatsApp account for any candidate")
	ErrAmbiguous  = errors.New("wa: candidates resolve to different accounts")
)

// Inbound is one allow-listed message, already reduced to what the bridge
// forwards. Nothing else from the event survives this boundary.
type Inbound struct {
	MessageID    string
	Conversation string // canonical JID of the chat
	SenderName   string
	SenderJID    string
	Text         string
	Timestamp    time.Time
}

// Allower decides whether a chat may be read at all. Returning false means the
// message is dropped before its content is touched.
type Allower interface {
	Allowed(conversationJID string) bool
}

type Client struct {
	wm      *whatsmeow.Client
	allow   Allower
	onMsg   func(Inbound)
	log     waLog.Logger
	dropped uint64 // history-sync payloads discarded, for the audit line

	loggedIn  chan struct{}
	loginOnce sync.Once
}

// LoggedIn closes once the device is linked *and* the session that follows has
// come up.
//
// PairSuccess is not that moment. whatsmeow tears the socket down and logs in
// again afterwards, and the phone waits for that second connection before it
// considers the device attached -- disconnect on PairSuccess and the phone
// reports "could not link device" for a pairing the server already accepted.
func (c *Client) LoggedIn() <-chan struct{} { return c.loggedIn }

func (c *Client) markLoggedIn() { c.loginOnce.Do(func() { close(c.loggedIn) }) }

// Open builds a client over an existing database handle.
//
// whatsmeow's own examples open the database themselves with the cgo sqlite3
// driver. We hand it a handle opened with the pure-Go driver instead and tell
// it the dialect, which keeps CGO_ENABLED=0 and the arm64 cross-build intact.
func Open(ctx context.Context, db *sql.DB, logLevel string) (*Client, error) {
	log := waLog.Stdout("wa", logLevel, false)
	container := sqlstore.NewWithDB(db, "sqlite3", log)
	if err := container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("wa: upgrading device store: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("wa: device store: %w", err)
	}
	c := &Client{wm: whatsmeow.NewClient(device, log), log: log, loggedIn: make(chan struct{})}
	c.wm.AddEventHandler(c.handle)
	return c, nil
}

// OnMessage registers the sink for allow-listed inbound text, and the
// allowlist that guards it. Both are required before connecting.
func (c *Client) OnMessage(allow Allower, fn func(Inbound)) {
	c.allow, c.onMsg = allow, fn
}

func (c *Client) Connect(ctx context.Context) error { return c.wm.Connect() }
func (c *Client) Disconnect()                       { c.wm.Disconnect() }

// LinkedJID reports the account this box is linked to, or ErrNotLinked.
func (c *Client) LinkedJID() (types.JID, error) {
	if c.wm.Store.ID == nil {
		return types.JID{}, ErrNotLinked
	}
	return *c.wm.Store.ID, nil
}

// DroppedHistorySyncs is the count of history-sync payloads discarded, so the
// audit log can show the drop happening rather than assert it.
func (c *Client) DroppedHistorySyncs() uint64 { return c.dropped }

// PairQR returns the channel of QR codes to render for linking. Pairing needs
// the phone in hand, which is worth knowing before the session drops.
func (c *Client) PairQR(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	if c.wm.Store.ID != nil {
		return nil, errors.New("wa: already linked; delete the device store to re-pair")
	}
	return c.wm.GetQRChannel(ctx)
}

// PairCode links by an eight-character code typed into WhatsApp instead of a
// scanned QR.
//
// This is the right path when the operator has one screen. A QR shown in a
// terminal on the same phone that runs WhatsApp cannot be scanned by that
// phone, so QR pairing quietly assumes a second screen that may not exist.
//
// whatsmeow requires the websocket to be up first, and the login socket closes
// after about 160 seconds, so the code is requested immediately after
// connecting to leave the operator the most time to type it.
// It connects on your behalf: the QR channel has to be opened before the
// socket, and the first item on it is the signal that the connection is
// established enough to mint a code. Sleeping instead races the server and
// gets a bare 400 back.
func (c *Client) PairCode(ctx context.Context, number string) (string, error) {
	if c.wm.Store.ID != nil {
		return "", errors.New("wa: already linked; delete the device store to re-pair")
	}
	digits := phone.Digits(number)
	if digits == "" {
		return "", fmt.Errorf("wa: %q has no digits", number)
	}

	qr, err := c.wm.GetQRChannel(ctx)
	if err != nil {
		return "", fmt.Errorf("wa: opening the login channel: %w", err)
	}
	if err := c.wm.Connect(); err != nil {
		return "", fmt.Errorf("wa: connecting: %w", err)
	}
	select {
	case _, ok := <-qr:
		if !ok {
			return "", errors.New("wa: the login socket closed before pairing could start")
		}
	case <-time.After(30 * time.Second):
		return "", errors.New("wa: the server never offered a login channel")
	case <-ctx.Done():
		return "", ctx.Err()
	}

	// The number goes through untouched. whatsmeow strips punctuation itself,
	// and the ninth-digit candidates are for finding *other* people's accounts
	// -- the operator knows which number is their own, and reshaping it here
	// would pair the wrong one.
	code, err := c.wm.PairPhone(ctx, number, true, whatsmeow.PairClientChrome, pairDisplayName)
	if err != nil {
		return "", annotatePairError(err, digits)
	}
	return code, nil
}

// annotatePairError turns WhatsApp's contentless 400 into something actionable.
//
// The server rejects a pairing request for a number that has no account with
// the same bare status it uses for a malformed one. For a Brazilian mobile
// written without its ninth digit that is the likeliest cause by far, so say
// so rather than leaving the operator to stare at "bad-request".
func annotatePairError(err error, digits string) error {
	alt := phone.NinthDigitVariant(digits)
	if alt == "" {
		return fmt.Errorf("wa: requesting a pairing code: %w", err)
	}
	return fmt.Errorf("wa: requesting a pairing code: %w\n"+
		"      WhatsApp returns this for a number it has no account for.\n"+
		"      This looks Brazilian, so try the other ninth-digit form: +%s", err, alt)
}

func (c *Client) handle(evt any) {
	switch e := evt.(type) {

	case *events.HistorySync:
		// Everything in here is other people's messages from chats we were
		// never granted. There is no filtering step that makes it acceptable
		// to keep, so it is not kept (SEC-9).
		c.dropped++
		c.log.Infof("discarded history sync (%d so far); it is not ours to read", c.dropped)

	case *events.Message:
		c.handleMessage(e)

	case *events.PairSuccess:
		// Accepted, but not finished: the login below is what the phone waits for.
		c.log.Infof("paired with %s; waiting for the session to come up", e.ID)

	case *events.Connected:
		if c.wm.Store.ID != nil {
			c.markLoggedIn()
		}

	case *events.LoggedOut:
		// Degrade to "forwards stop", loudly. Silent inactivity is the failure
		// mode that goes unnoticed for a week (OPS-5).
		c.log.Errorf("device was unlinked: %s -- forwarding has stopped until re-paired", e.Reason)
	}
}

func (c *Client) handleMessage(e *events.Message) {
	// Groups are a non-goal: forwarding one exposes third parties who never
	// agreed to any of this.
	if e.Info.IsGroup || e.Info.IsFromMe {
		return
	}
	chat, err := phone.ParseJID(e.Info.Chat.String())
	if err != nil {
		return // groups and unknown servers never reach the allowlist
	}
	if c.allow == nil || !c.allow.Allowed(chat.String()) {
		return
	}
	text := extractText(e.Message)
	if text == "" {
		return // media and other non-text are out of scope for v1
	}
	sender, err := phone.ParseJID(e.Info.Sender.String())
	if err != nil {
		return
	}
	if c.onMsg != nil {
		c.onMsg(Inbound{
			MessageID:    string(e.Info.ID),
			Conversation: chat.String(),
			SenderName:   e.Info.PushName,
			SenderJID:    sender.String(),
			Text:         text,
			Timestamp:    e.Info.Timestamp,
		})
	}
}

// extractText pulls the plain body out of the message shapes that carry one.
// Anything else returns empty and is skipped rather than guessed at.
func extractText(m *waE2E.Message) string {
	if m == nil {
		return ""
	}
	if s := m.GetConversation(); s != "" {
		return s
	}
	if ext := m.GetExtendedTextMessage(); ext != nil {
		return ext.GetText()
	}
	return ""
}

// Resolve answers which account a number really belongs to.
//
// Candidates come from phone.Candidates, which for Brazilian mobiles offers
// the number with and without the ninth digit. WhatsApp decides; we never do.
// Zero hits, or hits that disagree, fail loudly so a human picks (FR-10).
func (c *Client) Resolve(ctx context.Context, number string) (types.JID, error) {
	candidates, err := phone.Candidates(number)
	if err != nil {
		return types.JID{}, err
	}
	queries := make([]string, len(candidates))
	for i, d := range candidates {
		queries[i] = "+" + d
	}
	responses, err := c.wm.IsOnWhatsApp(ctx, queries)
	if err != nil {
		return types.JID{}, fmt.Errorf("wa: checking %v: %w", queries, err)
	}

	var found []types.JID
	for _, r := range responses {
		if !r.IsIn {
			continue
		}
		if !containsJID(found, r.JID) {
			found = append(found, r.JID)
		}
	}
	switch len(found) {
	case 0:
		return types.JID{}, fmt.Errorf("%w: tried %v", ErrNotOnWhats, queries)
	case 1:
		return found[0], nil
	default:
		return types.JID{}, fmt.Errorf("%w: %v", ErrAmbiguous, found)
	}
}

func containsJID(list []types.JID, j types.JID) bool {
	for _, v := range list {
		if v.User == j.User && v.Server == j.Server {
			return true
		}
	}
	return false
}

// SendText delivers one bubble. replyTo, when set, quotes the message being
// answered.
//
// This is the only path to WhatsApp in the binary, and it is deliberately
// dumb: every check that decides whether a message may go out has already
// happened by the time it is called.
func (c *Client) SendText(ctx context.Context, to types.JID, text string, replyTo *Inbound) (string, error) {
	msg := &waE2E.Message{Conversation: proto.String(text)}
	if replyTo != nil {
		participant, err := types.ParseJID(replyTo.SenderJID)
		if err != nil {
			return "", fmt.Errorf("wa: reply target: %w", err)
		}
		msg = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(text),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:    proto.String(replyTo.MessageID),
				Participant: proto.String(participant.String()),
			},
		}}
	}
	resp, err := c.wm.SendMessage(ctx, to, msg)
	if err != nil {
		return "", fmt.Errorf("wa: send: %w", err)
	}
	return resp.ID, nil
}
