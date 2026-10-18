package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// TelegramAPI is what the reminders need of Telegram's Bot API (decision
// Р-61 Б): the bot's name, the messages sent to it, a message to a chat.
type TelegramAPI interface {
	GetMe(ctx context.Context, token string) (username string, err error)
	GetUpdates(ctx context.Context, token string, offset int64) ([]Update, error)
	// SendMessage says gone when the chat is no more the bot's: the person
	// blocked it or deleted the chat.
	SendMessage(ctx context.Context, token string, chatID int64, text string) (gone bool, err error)
}

// Update is one message sent to the bot.
type Update struct {
	ID     int64
	ChatID int64
	Text   string
}

// ErrBadToken: Telegram does not know the bot's token.
var ErrBadToken = errors.New("notify: telegram does not know this bot token")

// BotAPI calls api.telegram.org. The token is part of every address, so no
// error it returns carries an address.
type BotAPI struct {
	Client *http.Client
	Base   string
}

func NewBotAPI() *BotAPI {
	return &BotAPI{Client: &http.Client{Timeout: 20 * time.Second}, Base: "https://api.telegram.org"}
}

type botAnswer struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
}

// call posts body to method and reads the answer; an error never names the
// address, which holds the token.
func (b *BotAPI) call(ctx context.Context, token, method string, body any) (botAnswer, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return botAnswer{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.Base+"/bot"+token+"/"+method, bytes.NewReader(raw))
	if err != nil {
		return botAnswer{}, fmt.Errorf("notify: telegram %s: a request could not be made", method)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.Client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return botAnswer{}, fmt.Errorf("notify: telegram %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return botAnswer{}, fmt.Errorf("notify: telegram %s: %w", method, err)
	}
	var a botAnswer
	if err := json.Unmarshal(data, &a); err != nil {
		return botAnswer{}, fmt.Errorf("notify: telegram %s: answer %d is not JSON", method, resp.StatusCode)
	}
	if a.ErrorCode == http.StatusUnauthorized || a.ErrorCode == http.StatusNotFound {
		return a, ErrBadToken
	}
	return a, nil
}

func (b *BotAPI) GetMe(ctx context.Context, token string) (string, error) {
	a, err := b.call(ctx, token, "getMe", map[string]any{})
	if err != nil {
		return "", err
	}
	if !a.OK {
		return "", fmt.Errorf("notify: telegram getMe: %s", a.Description)
	}
	var me struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(a.Result, &me); err != nil || me.Username == "" {
		return "", errors.New("notify: telegram getMe: no bot name in the answer")
	}
	return me.Username, nil
}

func (b *BotAPI) GetUpdates(ctx context.Context, token string, offset int64) ([]Update, error) {
	a, err := b.call(ctx, token, "getUpdates", map[string]any{"offset": offset, "timeout": 0, "allowed_updates": []string{"message"}})
	if err != nil {
		return nil, err
	}
	if !a.OK {
		return nil, fmt.Errorf("notify: telegram getUpdates: %s", a.Description)
	}
	var raw []struct {
		UpdateID int64 `json:"update_id"`
		Message  *struct {
			Text string `json:"text"`
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	}
	if err := json.Unmarshal(a.Result, &raw); err != nil {
		return nil, errors.New("notify: telegram getUpdates: the updates do not read")
	}
	out := make([]Update, 0, len(raw))
	for _, u := range raw {
		up := Update{ID: u.UpdateID}
		if u.Message != nil {
			up.ChatID, up.Text = u.Message.Chat.ID, u.Message.Text
		}
		out = append(out, up)
	}
	return out, nil
}

func (b *BotAPI) SendMessage(ctx context.Context, token string, chatID int64, text string) (bool, error) {
	a, err := b.call(ctx, token, "sendMessage", map[string]any{"chat_id": chatID, "text": text, "disable_web_page_preview": true})
	if err != nil {
		return false, err
	}
	if !a.OK {
		// 403: the person blocked the bot; 400 «chat not found»: the chat is gone.
		gone := a.ErrorCode == http.StatusForbidden || (a.ErrorCode == http.StatusBadRequest && strings.Contains(a.Description, "chat not found"))
		return gone, fmt.Errorf("notify: telegram sendMessage: %s", a.Description)
	}
	return false, nil
}

// sums are the money figures a reminder writes — «42 979,50 ₽», «34,84 ₽» —
// for a member who takes Telegram's messages without them.
var sums = regexp.MustCompile(`-?\d[\d\x{00a0}]*,\d\d\x{00a0}[^\s.,;:)]+`)

// TelegramText is a reminder as a message: its title, then its text — the
// sums put out as «…» unless the member takes them.
func TelegramText(r Reminder, amounts bool) string {
	text := r.Title + "\n" + r.Body
	if !amounts {
		text = sums.ReplaceAllString(text, "…")
	}
	return text
}
