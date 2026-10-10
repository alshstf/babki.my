// Package recurring finds the family's regular payments in the journal
// (household stage 2): the rent, the phone, a subscription, a salary — money
// that goes or comes with the same counterparty at a steady pace and a
// steady amount — and says when each is due next. It reads the journal and the
// accounts and owns no table; nothing is stored, so a payment that stops
// simply drops out.
package recurring

import (
	"context"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/operation"
)

// Cadence is how often a payment comes.
type Cadence string

const (
	Weekly    Cadence = "weekly"
	Monthly   Cadence = "monthly"
	Quarterly Cadence = "quarterly"
	Yearly    Cadence = "yearly"
)

// cadences: the gap between payments, in days, each cadence admits, how many
// payments make it regular, and the step to the next one.
var cadences = []struct {
	cadence  Cadence
	min, max int
	needed   int
	next     func(time.Time) time.Time
}{
	{Weekly, 6, 8, 4, func(t time.Time) time.Time { return t.AddDate(0, 0, 7) }},
	{Monthly, 26, 35, 3, func(t time.Time) time.Time { return t.AddDate(0, 1, 0) }},
	{Quarterly, 85, 97, 3, func(t time.Time) time.Time { return t.AddDate(0, 3, 0) }},
	{Yearly, 350, 380, 2, func(t time.Time) time.Time { return t.AddDate(1, 0, 0) }},
}

// After is the day a payment of this cadence comes next after one on t.
func (c Cadence) After(t time.Time) time.Time {
	for _, x := range cadences {
		if x.cadence == c {
			return x.next(t)
		}
	}
	return t.AddDate(0, 1, 0)
}

// lookback is how far back the journal is read: a yearly payment needs two
// years to show twice.
const lookback = 2*365 + 30

// spread is how far an amount may stray from the usual and still be the same
// payment: utilities move with the season, a subscription does not move at
// all.
const spread = 0.25

// Payment is one regular payment. Amount is the usual one, in minor units of
// Currency, signed as the journal signs it: negative going out.
type Payment struct {
	Name       string
	Cadence    Cadence
	Amount     int64
	Currency   string
	AccountID  uuid.UUID
	CategoryID *uuid.UUID
	Last       time.Time
	Next       time.Time
	Count      int
	// Overdue is a payment whose day has passed without it, within the slack
	// its pace allows: late, or not entered yet.
	Overdue bool
	// Hidden is a payment the family said is not regular: listed apart, left
	// out of the forecast.
	Hidden bool
}

type journals interface {
	ListMoneyFlows(ctx context.Context, spaceID uuid.UUID, from, to time.Time) ([]operation.Operation, error)
}

type accounts interface {
	ListWithBalance(ctx context.Context, spaceID uuid.UUID) ([]account.WithBalance, error)
}

type hiddenPayments interface {
	All(ctx context.Context, spaceID uuid.UUID) (map[Key]bool, error)
}

// Service finds regular payments.
type Service struct {
	journal  journals
	accounts accounts
	hidden   hiddenPayments
	now      func() time.Time
}

func NewService(j journals, acc accounts, hidden hiddenPayments) *Service {
	return &Service{journal: j, accounts: acc, hidden: hidden, now: time.Now}
}

