package receipt

import (
	"strings"
	"testing"
	"time"
)

// Letters made up in the shapes OFD letters come in, not a family's.
func TestALettersReceiptIsRead(t *testing.T) {
	// An HTML letter whose «check the receipt» link carries the QR line,
	// quoted-printable, beside a plain part.
	htmlLetter := strings.Join([]string{
		"From: noreply@ofd.example", "To: cheki@example.ru", "Subject: =?utf-8?B?0JrQsNGB0YHQvtCy0YvQuSDRh9C10Lo=?=",
		"MIME-Version: 1.0", `Content-Type: multipart/alternative; boundary="b1"`, "",
		"--b1", "Content-Type: text/plain; charset=utf-8", "", "Ваш чек во вложении.",
		"--b1", "Content-Type: text/html; charset=utf-8", "Content-Transfer-Encoding: quoted-printable", "",
		`<p>Спасибо за покупку!</p><a href=3D"https://check.ofd.example/rec?t=3D20261009T191500&amp;s=3D2340.90&amp;fn=3D73804407001=`,
		`23456&amp;i=3D51243&amp;fp=3D1234567890&amp;n=3D1">Проверить чек</a>`,
		"--b1--", "",
	}, "\r\n")
	got, err := ParseMail([]byte(htmlLetter))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("receipts = %+v", got)
	}
	r := got[0]
	if r.FN != "7380440700123456" || r.FD != "51243" || r.FP == nil || *r.FP != "1234567890" || r.Total != 234090 ||
		!r.IssuedAt.Equal(time.Date(2026, 10, 9, 19, 15, 0, 0, time.UTC)) || r.Kind != Purchase || r.Source != SourceMail {
		t.Errorf("receipt = %+v", r)
	}

	// A plain letter in windows-1251 naming the numbers in its text.
	text := "Кассовый чек. Возврат прихода\r\n08.10.2026 12:00\r\nИТОГ: 1 500,00\r\nФН: 9282440300810826 ФД: 777 ФПД: 3520412234\r\n"
	plain := "From: shop@example\r\nContent-Type: text/plain; charset=windows-1251\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" + cp1251(text)
	got, err = ParseMail([]byte(plain))
	if err != nil || len(got) != 1 {
		t.Fatalf("plain = %+v, %v", got, err)
	}
	if r := got[0]; r.FN != "9282440300810826" || r.FD != "777" || r.Total != 150000 || r.Kind != Refund ||
		!r.IssuedAt.Equal(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)) || r.FP == nil || *r.FP != "3520412234" {
		t.Errorf("plain receipt = %+v", r)
	}

	// A letter that is no receipt's.
	if got, err := ParseMail([]byte("From: a@b\r\nContent-Type: text/plain\r\n\r\nПривет! Встречаемся в 12:00.\r\n")); err != nil || len(got) != 0 {
		t.Errorf("not a receipt = %+v, %v", got, err)
	}
}

// cp1251 writes Cyrillic text in windows-1251, as some tills' letters come.
func cp1251(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'А' && r <= 'я':
			b.WriteByte(byte(r - 'А' + 0xC0))
		case r == 'Ё':
			b.WriteByte(0xA8)
		case r == 'ё':
			b.WriteByte(0xB8)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
