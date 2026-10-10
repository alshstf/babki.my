package notify

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"babki.my/babki/internal/creditcard"
)

// SendRemindersArgs is the hourly job that pushes what is due.
type SendRemindersArgs struct{}

func (SendRemindersArgs) Kind() string { return "notify.send_reminders" }

// Pushes go out in the day only, by the server's clock (TZ).
const (
	fromHour = 9
	toHour   = 21
)

type cards interface {
	All(ctx context.Context, spaceID uuid.UUID) ([]creditcard.Card, error)
}

type remindersWorker struct {
	river.WorkerDefaults[SendRemindersArgs]
	store  *Store
	cards  cards
	sender Sender
	log    *slog.Logger
	now    func() time.Time
}

// NewRemindersWorker pushes the reminders; with no sender (no encryption key
// to derive the push keys from) it does nothing.
func NewRemindersWorker(store *Store, c cards, sender Sender, log *slog.Logger) river.Worker[SendRemindersArgs] {
	return &remindersWorker{store: store, cards: c, sender: sender, log: log, now: time.Now}
}

func (w *remindersWorker) Timeout(*river.Job[SendRemindersArgs]) time.Duration {
	return 10 * time.Minute
}

func (w *remindersWorker) Work(ctx context.Context, _ *river.Job[SendRemindersArgs]) error {
	now := w.now()
	if w.sender == nil || now.Hour() < fromHour || now.Hour() >= toHour {
		return nil
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	spaces, err := w.store.Spaces(ctx)
	if err != nil {
		return err
	}
	for _, spaceID := range spaces {
		list, err := w.cards.All(ctx, spaceID)
		if err != nil {
			return err
		}
		if err := Deliver(ctx, w.store, w.sender, w.log, spaceID, CardReminders(list, today)); err != nil {
			return err
		}
	}
	return nil
}

// Deliver sends each reminder to every member it is for who has a device and
// has not had it yet, and notes it sent once a device took it. A device whose
// push service says it is gone is forgotten.
func Deliver(ctx context.Context, store *Store, sender Sender, log *slog.Logger, spaceID uuid.UUID, reminders []Reminder) error {
	if len(reminders) == 0 {
		return nil
	}
	devices, err := store.Devices(ctx, spaceID, nil)
	if err != nil {
		return err
	}
	byUser := map[uuid.UUID][]Subscription{}
	for _, d := range devices {
		byUser[d.UserID] = append(byUser[d.UserID], d)
	}
	forgotten := map[string]bool{}
	for _, r := range reminders {
		for user, subs := range byUser {
			if r.Owner != nil && *r.Owner != user {
				continue
			}
			sent, err := store.Sent(ctx, user, r.Key)
			if err != nil {
				return err
			}
			if sent {
				continue
			}
			delivered := false
			for _, sub := range subs {
				if forgotten[sub.Endpoint] {
					continue
				}
				gone, err := sender.Send(ctx, sub, r)
				switch {
				case gone:
					log.Info("push device gone, forgotten", "service", serviceHost(sub.Endpoint), "answer", err)
					forgotten[sub.Endpoint] = true
					if err := store.Forget(ctx, sub.Endpoint); err != nil {
						return err
					}
				case err != nil:
					log.Warn("push not delivered", "error", err)
				default:
					delivered = true
				}
			}
			if delivered {
				if err := store.MarkSent(ctx, user, r.Key); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