// Find is the space's regular payments, the soonest due first. Only everyday
// accounts are read: what moves on a broker's account is investing.
func (s *Service) Find(ctx context.Context, spaceID uuid.UUID) ([]Payment, error) {
	now := s.now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	list, err := s.accounts.ListWithBalance(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	everyday := map[uuid.UUID]bool{}
	for _, a := range list {
		if a.Type != account.TypeBrokerage && a.Status == account.StatusActive {
			everyday[a.ID] = true
		}
	}
	ops, err := s.journal.ListMoneyFlows(ctx, spaceID, today.AddDate(0, 0, -lookback), today)
	if err != nil {
		return nil, err
	}
	var kept []operation.Operation
	for _, op := range ops {
		if everyday[op.AccountID] && (op.Type == operation.TypeDeposit || op.Type == operation.TypeWithdrawal || op.Type == operation.TypeFee) {
			kept = append(kept, op)
		}
	}
	found := find(kept, today)
	hidden, err := s.hidden.All(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	for i := range found {
		found[i].Hidden = hidden[KeyOf(found[i].Name, found[i].Amount > 0, found[i].Currency)]
	}
	return found, nil
}

// key is what makes two rows the same payment: who it is with, which way the
// money goes, and in what currency.
type key struct {
	name     string
	incoming bool
	currency string
}

// payee is the row's counterparty, or its note without one, as payments are
// told apart: case aside, «ё» as «е», trailing words with digits — a shop's
// number, a card's tail, a date — dropped.
func payee(op operation.Operation) (folded, shown string) {
	text := strings.TrimSpace(op.Counterparty)
	if text == "" {
		text = strings.TrimSpace(op.Note)
	}
	words := strings.Fields(text)
	for len(words) > 1 && strings.ContainsFunc(words[len(words)-1], unicode.IsDigit) {
		words = words[:len(words)-1]
	}
	shown = strings.Join(words, " ")
	return strings.ReplaceAll(strings.ToLower(shown), "ё", "е"), shown
}

// find groups rows by payee and keeps the groups paid at a steady pace and a
// steady amount, still going today.
func find(ops []operation.Operation, today time.Time) []Payment {
	groups := map[key][]operation.Operation{}
	shown := map[key]string{}
	for _, op := range ops {
		folded, name := payee(op)
		if folded == "" || op.AmountMinor == 0 {
			continue
		}
		k := key{name: folded, incoming: op.AmountMinor > 0, currency: op.Currency}
		groups[k] = append(groups[k], op)
		shown[k] = name
	}
	var out []Payment
	for k, rows := range groups {
		slices.SortFunc(rows, func(a, b operation.Operation) int { return a.OccurredOn.Compare(b.OccurredOn) })
		if p, ok := regular(rows, today); ok {
			p.Name = shown[k]
			out = append(out, p)
			continue
		}
		// Twice a month from one payee — a salary and its advance — is two
		// monthly payments, told apart by the day of the month each comes on.
		for _, half := range byDayOfMonth(rows) {
			if p, ok := regular(half, today); ok && p.Cadence == Monthly {
				p.Name = shown[k]
				out = append(out, p)
			}
		}
	}
	slices.SortFunc(out, func(a, b Payment) int {
		if c := a.Next.Compare(b.Next); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// regular reads one payee's rows, oldest first, as a regular payment, or says
// they are not one.
func regular(rows []operation.Operation, today time.Time) (Payment, bool) {
	if len(rows) < 2 {
		return Payment{}, false
	}
	gaps := make([]int, 0, len(rows)-1)
	for i := 1; i < len(rows); i++ {
		gaps = append(gaps, int(rows[i].OccurredOn.Sub(rows[i-1].OccurredOn).Hours()/24))
	}
	median := sortedMedian(gaps)
	for _, c := range cadences {
		if median < c.min || median > c.max || len(rows) < c.needed {
			continue
		}
		// One gap off the pace is forgiven: a payment a few days late, or one
		// skipped month.
		off := 0
		for _, g := range gaps {
			if g < c.min || g > c.max {
				off++
			}
		}
		if off > 1 {
			return Payment{}, false
		}
		amounts := make([]int, 0, len(rows))
		for _, r := range rows {
			amounts = append(amounts, int(abs(r.AmountMinor)))
		}
		usual := sortedMedian(amounts)
		for _, r := range rows[len(rows)-min(len(rows), 3):] {
			if d := float64(abs(abs(r.AmountMinor) - int64(usual))); d > spread*float64(usual) {
				return Payment{}, false
			}
		}
		last := rows[len(rows)-1]
		next := c.next(last.OccurredOn)
		// A payment that missed its day by more than half its pace has stopped.
		if today.Sub(next) > time.Duration(c.max/2)*24*time.Hour {
			return Payment{}, false
		}
		amount := int64(usual)
		if last.AmountMinor < 0 {
			amount = -amount
		}
		return Payment{
			Cadence: c.cadence, Amount: amount, Currency: last.Currency, AccountID: last.AccountID,
			CategoryID: last.CategoryID, Last: last.OccurredOn, Next: next, Count: len(rows),
			Overdue: next.Before(today),
		}, true
	}
	return Payment{}, false
}

// halfSpread is how many days either side of its usual day a payment of a
// twice-monthly pair may come — a weekend moves it — and still be that one.
const halfSpread = 7

// byDayOfMonth splits a payee's rows in two by the day of the month, where the
// days fall in two bunches with a clear gap between them (the 5th and the
// 25th; the 4th–6th and the 24th–26th); nil otherwise. The month is taken
// round, so the 30th and the 1st sit together.
func byDayOfMonth(rows []operation.Operation) [][]operation.Operation {
	seen := map[int]bool{}
	for _, r := range rows {
		seen[r.OccurredOn.Day()] = true
	}
	days := make([]int, 0, len(seen))
	for d := range seen {
		days = append(days, d)
	}
	slices.Sort(days)
	if len(days) < 2 {
		return nil
	}
	n := len(days)
	gap := func(i int) int { return (days[(i+1)%n] - days[i] + 31) % 31 }
	// The two widest gaps cut the round month into the two bunches.
	first, second := -1, -1
	for i := range n {
		switch {
		case first < 0 || gap(i) > gap(first):
			first, second = i, first
		case second < 0 || gap(i) > gap(second):
			second = i
		}
	}
	// A bunch runs from the day after one cut to the day before the next.
	bunch := func(from, to int) map[int]bool {
		in := map[int]bool{}
		for i := (from + 1) % n; ; i = (i + 1) % n {
			in[days[i]] = true
			if i == to {
				break
			}
		}
		return in
	}
	spread := func(from, to int) int { return (days[to] - days[(from+1)%n] + 31) % 31 }
	if spread(first, second) > 2*halfSpread || spread(second, first) > 2*halfSpread || gap(second) <= halfSpread {
		return nil
	}
	a := bunch(first, second)
	var one, two []operation.Operation
	for _, r := range rows {
		if a[r.OccurredOn.Day()] {
			one = append(one, r)
		} else {
			two = append(two, r)
		}
	}
	// Each half is one payment a month: two in one month is a shop visited
	// twice, not a salary.
	if !oncePerMonth(one) || !oncePerMonth(two) {
		return nil
	}
	return [][]operation.Operation{one, two}
}

func oncePerMonth(rows []operation.Operation) bool {
	seen := map[[2]int]bool{}
	for _, r := range rows {
		m := [2]int{r.OccurredOn.Year(), int(r.OccurredOn.Month())}
		if seen[m] {
			return false
		}
		seen[m] = true
	}
	return true
}

func sortedMedian(xs []int) int {
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[len(s)/2]
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
