package notify_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
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

// fakeTelegram is a Telegram that knows one bot, hands over the updates
// queued for it and notes the messages sent; a chat in gone was left.
type fakeTelegram struct {
	mu      sync.Mutex
	updates []notify.Update
	sent    map[int64][]string
	gone    map[int64]bool
}

func (f *fakeTelegram) GetMe(_ context.Context, token string) (string, error) {
	if token != "123456:the-family-bot-token-0000" {
		return "", notify.ErrBadToken
	}
	return "babki_family_bot", nil
}

func (f *fakeTelegram) GetUpdates(context.Context, string, int64) ([]notify.Update, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.updates
	f.updates = nil
	return out, nil
}

func (f *fakeTelegram) SendMessage(_ context.Context, _ string, chatID int64, text string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone[chatID] {
		return true, errors.New("blocked")
	}
	f.sent[chatID] = append(f.sent[chatID], text)
	return false, nil
}

// Decision Р-61 Б: the owner gives the space a bot — a token Telegram does
// not know is refused, an editor may not; a member takes a code, sends it to
// the bot and their chat is tied, a stale code is not; the reminders reach
// the chat as the member chose — a kind off, without the sums — once; a chat
// the person left is forgotten.
func TestTelegramAsTheSecondChannel(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	famSvc := family.NewService(famStore)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	store := notify.NewStore(pool)
	box, _ := secretbox.New(bytes.Repeat([]byte{3}, secretbox.KeySize))
	api := &fakeTelegram{sent: map[int64][]string{}, gone: map[int64]bool{}}
	tg := &notify.Telegram{API: api, Box: box}
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(famSvc, famStore, auth, sm).Mount(srv)
	notify.NewHandler(store, nil, nil, auth, sm, slog.Default()).WithTelegram(tg).Mount(srv)
	base, c := apitest.Serve(t, srv.Handler())

	var st apitypes.TelegramStatus
	apitest.Decode(t, apitest.Do(t, c, "GET", base+"/api/v1/telegram", ""), &st)
	if !st.Available || !st.CanManage || !st.Bot.IsNull() || st.Linked {
		t.Fatalf("before = %+v", st)
	}
	if r := apitest.Do(t, c, "POST", base+"/api/v1/telegram/link", ""); r.StatusCode != http.StatusConflict {
		t.Errorf("a code without a bot = %d, want 409", r.StatusCode)
	}
	for _, bad := range []string{`{"token":"123456:not-the-token-at-all-0000"}`, `{"token":"12/../x:yyyyyyyyyyyyyyyyyyyy"}`} {
		if r := apitest.Do(t, c, "PUT", base+"/api/v1/telegram/bot", bad); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, r.StatusCode)
		}
	}
	apitest.Decode(t, apitest.Do(t, c, "PUT", base+"/api/v1/telegram/bot", `{"token":"123456:the-family-bot-token-0000"}`), &st)
	if b := st.Bot.MustGet(); b.Username != "babki_family_bot" || b.TokenLast4 != "0000" {
		t.Fatalf("bot = %+v", b)
	}
	var sealed []byte
	if err := pool.QueryRow(t.Context(), `SELECT token_ciphertext FROM telegram_bots`).Scan(&sealed); err != nil || bytes.Contains(sealed, []byte("the-family-bot")) {
		t.Errorf("the token is kept as it is: %v", err)
	}

	var link apitypes.TelegramLink
	apitest.Decode(t, apitest.Do(t, c, "POST", base+"/api/v1/telegram/link", ""), &link)
	if link.Url != "https://t.me/babki_family_bot?start="+link.Code || len(link.Code) < 16 {
		t.Fatalf("link = %+v", link)
	}
	api.updates = []notify.Update{{ID: 7, ChatID: 555, Text: "/start nonsense"}, {ID: 8, ChatID: 100, Text: "/start " + link.Code}, {ID: 9, ChatID: 200, Text: "привет"}}
	bots, err := store.Bots(t.Context())
	if err != nil || len(bots) != 1 {
		t.Fatalf("bots = %v, %v", bots, err)
	}
	if err := notify.ReadUpdates(t.Context(), store, tg, slog.Default(), bots[0], time.Now()); err != nil {
		t.Fatal(err)
	}
	apitest.Decode(t, apitest.Do(t, c, "GET", base+"/api/v1/telegram", ""), &st)
	if !st.Linked || !strings.HasPrefix(api.sent[100][0], "Готово") || !strings.HasPrefix(api.sent[555][0], "Код не подошёл") || !strings.HasPrefix(api.sent[200][0], "Это бот") {
		t.Errorf("linked %v, answers %v", st.Linked, api.sent)
	}
	if bots, _ := store.Bots(t.Context()); bots[0].Offset != 10 {
		t.Errorf("offset = %d, want past the last update", bots[0].Offset)
	}

	var alex family.Principal
	if err := pool.QueryRow(t.Context(), `SELECT u.id, s.id FROM users u, spaces s WHERE u.username = 'alex'`).Scan(&alex.UserID, &alex.SpaceID); err != nil {
		t.Fatal(err)
	}
	alex.Role = family.RoleOwner
	var got apitypes.PushSettings
	apitest.Decode(t, apitest.Do(t, c, "PUT", base+"/api/v1/push/settings", `{"off":[],"telegram_off":["budget"],"amounts":false,"quiet_from":0,"quiet_to":0}`), &got)
	if got.Amounts || got.Kinds[1].Telegram || !got.Kinds[1].On || !got.Kinds[0].Telegram {
		t.Fatalf("settings = %+v", got)
	}
	reminders := []notify.Reminder{
		{Kind: notify.KindBudget, Key: "budget", Title: "Бюджет", Body: "Лимит превышен."},
		{Kind: notify.KindPayout, Key: "payout", Title: "Пришли выплаты 18.10", Body: "Сбербанк — дивиденды 1 234,56 ₽."},
	}
	for range 2 {
		if err := notify.DeliverTelegram(t.Context(), store, tg, slog.Default(), alex.SpaceID, reminders, noon); err != nil {
			t.Fatal(err)
		}
	}
	if msgs := api.sent[100]; len(msgs) != 2 || msgs[1] != "Пришли выплаты 18.10\nСбербанк — дивиденды …." {
		t.Errorf("chat = %q, want the payouts alone, once, without the sum", msgs)
	}

	api.gone[100] = true
	if err := notify.DeliverTelegram(t.Context(), store, tg, slog.Default(), alex.SpaceID, []notify.Reminder{{Kind: notify.KindCard, Key: "card"}}, noon); err != nil {
		t.Fatal(err)
	}
	apitest.Decode(t, apitest.Do(t, c, "GET", base+"/api/v1/telegram", ""), &st)
	if st.Linked {
		t.Error("a chat the person left is still tied")
	}

	if _, err := famSvc.CreateMember(t.Context(), alex, "bob", "Боб", "secret123", family.RoleEditor); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	bc := &http.Client{Jar: jar}
	login, err := bc.Post(base+"/api/v1/auth/login", "application/json", strings.NewReader(`{"username":"bob","password":"secret123"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = login.Body.Close()
	if r := apitest.Do(t, bc, "PUT", base+"/api/v1/telegram/bot", `{"token":"123456:the-family-bot-token-0000"}`); r.StatusCode != http.StatusForbidden {
		t.Errorf("an editor gives a bot = %d, want 403", r.StatusCode)
	}
	apitest.Decode(t, apitest.Do(t, c, "DELETE", base+"/api/v1/telegram/bot", ""), &st)
	if !st.Bot.IsNull() {
		t.Errorf("after taking the bot away = %+v", st)
	}
}
