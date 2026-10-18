package notify

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
)

// Telegram sends the reminders to the members' chats with their space's bot
// (decision Р-61 Б): Box opens the bot's token.
type Telegram struct {
	API TelegramAPI
	Box sealer
}

// DeliverTelegram sends each reminder to every member with a chat with the
// space's bot who takes its kind there and has not had it there yet, outside
// their quiet hours; a chat the person left is forgotten.
func DeliverTelegram(ctx context.Context, store *Store, tg *Telegram, log *slog.Logger, spaceID uuid.UUID, reminders []Reminder, now time.Time) error {
	if tg == nil || len(reminders) == 0 {
		return nil
	}
	bot, err := store.Bot(ctx, spaceID)
	if err != nil || bot == nil {
		return err
	}
	token, err := tg.Box.Open(bot.TokenCiphertext)
	if err != nil {
		log.Error("telegram bot token does not open", "space", spaceID, "error", err)
		return nil
	}
	chats, err := store.Chats(ctx, spaceID)
	if err != nil {
		return err
	}
	for _, c := range chats {
		prefs, err := store.Prefs(ctx, c.UserID)
		if err != nil {
			return err
		}
		if prefs.Quiet(now.Hour()) {
			continue
		}
	reminders:
		for _, r := range reminders {
			if (r.Owner != nil && *r.Owner != c.UserID) || !prefs.WantsTelegram(r.Kind) {
				continue
			}
			key := "tg:" + r.Key
			sent, err := store.Sent(ctx, c.UserID, key)
			if err != nil {
				return err
			}
			if sent {
				continue
			}
			gone, err := tg.API.SendMessage(ctx, string(token), c.ChatID, TelegramText(r, prefs.Amounts))
			switch {
			case gone:
				log.Info("telegram chat gone, forgotten", "answer", err)
				if _, err := store.Unlink(ctx, c.UserID); err != nil {
					return err
				}
				break reminders
			case errors.Is(err, ErrBadToken):
				log.Error("telegram does not know the space's bot token", "space", spaceID)
				return nil
			case err != nil:
				log.Warn("telegram message not delivered", "error", err)
			default:
				if err := store.MarkSent(ctx, c.UserID, key); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// TelegramUpdatesArgs is the job that reads what was sent to the spaces'
// bots: a member's «/start <code>» ties their chat.
type TelegramUpdatesArgs struct{}

func (TelegramUpdatesArgs) Kind() string { return "notify.telegram_updates" }

type telegramUpdatesWorker struct {
	river.WorkerDefaults[TelegramUpdatesArgs]
	store *Store
	tg    *Telegram
	log   *slog.Logger
	now   func() time.Time
}

// NewTelegramUpdatesWorker ties the chats; without a Telegram (no encryption
// key to open the bots' tokens) it does nothing.
func NewTelegramUpdatesWorker(store *Store, tg *Telegram, log *slog.Logger) river.Worker[TelegramUpdatesArgs] {
	return &telegramUpdatesWorker{store: store, tg: tg, log: log, now: time.Now}
}

// Timeout leaves the minute's next run its own time.
func (w *telegramUpdatesWorker) Timeout(*river.Job[TelegramUpdatesArgs]) time.Duration {
	return 45 * time.Second
}

func (w *telegramUpdatesWorker) Work(ctx context.Context, _ *river.Job[TelegramUpdatesArgs]) error {
	if w.tg == nil {
		return nil
	}
	bots, err := w.store.Bots(ctx)
	if err != nil {
		return err
	}
	for _, b := range bots {
		if err := ReadUpdates(ctx, w.store, w.tg, w.log, b, w.now()); err != nil {
			w.log.Warn("telegram updates", "space", b.SpaceID, "error", err)
		}
	}
	return nil
}

// Answers to what is sent to a bot.
const (
	linkedText  = "Готово: уведомления babki.my будут приходить в этот чат. О чём присылать — в «Настройки → Уведомления»."
	badCodeText = "Код не подошёл или устарел — возьмите новый в «Настройки → Уведомления» и откройте ссылку оттуда."
	helloText   = "Это бот уведомлений babki.my. Чтобы получать их сюда, нажмите «Привязать Telegram» в «Настройки → Уведомления»."
)

// ReadUpdates reads what was sent to the bot since the last time: a
// «/start <code>» ties the chat to the member whose code it is; anything
// else is answered with how to tie one.
func ReadUpdates(ctx context.Context, store *Store, tg *Telegram, log *slog.Logger, b Bot, now time.Time) error {
	token, err := tg.Box.Open(b.TokenCiphertext)
	if err != nil {
		return err
	}
	updates, err := tg.API.GetUpdates(ctx, string(token), b.Offset)
	if err != nil || len(updates) == 0 {
		return err
	}
	next := b.Offset
	for _, u := range updates {
		next = max(next, u.ID+1)
		if u.ChatID == 0 {
			continue
		}
		answer := helloText
		if code, ok := strings.CutPrefix(strings.TrimSpace(u.Text), "/start "); ok {
			linked, err := store.Link(ctx, b.SpaceID, code, u.ChatID, now)
			if err != nil {
				return err
			}
			answer = badCodeText
			if linked {
				answer = linkedText
			}
		}
		if _, err := tg.API.SendMessage(ctx, string(token), u.ChatID, answer); err != nil {
			log.Warn("telegram answer not delivered", "error", err)
		}
	}
	return store.SetOffset(ctx, b.SpaceID, next)
}
