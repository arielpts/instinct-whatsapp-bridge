// Package store is the bridge's durable state: issued tokens, what we have
// already forwarded, outbound candidates and their bubbles, and the quota
// ledger.
//
// It uses the pure-Go SQLite driver so the binary builds with CGO_ENABLED=0
// and cross-compiles to arm64.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/arielpts/instinct-whatsapp-bridge/internal/token"
)

//go:embed schema.sql
var schema string

var (
	ErrUnknownToken  = errors.New("store: no such token")
	ErrTokenRedeemed = errors.New("store: token already redeemed")
	ErrDuplicate     = errors.New("store: already seen")
	ErrWrongDomain   = errors.New("store: state belongs to a different mail domain")
)

// Candidate states.
const (
	StatePending  = "pending"
	StateApproved = "approved"
	StateSent     = "sent"
	StatePartial  = "partial"
	StateRejected = "rejected"
	StateExpired  = "expired"
	StateFailed   = "failed"
)

type Store struct {
	db     *sql.DB
	domain string
	now    func() time.Time
}

// Open prepares the database and binds it to a mail domain.
//
// The domain is recorded on first use and checked on every later open. Tokens
// and addresses are scoped to a domain, so opening yesterday's state under a
// new one would let old tokens redeem against new addresses.
func Open(ctx context.Context, path, domain string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// Not one connection: whatsmeow shares this handle and issues queries
	// inside its own transactions, which on a single connection deadlocks.
	// WAL plus busy_timeout handles the concurrency instead.
	db.SetMaxOpenConns(8)

	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: schema: %w", err)
	}

	s := &Store{db: db, domain: domain, now: time.Now}
	if err := s.bindDomain(ctx, domain); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// bindDomain ties the state to a mail domain, once one exists.
//
// The operator commands run before any mail is configured, so an empty domain
// has to mean "not yet" rather than a domain literally named "". Binding the
// empty string would make the database refuse to open the moment a real domain
// was configured -- taking the linked device with it, since the pairing lives
// in the same file.
func (s *Store) bindDomain(ctx context.Context, domain string) error {
	var stored string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'mail_domain'`).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if domain == "" {
			return nil // nothing to bind yet
		}
		_, err = s.db.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('mail_domain', ?)`, domain)
		return err
	case err != nil:
		return err
	case stored == domain:
		return nil
	case domain == "":
		// An operator command on a box that does have a domain: adopt it, so
		// tokens stay scoped correctly.
		s.domain = stored
		return nil
	case stored == "":
		// Bound before mail existed. Adopt the real domain now.
		_, err = s.db.ExecContext(ctx, `UPDATE meta SET value = ? WHERE key = 'mail_domain'`, domain)
		return err
	default:
		return fmt.Errorf("%w: state is %s, configured %s", ErrWrongDomain, stored, domain)
	}
}

func (s *Store) Close() error { return s.db.Close() }

// SQL exposes the handle so whatsmeow's device store can live in the same
// file: one database to back up, one to restore.
func (s *Store) SQL() *sql.DB { return s.db }

func (s *Store) unix() int64 { return s.now().Unix() }

