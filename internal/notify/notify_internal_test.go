package notify

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/creditcard"
	"babki.my/babki/internal/platform/secretbox"
)

func d(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

// The push keys are the same while the encryption key is, differ with it,
// and the public one is a P-256 point a browser takes.
func TestThePushKeysComeFromTheEncryptionKey(t *testing.T) {
	one, _ := secretbox.New(bytes.Repeat([]byte{7}, secretbox.KeySize))
	two, _ := secretbox.New(bytes.Repeat([]byte{8}, secretbox.KeySize))
	a, err := KeysFrom(one)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := KeysFrom(one)
	other, _ := KeysFrom(two)
	if a != again || a.Public == other.Public {
		t.Errorf("keys: %+v %+v %+v", a, again, other)
	}
	pub, err := base64.RawURLEncoding.DecodeString(a.Public)
	if err != nil || len(pub) != 65 || pub[0] != 4 {
		t.Errorf("public key %q is not an uncompressed point", a.Public)
	}
}

// Only a push service's https address is sent to: never a machine of the
// home network, another port, a password in the address.
func TestOnlyPushServicesAreSentTo(t *testing.T) {
	for endpoint, want := range map[string]bool{
		"https://fcm.googleapis.com/fcm/send/abc":                true,
		"https://jmt17.google.com/fcm/send/abc":                  true,
		"https://web.push.apple.com/QAbc":                        true,
		"https://updates.push.services.mozilla.com/wpush/v2/abc": true,
		"https://wns2-db5p.notify.windows.com/w/?token=abc":      true,
		"http://fcm.googleapis.com/fcm/send/abc":                 false,
		"https://192.168.0.1/hook":                               false,
		"https://fcm.googleapis.com:8443/x":                      false,
		"https://user:pw@fcm.googleapis.com/x":                   false,
		"https://fcm.googleapis.com.evil.example/x":              false,
		"https://evilfcm.googleapis.com.example/x":               false,
		"not a url": false,
	} {
		if got := AllowedEndpoint(endpoint); got != want {
			t.Errorf("%s: %v, want %v", endpoint, got, want)
		}
	}
}

func cardWith(owner *uuid.UUID, st creditcard.Status) creditcard.Card {
	return creditcard.Card{
		Account: account.WithBalance{Account: account.Account{ID: uuid.New(), Name: "Кредитка Альфа", Currency: "RUB", OwnerUserID: owner}},
		Status:  st, ByJournal: true,
	}
}

// Three days ahead and on the day; the minimum is left out when the grace's
// sum that day covers it; a missed minimum and a lost grace at once.
func TestTheCardsCallForTheirReminders(t *testing.T) {
	today := d("2026-10-18")
	soon := cardWith(nil, creditcard.Status{
		Grace: []creditcard.Due{{On: d("2026-10-21"), Amount: 42_979_00}}, Minimum: 1_289_37, MinimumOn: d("2026-10-21"),
	})
	me := uuid.New()
	late := cardWith(&me, creditcard.Status{
		Minimum: 300_00, MinimumOn: d("2026-10-10"), MinimumMissed: true,
		Lost: []creditcard.Lost{{From: d("2026-08-01"), To: d("2026-08-31"), Amount: 25_679_00}},
	})
	far := cardWith(nil, creditcard.Status{Grace: []creditcard.Due{{On: d("2026-10-30"), Amount: 1_00}}, Minimum: 300_00, MinimumOn: d("2026-10-30")})
	got := CardReminders([]creditcard.Card{soon, late, far}, today)
	if len(got) != 3 {
		t.Fatalf("reminders = %+v", got)
	}
	if !strings.HasSuffix(got[0].Key, ":grace:2026-10-21:soon") || got[0].Body != "42 979,00 ₽ до 21.10, чтобы не платить проценты." || got[0].Owner != nil {
		t.Errorf("grace = %+v", got[0])
	}
	if !strings.HasSuffix(got[1].Key, ":min-missed:2026-10-10") || got[1].Owner == nil || *got[1].Owner != me {
		t.Errorf("missed = %+v", got[1])
	}
	if !strings.HasSuffix(got[2].Key, ":lost:2026-08-01") || !strings.Contains(got[2].Body, "01.08–31.08") {
		t.Errorf("lost = %+v", got[2])
	}
	if got := CardReminders([]creditcard.Card{soon}, d("2026-10-21")); len(got) != 1 || !strings.HasSuffix(got[0].Key, ":today") {
		t.Errorf("on the day = %+v", got)
	}

	// The grace taken off the whole debt: the window that missed, and one
	// message for the rest — not one per window.
	off := cardWith(nil, creditcard.Status{
		Lost: []creditcard.Lost{
			{From: d("2026-07-01"), To: d("2026-08-31"), Amount: 6_385_00},
			{From: d("2026-09-01"), To: d("2026-10-31"), Amount: 15_000_00, Early: true},
		},
		GraceOffSince: d("2027-01-01"), ToRestore: 21_385_00,
	})
	got = CardReminders([]creditcard.Card{off}, d("2027-01-02"))
	if len(got) != 2 || !strings.HasSuffix(got[0].Key, ":lost:2026-07-01") || !strings.HasSuffix(got[1].Key, ":grace-off:2027-01-01") ||
		!strings.Contains(got[1].Body, amount(21_385_00, "RUB")) {
		t.Errorf("grace off = %+v", got)
	}
}

func TestAmountsAreWrittenTheRussianWay(t *testing.T) {
	for minor, want := range map[int64]string{
		0: "0,00 ₽", 5: "0,05 ₽", 99_50: "99,50 ₽", 1_234_567_89: "1 234 567,89 ₽", -300_00: "-300,00 ₽",
	} {
		if got := amount(minor, "RUB"); got != want {
			t.Errorf("%d = %q, want %q", minor, got, want)
		}
	}
	if got := amount(10_00, "GBP"); got != "10,00 GBP" {
		t.Errorf("GBP = %q", got)
	}
}
