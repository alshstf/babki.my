package notify_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"testing"

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
		if err := notify.Deliver(t.Context(), store, sender, slog.Default(), owner.SpaceID, reminders); err != nil {
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
