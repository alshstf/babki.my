// Package mailbox reads a family's mailbox for receipts (decision Р-35, А):
// a box kept for receipts alone, the family's mail forwarding the shops' and
// OFD letters there. The program reads that box only — over IMAP with TLS,
// by an app password sealed with the encryption key, never shown again — and
// each new letter's receipts go to internal/receipt as a statement's do.
package mailbox

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/receipt"
)

// Problem names what went wrong the last time the box was read.
type Problem string

const (
	ProblemNone    Problem = ""
	ProblemConnect Problem = "connect"
	ProblemLogin   Problem = "login"
	ProblemFolder  Problem = "folder"
	ProblemRead    Problem = "read"
)

// Box is a space's mailbox as the program shows it: where and as whom it is
// read, never the password; when it was read last, what was wrong then and
// how many receipts its new letters brought.
type Box struct {
	Host      string
	Port      int
	Username  string
	Folder    string
	CheckedAt *time.Time
	Problem   Problem
	LastFound int
}

// Settings are a box as stated; Password nil keeps the one stored.
type Settings struct {
	Host     string
	Port     int
	Username string
	Folder   string
	Password *string
}

var hostRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.-]{0,253}[a-zA-Z0-9])?$`)

// clean trims the settings and refuses what no IMAP server answers to.
func (s Settings) clean() (Settings, error) {
	s.Host, s.Username, s.Folder = strings.TrimSpace(s.Host), strings.TrimSpace(s.Username), strings.TrimSpace(s.Folder)
	if s.Folder == "" {
		s.Folder = "INBOX"
	}
	switch {
	case !hostRe.MatchString(s.Host):
		return Settings{}, fmt.Errorf("%w: the server is a host name like imap.yandex.ru", family.ErrValidation)
	case s.Port < 1 || s.Port > 65535:
		return Settings{}, fmt.Errorf("%w: the port is 1 to 65535 (IMAP with TLS: 993)", family.ErrValidation)
	case s.Username == "" || utf8.RuneCountInString(s.Username) > 320:
		return Settings{}, fmt.Errorf("%w: the login is the box's address", family.ErrValidation)
	case utf8.RuneCountInString(s.Folder) > 255:
		return Settings{}, fmt.Errorf("%w: the folder's name is at most 255 characters", family.ErrValidation)
	case s.Password != nil && (*s.Password == "" || len(*s.Password) > 1024):
		return Settings{}, fmt.Errorf("%w: the app password is 1 to 1024 characters", family.ErrValidation)
	}
	return s, nil
}

// Letter is one letter read off the box, by its UID.
type Letter struct {
	UID uint32
	Raw []byte
}

// Reader reads a box's letters after UID after: the box's UID validity, and
// the letters, at most maxLetters of the newest. A validity other than the one
// given means the box was rebuilt and every letter is new again.
type Reader interface {
	Read(ctx context.Context, s Settings, password string, after, validity uint32) (newValidity uint32, letters []Letter, err error)
}

// maxLetters is how many letters one reading takes, the newest.
const maxLetters = 300

// readError is a reading's failure with what to tell the family.
type readError struct {
	problem Problem
	err     error
}

func (e readError) Error() string { return string(e.problem) + ": " + e.err.Error() }
func (e readError) Unwrap() error { return e.err }

// importer takes the receipts the letters held.
type importer interface {
	Import(ctx context.Context, spaceID uuid.UUID, receipts []receipt.Receipt) (receipt.ImportResult, error)
}
