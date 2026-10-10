package mailbox_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/mailbox"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/secretbox"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/receipt"
)

// fakeReader stands for the IMAP server: what it was asked, what it hands.
type fakeReader struct {
	password        string
	after, validity uint32
	answer          uint32
	letters         []mailbox.Letter
	err             error
}

func (f *fakeReader) Read(_ context.Context, _ mailbox.Settings, password string, after, validity uint32) (uint32, []mailbox.Letter, error) {
	f.password, f.after, f.validity = password, after, validity
	return f.answer, f.letters, f.err
}

// A letter made up in the shape OFD letters come in, not a family's.
const letter = "From: noreply@ofd.example\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" +
	`<a href="https://check.ofd.example/rec?t=20261009T1915&amp;s=2340.90&amp;fn=7380440700123456&amp;i=51243&amp;fp=1234567890&amp;n=1">Чек</a>`

// The box is stated with its app password sealed; a reading takes the new
// letters' receipts and remembers where it stopped; a refusal is noted on the
// box; another box is read from its first letter.
func TestTheMailboxIsReadForReceipts(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	fam := family.NewStore(pool)
	u, err := fam.CreateUser(ctx, "alex", "A", "h")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := fam.CreateSpaceWithOwner(ctx, "S", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(bytes.Repeat([]byte{7}, secretbox.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	opStore := operation.NewStore(pool)
	receipts := receipt.NewService(pool, opStore, account.NewStore(pool), category.NewStore(pool), operation.NewService(opStore))
	reader := &fakeReader{answer: 7, letters: []mailbox.Letter{{UID: 5, Raw: []byte(letter)}}}
	svc := mailbox.NewService(pool, box, reader, receipts, slog.Default())

	set := mailbox.Settings{Host: "imap.yandex.ru", Port: 993, Username: "cheki@example.ru"}
	if _, err := svc.Set(ctx, sp.ID, set); !errors.Is(err, family.ErrValidation) {
		t.Errorf("a new box without its password = %v", err)
	}
	pw := "app-password-123"
	set.Password = &pw
	b, err := svc.Set(ctx, sp.ID, set)
	if err != nil {
		t.Fatal(err)
	}
	if b.Folder != "INBOX" || b.CheckedAt != nil {
		t.Errorf("box = %+v", b)
	}
	var sealed []byte
	if err := pool.QueryRow(ctx, `SELECT password_sealed FROM mailboxes WHERE space_id = $1`, sp.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(pw)) {
		t.Error("the password is stored as it is")
	}

	b, res, err := svc.Check(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reader.password != pw || reader.after != 0 || res.Found != 1 || res.Waiting != 1 || b.LastFound != 1 || b.Problem != mailbox.ProblemNone || b.CheckedAt == nil {
		t.Errorf("first reading: asked %q after %d, result %+v, box %+v", reader.password, reader.after, res, b)
	}
	reader.letters = nil
	if _, _, err := svc.Check(ctx, sp.ID); err != nil {
		t.Fatal(err)
	}
	if reader.after != 5 || reader.validity != 7 {
		t.Errorf("second reading after %d, validity %d; want 5 and 7", reader.after, reader.validity)
	}

	// Refused: noted, no error; the password kept on a restatement.
	reader.err = errors.New("NO [AUTHENTICATIONFAILED]")
	if b, _, err := svc.Check(ctx, sp.ID); err != nil || b.Problem != mailbox.ProblemRead {
		t.Errorf("a failed reading = %+v, %v", b, err)
	}
	reader.err = nil
	set.Password = nil
	set.Username = "другой@example.ru"
	if _, err := svc.Set(ctx, sp.ID, set); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Check(ctx, sp.ID); err != nil {
		t.Fatal(err)
	}
	if reader.password != pw || reader.after != 0 {
		t.Errorf("another box: asked %q after %d, want the kept password from the first letter", reader.password, reader.after)
	}
	if err := svc.Delete(ctx, sp.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, sp.ID); !errors.Is(err, mailbox.ErrNotFound) {
		t.Errorf("after delete = %v", err)
	}
	if _, err := svc.Set(ctx, sp.ID, mailbox.Settings{Host: "imap yandex", Port: 993, Username: "a@b", Password: &pw}); !errors.Is(err, family.ErrValidation) {
		t.Errorf("a bad host = %v", err)
	}
	noKey := mailbox.NewService(pool, nil, reader, receipts, slog.Default())
	if _, err := noKey.Set(ctx, sp.ID, mailbox.Settings{Host: "imap.yandex.ru", Port: 993, Username: "a@b", Password: &pw}); err == nil ||
		!strings.Contains(err.Error(), "encryption key") {
		t.Errorf("without a key = %v", err)
	}
}