// IssueToken records a binding and returns the token that stands for it.
func (s *Store) IssueToken(ctx context.Context, key []byte, conversation, message string) (string, error) {
	b := token.Binding{
		Conversation: conversation,
		Message:      message,
		Domain:       s.domain,
		IssuedUnix:   s.unix(),
	}
	t := token.Issue(key, b)
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO tokens (token, conversation, message, domain, issued_unix)
		 VALUES (?, ?, ?, ?, ?)`,
		t, b.Conversation, b.Message, b.Domain, b.IssuedUnix)
	if err != nil {
		return "", fmt.Errorf("store: issue token: %w", err)
	}
	return t, nil
}

// RedeemToken spends a token once and returns what it bound.
//
// Verification re-derives the HMAC from the stored binding rather than
// trusting that the row was found: a token is never valid merely because it is
// in the table.
func (s *Store) RedeemToken(ctx context.Context, key []byte, t string) (token.Binding, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return token.Binding{}, err
	}
	defer tx.Rollback()

	var b token.Binding
	var redeemed sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT conversation, message, domain, issued_unix, redeemed_unix FROM tokens WHERE token = ?`, t).
		Scan(&b.Conversation, &b.Message, &b.Domain, &b.IssuedUnix, &redeemed)
	if errors.Is(err, sql.ErrNoRows) {
		return token.Binding{}, ErrUnknownToken
	}
	if err != nil {
		return token.Binding{}, err
	}
	if redeemed.Valid {
		return token.Binding{}, fmt.Errorf("%w at %d", ErrTokenRedeemed, redeemed.Int64)
	}
	if b.Domain != s.domain {
		return token.Binding{}, fmt.Errorf("%w: token is for %s", ErrWrongDomain, b.Domain)
	}
	if !token.Verify(key, t, b) {
		return token.Binding{}, ErrUnknownToken
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tokens SET redeemed_unix = ? WHERE token = ?`, s.unix(), t); err != nil {
		return token.Binding{}, err
	}
	return b, tx.Commit()
}

// RecordForward notes an inbound message. ErrDuplicate means WhatsApp
// redelivered something we have already handled.
func (s *Store) RecordForward(ctx context.Context, messageID, conversation, sender, body string) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO forwards (message_id, conversation, sender, body, received_unix)
		 VALUES (?, ?, ?, ?, ?)`,
		messageID, conversation, sender, body, s.unix())
	if err != nil {
		return fmt.Errorf("store: record forward: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrDuplicate
	}
	return nil
}

// Pending lists messages recorded but never forwarded.
//
// Recording precedes sending, so a failed send leaves a row behind. Without
// this the message is simply lost: WhatsApp considers it delivered, and
// deduplication stops it ever being handled again. A transient SMTP failure
// must not be a silent deletion.
type Pending struct {
	MessageID    string
	Conversation string
	Sender       string
	Body         string
	Received     time.Time
}

func (s *Store) Pending(ctx context.Context, olderThan time.Time, limit int) ([]Pending, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT message_id, conversation, sender, body, received_unix
		   FROM forwards
		  WHERE forwarded_unix IS NULL AND body IS NOT NULL AND received_unix < ?
		  ORDER BY received_unix LIMIT ?`, olderThan.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pending
	for rows.Next() {
		var p Pending
		var received int64
		if err := rows.Scan(&p.MessageID, &p.Conversation, &p.Sender, &p.Body, &received); err != nil {
			return nil, err
		}
		p.Received = time.Unix(received, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkForwarded records that the email went out.
func (s *Store) MarkForwarded(ctx context.Context, messageID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE forwards SET forwarded_unix = ? WHERE message_id = ?`, s.unix(), messageID)
	return err
}

// CreateCandidate stores a reply and its bubbles as one unit. A second call
// with the same mail Message-ID returns ErrDuplicate rather than queueing the
// message twice (FR-9).
func (s *Store) CreateCandidate(ctx context.Context, mailMessageID, tok, conversation string, bubbles []string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO candidates (mail_message_id, token, conversation, state, created_unix)
		 VALUES (?, ?, ?, ?, ?)`,
		mailMessageID, tok, conversation, StatePending, s.unix())
	if err != nil {
		return 0, fmt.Errorf("store: create candidate: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrDuplicate
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for i, body := range bubbles {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO bubbles (candidate_id, idx, body) VALUES (?, ?, ?)`, id, i, body); err != nil {
			return 0, fmt.Errorf("store: bubble %d: %w", i, err)
		}
	}
	return id, tx.Commit()
}

// Bubbles returns a candidate's messages in order, with those already
// delivered marked, so a resumed delivery skips them instead of repeating them.
type Bubble struct {
	Index int
	Body  string
	Sent  bool
}

