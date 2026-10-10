package notify

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"babki.my/babki/internal/budget"
	"babki.my/babki/internal/category"
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

type budgets interface {
	Month(ctx context.Context, spaceID uuid.UUID, m time.Time) (budget.Month, error)
}

type categories interface {
	List(ctx context.Context, spaceID uuid.UUID) ([]category.Category, error)
}

type remindersWorker struct {
	river.WorkerDefaults[SendRemindersArgs]
	store      *Store
	cards      cards
	budgets    budgets
	categories categories
	sender     Sender
	log        *slog.Logger
	now        func() time.Time
}

// NewRemindersWorker pushes the reminders; with no sender (no encryption key
// to derive the push keys from) it does nothing.
func NewRemindersWorker(store *Store, c cards, b budgets, cats categories, sender Sender, log *slog.Logger) river.Worker[SendRemindersArgs] {
	return &remindersWorker{store: store, cards: c, budgets: b, categories: cats, sender: sender, log: log, now: time.Now}
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
		reminders := CardReminders(list, today)
		more, err := w.budgetReminders(ctx, spaceID, today)
		if err != nil {
			return err
		}
		if err := Deliver(ctx, w.store, w.sender, w.log, spaceID, append(reminders, more...)); err != nil {
			return err
		}
	}
	return nil
}

// budgetReminders are the month's budget pushes for the space.
func (w *remindersWorker) budgetReminders(ctx context.Context, spaceID uuid.UUID, today time.Time) ([]Reminder, error) {
	b, err := w.budgets.Month(ctx, spaceID, today)
	if err != nil || len(b.Lines) == 0 {
		return nil, err
	}
	cats, err := w.categories.List(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	names := make(map[uuid.UUID]string, len(cats))
	for _, c := range cats {
		names[c.ID] = c.Name
	}
	return BudgetReminders(b, names), nil
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
