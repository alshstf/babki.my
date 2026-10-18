package notify

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Reminders of what happened to the family's data (decision Р-61 Б): a
// broker connection that stopped, an account that no longer matches its
// broker, a source of prices or rates that stopped loading, payouts that
// came, dividends declared on the family's papers.

// SyncState is a broker connection as its sync last left it: Revoked since
// the day the broker refused its token; LastOK the last run that went
// through, Failing when the last run failed.
type SyncState struct {
	ID      uuid.UUID
	Broker  string
	Last4   string
	Revoked bool
	Since   time.Time
	LastOK  *time.Time
	Failing bool
}

// failingDays is how long the runs may fail before a push: a broker's
// trouble of an hour or a night passes by itself.
const failingDays = 2

// SyncReminders: a token the broker refused, once; runs failing for two
// days since the last that went through, once for that day.
func SyncReminders(list []SyncState, today time.Time) []Reminder {
	var out []Reminder
	for _, s := range list {
		key := "sync:" + s.ID.String()
		switch {
		case s.Revoked:
			out = append(out, Reminder{
				Kind: KindSync, Key: key + ":revoked:" + day(s.Since), Title: s.Broker, URL: "/settings",
				Body: fmt.Sprintf("Брокер отклонил токен …%s — синхронизация остановлена. Вставьте новый токен в «Настройки → Подключения».", s.Last4),
			})
		case s.Failing && s.LastOK != nil && daysBetween(*s.LastOK, today) >= failingDays:
			out = append(out, Reminder{
				Kind: KindSync, Key: key + ":failing:" + day(*s.LastOK), Title: s.Broker, URL: "/settings",
				Body: fmt.Sprintf("Синхронизация не проходит с %s — счета не обновляются. Что случилось — в «Настройки → Подключения».", short(*s.LastOK)),
			})
		}
	}
	return out
}

// Mismatch is an account whose last reconciliation with its broker found it
// apart: RunID is that run's.
type Mismatch struct {
	AccountID uuid.UUID
	Name      string
	Owner     *uuid.UUID
	RunID     uuid.UUID
	On        time.Time
}

// ReconcileReminders: each reconciliation that found an account apart, once.
func ReconcileReminders(list []Mismatch) []Reminder {
	out := make([]Reminder, 0, len(list))
	for _, m := range list {
		out = append(out, Reminder{
			Kind: KindReconcile, Key: "reconcile:" + m.RunID.String(), Owner: m.Owner, Title: m.Name, URL: "/accounts/" + m.AccountID.String(),
			Body: fmt.Sprintf("Сверка с брокером %s: позиции счёта разошлись с брокером. На странице счёта видно, в чём разница.", short(m.On)),
		})
	}
	return out
}

// StaleSource is a source of prices or rates that has not loaded for three
// of its intervals: Name as people call it, LastOK its last success.
type StaleSource struct {
	Kind   string
	Name   string
	LastOK *time.Time
}

// StaleReminders: each source that stopped, once for its last success.
func StaleReminders(list []StaleSource) []Reminder {
	out := make([]Reminder, 0, len(list))
	for _, s := range list {
		r := Reminder{Kind: KindStale, Title: "Данные не обновляются", URL: "/settings"}
		if s.LastOK == nil {
			r.Key = "stale:" + s.Kind + ":never"
			r.Body = s.Name + ": ещё ни разу не загрузились — суммы на экранах без них неточны."
		} else {
			r.Key = "stale:" + s.Kind + ":" + day(*s.LastOK)
			r.Body = fmt.Sprintf("%s: последний раз %s — суммы на экранах по старым данным.", s.Name, short(*s.LastOK))
		}
		out = append(out, r)
	}
	return out
}

// Arrived is a payout that reached an account: Paper and What (купон,
// дивиденды…) as people call them, Got in Currency on On.
type Arrived struct {
	AccountID uuid.UUID
	Owner     *uuid.UUID
	Paper     string
	What      string
	Got       int64
	Currency  string
	On        time.Time
}

// arrivedDays is how far back a payout that came is told of: a broker books
// some a day or two late.
const arrivedDays = 3

// PayoutReminders: the payouts of each day of the last three, one push a day
// for each member they are for — an account's owner, or everyone for a
// shared one.
func PayoutReminders(list []Arrived, today time.Time) []Reminder {
	type group struct {
		on    time.Time
		owner *uuid.UUID
		lines []string
	}
	groups := map[string]*group{}
	for _, a := range list {
		if d := daysBetween(a.On, today); d < 0 || d > arrivedDays || a.Got <= 0 {
			continue
		}
		who := "all"
		if a.Owner != nil {
			who = a.Owner.String()
		}
		k := day(a.On) + ":" + who
		g, ok := groups[k]
		if !ok {
			g = &group{on: a.On, owner: a.Owner}
			groups[k] = g
		}
		g.lines = append(g.lines, fmt.Sprintf("%s — %s %s", a.Paper, a.What, amount(a.Got, a.Currency)))
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Reminder, 0, len(keys))
	for _, k := range keys {
		g := groups[k]
		out = append(out, Reminder{
			Kind: KindPayout, Key: "payout:" + k, Owner: g.owner, Title: "Пришли выплаты " + short(g.on), URL: "/payouts",
			Body: strings.Join(g.lines, "; ") + ".",
		})
	}
	return out
}

// Declared is a dividend declared on a paper the family holds: PerShare in
// Currency; Net what reaches the family's accounts, in NetCurrency, when
// known; Owner the one member it is for, nil for everyone.
type Declared struct {
	InstrumentID uuid.UUID
	Paper        string
	PerShare     decimal.Decimal
	Currency     string
	RecordOn     time.Time
	PayOn        *time.Time
	Net          *int64
	NetCurrency  string
	Owner        *uuid.UUID
}

// DeclaredReminders: each dividend whose record day is ahead, once.
func DeclaredReminders(list []Declared, today time.Time) []Reminder {
	var out []Reminder
	for _, d := range list {
		if d.RecordOn.Before(today) {
			continue
		}
		symbol, ok := symbols[d.Currency]
		if !ok {
			symbol = d.Currency
		}
		body := fmt.Sprintf("%s\u00a0%s на акцию, реестр %s", strings.Replace(d.PerShare.String(), ".", ",", 1), symbol, d.RecordOn.Format("02.01.2006"))
		if d.PayOn != nil {
			body += ", выплата до " + d.PayOn.Format("02.01.2006")
		}
		if d.Net != nil {
			body += fmt.Sprintf(". Придёт ≈ %s", amount(*d.Net, d.NetCurrency))
		}
		out = append(out, Reminder{
			Kind: KindDeclared, Key: "declared:" + d.InstrumentID.String() + ":" + day(d.RecordOn), Owner: d.Owner,
			Title: "Дивиденды: " + d.Paper, URL: "/payouts", Body: body + ".",
		})
	}
	return out
}
