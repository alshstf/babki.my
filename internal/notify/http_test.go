package notify_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/notify"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/secretbox"
	"babki.my/babki/internal/platform/testdb"
)

// fakeSender takes every push, says the devices in gone are no more, and
// notes who got what.
type fakeSender struct {
	mu    sync.Mutex
	gone  map[string]bool
	got   map[string][]string
	tries map[string]int
}

func (f *fakeSender) Send(_ context.Context, sub notify.Subscription, r notify.Reminder) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tries[sub.Endpoint]++
	if f.gone[sub.Endpoint] {
		return true, nil
	}
	f.got[sub.Endpoint] = append(f.got[sub.Endpoint], r.Key)
	return false, nil
}

// A member turns reminders on for a device of a push service, never for an
// address of the home network; each reminder goes once to each member it is
// for — a personal card's to its owner alone — and a device its service says
// is gone is forgotten.
func TestRemindersReachTheirMembersOnce(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	famSvc := family.NewService(famStore)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	store := notify.NewStore(pool)
	box, _ := secretbox.New(bytes.Repeat([]byte{3}, secretbox.KeySize))
	keys, err := notify.KeysFrom(box)
	if err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{gone: map[string]bool{}, got: map[string][]string{}, tries: map[string]int{}}
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(famSvc, famStore, auth, sm).Mount(srv)
	notify.NewHandler(store, &keys, sender, auth, sm, slog.Default()).Mount(srv)
	base, c := apitest.Serve(t, srv.Handler())

	var key apitypes.PushKey
	apitest.Decode(t, apitest.Do(t, c, "GET", base+"/api/v1/push/key", ""), &key)
	if key.PublicKey != keys.Public {
		t.Errorf("key = %q", key.PublicKey)
	}
	if r := apitest.Do(t, c, "PUT", base+"/api/v1/push/subscriptions", `{"endpoint":"https://192.168.0.1/hook","p256dh":"k","auth":"a"}`); r.StatusCode != http.StatusBadRequest {
		t.Errorf("a home-network address = %d, want 400", r.StatusCode)
	}
	alexPhone := "https://fcm.googleapis.com/fcm/send/alex"
	if r := apitest.Do(t, c, "PUT", base+"/api/v1/push/subscriptions", `{"endpoint":"`+alexPhone+`","p256dh":"k","auth":"a"}`); r.StatusCode != http.StatusNoContent {
		t.Fatalf("subscribe = %d", r.StatusCode)
	}
	var test apitypes.PushTestResult
	apitest.Decode(t, apitest.Do(t, c, "POST", base+"/api/v1/push/test", ""), &test)
	if test.Devices != 1 || test.Delivered != 1 {
		t.Errorf("test = %+v", test)
	}

	var owner family.Principal
	if err := pool.QueryRow(t.Context(), `SELECT u.id, s.id FROM users u, spaces s WHERE u.username = 'alex'`).Scan(&owner.UserID, &owner.SpaceID); err != nil {
		t.Fatal(err)
	}
	owner.Role = family.RoleOwner
	bob, err := famSvc.CreateMember(t.Context(), owner, "bob", "Боб", "secret123", family.RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	bobPhone, bobOld := "https://web.push.apple.com/bob", "https://web.push.apple.com/bob-old"
	for _, e := range []string{bobPhone, bobOld} {
		if err := store.Subscribe(t.Context(), owner.SpaceID, notify.Subscription{Endpoint: e, UserID: bob.ID, P256dh: "k", Auth: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	sender.gone[bobOld] = true

	reminders := []notify.Reminder{{Key: "shared"}, {Key: "alex-only", Owner: &owner.UserID}}
	for range 2 {
		if err := notify.Deliver(t.Context(), store, sender, slog.Default(), owner.SpaceID, reminders, noon); err != nil {
			t.Fatal(err)
		}
	}
	if got := sender.got[alexPhone]; len(got) != 3 || got[1] != "shared" && got[2] != "shared" {
		t.Errorf("alex got %v, want the test, then shared and alex-only once each", got)
	}
	if got := sender.got[bobPhone]; len(got) != 1 || got[0] != "shared" {
		t.Errorf("bob got %v, want shared once", got)
	}
	if sender.tries[bobOld] != 1 {
		t.Errorf("the gone device was tried %d times, want once", sender.tries[bobOld])
	}
	devices, err := store.Devices(t.Context(), owner.SpaceID, &bob.ID)
	if err != nil || len(devices) != 1 || devices[0].Endpoint != bobPhone {
		t.Errorf("bob's devices = %+v (%v), want the old one forgotten", devices, err)
	}

	if r := apitest.Do(t, c, "DELETE", base+"/api/v1/push/subscriptions?endpoint="+url.QueryEscape(bobPhone), ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("turning off another member's device = %d, want 404", r.StatusCode)
	}
	if r := apitest.Do(t, c, "DELETE", base+"/api/v1/push/subscriptions?endpoint="+url.QueryEscape(alexPhone), ""); r.StatusCode != http.StatusNoContent {
		t.Errorf("turning off = %d", r.StatusCode)
	}
}

var noon = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// Decision Р-61 Б: a member's choice — every kind and quiet from 21 to 9
// until said otherwise; with budgets off and quiet from 23 to 7, a budget's
// reminder never reaches them, a card's waits through the quiet hours and
// comes after; another member's choice is theirs alone. A quiet hour out of
// 0–23 or an unknown kind is refused.
func TestTheMemberChoosesTheirReminders(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	store := notify.NewStore(pool)
	sender := &fakeSender{gone: map[string]bool{}, got: map[string][]string{}, tries: map[string]int{}}
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	notify.NewHandler(store, nil, sender, auth, sm, slog.Default()).Mount(srv)
	base, c := apitest.Serve(t, srv.Handler())

	var got apitypes.PushSettings
	apitest.Decode(t, apitest.Do(t, c, "GET", base+"/api/v1/push/settings", ""), &got)
	if got.QuietFrom != 21 || got.QuietTo != 9 || len(got.Kinds) != 9 || got.Kinds[0].Kind != "card" || !got.Kinds[0].On {
		t.Fatalf("default = %+v", got)
	}
	apitest.Decode(t, apitest.Do(t, c, "PUT", base+"/api/v1/push/settings", `{"off":["budget"],"quiet_from":23,"quiet_to":7}`), &got)
	if got.QuietFrom != 23 || got.QuietTo != 7 || got.Kinds[1].Kind != "budget" || got.Kinds[1].On || !got.Kinds[0].On {
		t.Fatalf("set = %+v", got)
	}
	for _, bad := range []string{`{"off":[],"quiet_from":24,"quiet_to":7}`, `{"off":["news"],"quiet_from":23,"quiet_to":7}`} {
		if r := apitest.Do(t, c, "PUT", base+"/api/v1/push/settings", bad); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, r.StatusCode)
		}
	}

	var alex family.Principal
	if err := pool.QueryRow(t.Context(), `SELECT u.id, s.id FROM users u, spaces s WHERE u.username = 'alex'`).Scan(&alex.UserID, &alex.SpaceID); err != nil {
		t.Fatal(err)
	}
	alex.Role = family.RoleOwner
	bob, err := family.NewService(famStore).CreateMember(t.Context(), alex, "bob", "Боб", "secret123", family.RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	alexPhone, bobPhone := "https://fcm.googleapis.com/fcm/send/alex", "https://web.push.apple.com/bob"
	for _, d := range []notify.Subscription{{Endpoint: alexPhone, UserID: alex.UserID, P256dh: "k", Auth: "a"}, {Endpoint: bobPhone, UserID: bob.ID, P256dh: "k", Auth: "a"}} {
		if err := store.Subscribe(t.Context(), alex.SpaceID, d); err != nil {
			t.Fatal(err)
		}
	}
	reminders := []notify.Reminder{{Kind: notify.KindBudget, Key: "budget"}, {Kind: notify.KindCard, Key: "card"}}
	for _, at := range []int{23, 6, 7} {
		if err := notify.Deliver(t.Context(), store, sender, slog.Default(), alex.SpaceID, reminders, time.Date(2026, 10, 19, at, 30, 0, 0, time.UTC)); err != nil {
			t.Fatal(err)
		}
		if at == 6 && len(sender.got[alexPhone]) != 0 {
			t.Errorf("at 6 alex got %v in his quiet hours", sender.got[alexPhone])
		}
	}
	if g := sender.got[alexPhone]; len(g) != 1 || g[0] != "card" {
		t.Errorf("alex got %v, want the card's alone, after 7", g)
	}
	// Bob's choice is the default: quiet from 21 till 9.
	if g := sender.got[bobPhone]; len(g) != 0 {
		t.Errorf("bob got %v before 9", g)
	}
	if err := notify.Deliver(t.Context(), store, sender, slog.Default(), alex.SpaceID, reminders, noon); err != nil {
		t.Fatal(err)
	}
	if g := sender.got[bobPhone]; len(g) != 2 {
		t.Errorf("bob got %v at noon, want both", g)
	}
}
