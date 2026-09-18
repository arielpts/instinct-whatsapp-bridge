package mail

import (
	"fmt"
	"io"
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

	// Debug receives the IMAP protocol trace when set. It includes
	// credentials, so it is opt-in and meant for a person watching a log.
	Debug io.Writer
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

// Poll fetches unseen mail, hands each message to decide, and marks seen only
// those it returns true for.
//
// One connection does all of it. Dialling separately to fetch and to mark cost
// two logins per cycle and left a window where a message was handled but not
// yet flagged; a single session closes both.
//
// Messages are fetched with PEEK so reading marks nothing by itself, and a
// message decide rejects stays unread, to be tried again rather than lost.
func (i Inbox) Poll(limit int, decide func(Message) bool) error {
	c, err := i.dial()
	if err != nil {
		return err
	}
	defer c.Close()

	mailbox := i.Mailbox
	if mailbox == "" {
		mailbox = "INBOX"
	}
	if _, err := c.Select(mailbox, nil).Wait(); err != nil {
		return fmt.Errorf("mail: selecting %s: %w", mailbox, err)
	}

	data, err := c.UIDSearch(&imap.SearchCriteria{
		NotFlag: []imap.Flag{imap.FlagSeen},
	}, nil).Wait()
	if err != nil {
		return fmt.Errorf("mail: searching: %w", err)
	}
	uids := data.AllUIDs()
	if len(uids) == 0 {
		return c.Logout().Wait()
	}
	if limit > 0 && len(uids) > limit {
		uids = uids[:limit]
	}

	var set imap.UIDSet
	for _, uid := range uids {
		set.AddNum(uid)
	}
	buffers, err := c.Fetch(set, &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{{Peek: true}},
	}).Collect()
	if err != nil {
		return fmt.Errorf("mail: fetching: %w", err)
	}

	var handled imap.UIDSet
	for _, b := range buffers {
		for _, section := range b.BodySection {
			if decide(Message{UID: uint32(b.UID), Raw: section.Bytes}) {
				handled.AddNum(b.UID)
			}
			break
		}
	}
	if len(handled) > 0 {
		cmd := c.Store(handled, &imap.StoreFlags{
			Op:    imap.StoreFlagsAdd,
			Flags: []imap.Flag{imap.FlagSeen},
		}, nil)
		if err := cmd.Close(); err != nil {
			return fmt.Errorf("mail: marking seen: %w", err)
		}
	}
	return c.Logout().Wait()
}

func (i Inbox) dial() (*imapclient.Client, error) {
	c, err := imapclient.DialTLS(i.addr(), &imapclient.Options{
		Dialer:      &net.Dialer{Timeout: 20 * time.Second},
		DebugWriter: i.Debug,
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
