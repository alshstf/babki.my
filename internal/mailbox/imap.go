package mailbox

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

// maxLetterBytes is the most of one letter read: a receipt's letter is a few
// dozen kilobytes; attachments past this are not a receipt's text.
const maxLetterBytes = 5 << 20

// IMAP reads a box over IMAP with TLS, read-only: letters are not marked read
// or moved.
type IMAP struct {
	// Timeout bounds the whole reading.
	Timeout time.Duration
}

func (r IMAP) Read(ctx context.Context, s Settings, password string, after, validity uint32) (uint32, []Letter, error) {
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dialer := &net.Dialer{Timeout: 20 * time.Second}
	c, err := client.DialWithDialerTLS(dialer, net.JoinHostPort(s.Host, strconv.Itoa(s.Port)), &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12})
	if err != nil {
		return 0, nil, readError{ProblemConnect, err}
	}
	defer func() { _ = c.Logout() }()
	c.Timeout = time.Minute
	// The reading stops when ctx does: the connection is closed under it.
	stop := context.AfterFunc(ctx, func() { _ = c.Terminate() })
	defer stop()

	if err := c.Login(s.Username, password); err != nil {
		return 0, nil, readError{ProblemLogin, err}
	}
	box, err := c.Select(s.Folder, true)
	if err != nil {
		return 0, nil, readError{ProblemFolder, err}
	}
	if box.UidValidity != validity {
		after = 0
	}
	if box.Messages == 0 {
		return box.UidValidity, nil, nil
	}
	criteria := imap.NewSearchCriteria()
	criteria.Uid = new(imap.SeqSet)
	criteria.Uid.AddRange(after+1, 0)
	uids, err := c.UidSearch(criteria)
	if err != nil {
		return 0, nil, readError{ProblemRead, err}
	}
	// «after+1:*» names the last letter even when it is not new.
	fresh := uids[:0]
	for _, u := range uids {
		if u > after {
			fresh = append(fresh, u)
		}
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i] < fresh[j] })
	if len(fresh) > maxLetters {
		fresh = fresh[len(fresh)-maxLetters:]
	}
	if len(fresh) == 0 {
		return box.UidValidity, nil, nil
	}
	set := new(imap.SeqSet)
	set.AddNum(fresh...)
	section := &imap.BodySectionName{Peek: true}
	messages := make(chan *imap.Message, 16)
	done := make(chan error, 1)
	go func() { done <- c.UidFetch(set, []imap.FetchItem{imap.FetchUid, section.FetchItem()}, messages) }()
	var letters []Letter
	for m := range messages {
		body := m.GetBody(section)
		if body == nil {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(body, maxLetterBytes))
		if err != nil {
			continue
		}
		letters = append(letters, Letter{UID: m.Uid, Raw: raw})
	}
	if err := <-done; err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return 0, nil, readError{ProblemRead, ctx.Err()}
		}
		return 0, nil, readError{ProblemRead, err}
	}
	return box.UidValidity, letters, nil
}
