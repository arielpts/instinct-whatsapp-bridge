package mail

import (
	"fmt"
	"net"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// Inbox is the reading half of the mail settings.
type Inbox struct {
	Host     string
	Port     int
	User     string
	Password string
	Mailbox  string // usually INBOX
}

func (i Inbox) addr() string {
	port := i.Port
	if port == 0 {
		port = 993
	}
	return net.JoinHostPort(i.Host, fmt.Sprint(port))
}

// Message is one fetched mail, with the UID needed to mark it seen.
type Message struct {
	UID uint32
	Raw []byte
}

// FetchUnseen returns unread messages and leaves them unread.
//
// Marking happens only after the bridge has decided what to do with a message
// (MarkSeen), so a crash mid-processing means the message is fetched again
// rather than silently skipped. Deduplication in the store is what stops that
// becoming a second candidate.
func (i Inbox) FetchUnseen(limit int) ([]Message, error) {
	c, err := i.dial()
	if err != nil {
		return nil, err
	}
	defer c.Logout().Wait()

	mailbox := i.Mailbox
	if mailbox == "" {
		mailbox = "INBOX"
	}
	if _, err := c.Select(mailbox, nil).Wait(); err != nil {
		return nil, fmt.Errorf("mail: selecting %s: %w", mailbox, err)
	}

	data, err := c.UIDSearch(&imap.SearchCriteria{
		NotFlag: []imap.Flag{imap.FlagSeen},
	}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("mail: searching: %w", err)
	}
	uids := data.AllUIDs()
	if len(uids) == 0 {
		return nil, nil
	}
	if limit > 0 && len(uids) > limit {
		uids = uids[:limit]
	}

	var set imap.UIDSet
	for _, uid := range uids {
		set.AddNum(uid)
	}
	// PEEK, so fetching does not mark anything read behind our back.
	buffers, err := c.Fetch(set, &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{{Peek: true}},
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("mail: fetching: %w", err)
	}

	out := make([]Message, 0, len(buffers))
	for _, b := range buffers {
		for _, section := range b.BodySection {
			out = append(out, Message{UID: uint32(b.UID), Raw: section.Bytes})
			break
		}
	}
	return out, nil
}

// MarkSeen flags messages as read, which is how the bridge remembers it has
// dealt with them.
func (i Inbox) MarkSeen(uids []uint32) error {
	if len(uids) == 0 {
		return nil
	}
	c, err := i.dial()
	if err != nil {
		return err
	}
	defer c.Logout().Wait()

	mailbox := i.Mailbox
	if mailbox == "" {
		mailbox = "INBOX"
	}
	if _, err := c.Select(mailbox, nil).Wait(); err != nil {
		return fmt.Errorf("mail: selecting %s: %w", mailbox, err)
	}
	var set imap.UIDSet
	for _, uid := range uids {
		set.AddNum(imap.UID(uid))
	}
	cmd := c.Store(set, &imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.FlagSeen},
	}, nil)
	return cmd.Close()
}

func (i Inbox) dial() (*imapclient.Client, error) {
	c, err := imapclient.DialTLS(i.addr(), &imapclient.Options{
		Dialer: &net.Dialer{Timeout: 20 * time.Second},
	})
	if err != nil {
		return nil, fmt.Errorf("mail: connecting to %s: %w", i.addr(), err)
	}
	if err := c.Login(i.User, i.Password).Wait(); err != nil {
		c.Close()
		return nil, fmt.Errorf("mail: logging in as %s: %w", i.User, err)
	}
	return c, nil
}