func (s *Store) Bubbles(ctx context.Context, candidateID int64) ([]Bubble, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT idx, body, sent_unix IS NOT NULL FROM bubbles WHERE candidate_id = ? ORDER BY idx`, candidateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bubble
	for rows.Next() {
		var b Bubble
		if err := rows.Scan(&b.Index, &b.Body, &b.Sent); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// MarkBubbleSent records a delivered bubble and charges it to the quota, in
// one transaction: a bubble that went out but was not counted would let the
// cap drift upward.
func (s *Store) MarkBubbleSent(ctx context.Context, candidateID int64, idx int, conversation, waMessageID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := s.unix()
	if _, err := tx.ExecContext(ctx,
		`UPDATE bubbles SET sent_unix = ?, wa_message_id = ? WHERE candidate_id = ? AND idx = ?`,
		now, waMessageID, candidateID, idx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sends (conversation, sent_unix) VALUES (?, ?)`, conversation, now); err != nil {
		return err
	}
	return tx.Commit()
}

// Resolve moves a candidate to a terminal state.
func (s *Store) Resolve(ctx context.Context, candidateID int64, state, reason string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE candidates SET state = ?, resolved_unix = ?, reason = ? WHERE id = ?`,
		state, s.unix(), reason, candidateID)
	return err
}

func (s *Store) State(ctx context.Context, candidateID int64) (string, error) {
	var state string
	err := s.db.QueryRowContext(ctx, `SELECT state FROM candidates WHERE id = ?`, candidateID).Scan(&state)
	return state, err
}

// SendsSince counts delivered bubbles, for a conversation or (empty string)
// across all of them.
func (s *Store) SendsSince(ctx context.Context, conversation string, since time.Time) (int, error) {
	var n int
	var err error
	if conversation == "" {
		err = s.db.QueryRowContext(ctx,
			`SELECT count(*) FROM sends WHERE sent_unix >= ?`, since.Unix()).Scan(&n)
	} else {
		err = s.db.QueryRowContext(ctx,
			`SELECT count(*) FROM sends WHERE conversation = ? AND sent_unix >= ?`,
			conversation, since.Unix()).Scan(&n)
	}
	return n, err
}

// PruneContacts deletes whatsmeow's contact rows for anyone not allow-listed,
// returning how many went.
//
// Dropping the *events.HistorySync payload is not enough on its own.
// whatsmeow processes that payload before our handler ever sees it, and writes
// push names into whatsmeow_contacts as it goes -- on a real account that is
// thousands of names from chats the bridge was never granted. Discarding the
// event stops us reading them; it does not stop them being on disk.
//
// Names are the identifying part, so they are what we remove. Run it after
// every sync, not once.
func (s *Store) PruneContacts(ctx context.Context, keep []string) (int64, error) {
	query := `DELETE FROM whatsmeow_contacts`
	args := make([]any, 0, len(keep))
	if len(keep) > 0 {
		placeholders := make([]string, len(keep))
		for i, jid := range keep {
			placeholders[i] = "?"
			args = append(args, jid)
		}
		query += ` WHERE their_jid NOT IN (` + strings.Join(placeholders, ",") + `)`
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		// The table only exists once whatsmeow has run its migrations.
		if strings.Contains(err.Error(), "no such table") {
			return 0, nil
		}
		return 0, fmt.Errorf("store: pruning contacts: %w", err)
	}
	return res.RowsAffected()
}

// ContactCount reports how many contact rows are stored, so the operator can
// see the number rather than take a claim on trust.
func (s *Store) ContactCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM whatsmeow_contacts`).Scan(&n)
	if err != nil && strings.Contains(err.Error(), "no such table") {
		return 0, nil
	}
	return n, err
}

// PurgeBodies drops message content past the retention window while keeping
// the identifiers that make duplicates recognisable (SEC-7).
func (s *Store) PurgeBodies(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE forwards SET body = NULL WHERE body IS NOT NULL AND received_unix < ?`, olderThan.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ExpirePending fails candidates nobody approved in time, so an unanswered
// draft cannot be delivered hours later out of context.
func (s *Store) ExpirePending(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE candidates SET state = ?, resolved_unix = ?, reason = 'not approved in time'
		 WHERE state = ? AND created_unix < ?`,
		StateExpired, s.unix(), StatePending, olderThan.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
