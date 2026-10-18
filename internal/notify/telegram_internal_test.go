package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "123456:secret-token-of-the-bot-abcdef"

// The Bot API's answers read: the bot's name; an unknown token; a chat the
// person left. No error ever holds the token — it is in every address.
func TestTheBotAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/bot"+testToken+"/getMe"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"username":"babki_family_bot"}}`))
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
		}
	}))
	api := &BotAPI{Client: srv.Client(), Base: srv.URL}
	ctx := context.Background()
	if name, err := api.GetMe(ctx, testToken); err != nil || name != "babki_family_bot" {
		t.Errorf("getMe = %q, %v", name, err)
	}
	if _, err := api.GetMe(ctx, "999:wrong"); !errors.Is(err, ErrBadToken) {
		t.Errorf("an unknown token = %v", err)
	}
	if gone, err := api.SendMessage(ctx, testToken, 42, "hi"); !gone || err == nil || strings.Contains(err.Error(), testToken) {
		t.Errorf("a blocked bot = %v, %v", gone, err)
	}
	srv.Close()
	if _, err := api.GetMe(ctx, testToken); err == nil || strings.Contains(err.Error(), testToken) {
		t.Errorf("an unreachable Telegram = %v: the error must not name the token", err)
	}
}

// A member who takes Telegram's messages without the sums gets «…» in their
// place, the text around them kept.
func TestTelegramTextWithoutSums(t *testing.T) {
	r := Reminder{Title: "Пришли выплаты 18.10", Body: "Сбербанк — дивиденды " + amount(1_234_56, "RUB") + "; ОФЗ — купон " + amount(345_67, "RUB") + "."}
	if got := TelegramText(r, true); !strings.Contains(got, "1 234,56 ₽") {
		t.Errorf("with sums = %q", got)
	}
	if got := TelegramText(r, false); got != "Пришли выплаты 18.10\nСбербанк — дивиденды …; ОФЗ — купон …." {
		t.Errorf("without sums = %q", got)
	}
}
