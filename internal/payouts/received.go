package payouts

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/money"
	"babki.my/babki/internal/portfolio"
)

// Status says whether a payout due reached the journal (#424).
type Status string

const (
	// Received: a row of its kind for the paper came about its day.
	Received Status = "received"
	// Short: it came, but less than shortShare of what was due — more than
	// tax would take.
	Short Status = "short"
	// Missing: nothing came, and its day is past by more than the kind's
	// window.
	Missing Status = "missing"
	// Awaited: nothing yet, within the window.
	Awaited Status = "awaited"
)

// Check is one payout that was due: the Event as it was — what the account
// held the day before its record day — and what the journal shows of it.
type Check struct {
	Event
	Status      Status
	Got         int64
	GotCurrency string
	GotOn       *time.Time
}

// MaxDays is the furthest back a check looks.
const MaxDays = 366

// shortShare: what came below this share of the gross due is short. Russian
// tax takes 13–15% and a foreign withholding up to 30%, so two thirds still
// counts as come.
var shortShare = decimal.RequireFromString("0.65")

// early is how many days before its day a row still counts for a payout;
// window how many after, by kind: a broker books a coupon in days, a
// foreign dividend can take weeks.
const early = 5

func window(k Kind) int {
	if k == KindDividend {
		return 45
	}
	return 10
}

var rowType = map[Kind]operation.Type{
	KindCoupon: operation.TypeCoupon, KindAmortization: operation.TypeAmortization,
	KindRedemption: operation.TypeRedemption, KindDividend: operation.TypeDividend,
}

// due is a payout of a paper, before it is put to any account.
type due struct {
	on, record time.Time
	kind       Kind
	perUnit    *decimal.Decimal
	currency   string
}

// Received checks the payouts due in the last days within scope against the
// journals, most recent first.
func (s *Service) Received(ctx context.Context, spaceID uuid.UUID, days int, scope Scope) ([]Check, error) {
	if days < 1 || days > MaxDays {
		return nil, fmt.Errorf("%w: a check looks 1 to %d days back", family.ErrValidation, MaxDays)
	}
	now := s.now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	from := today.AddDate(0, 0, -days)

	list, err := s.accounts.ListWithBalance(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	journals := map[uuid.UUID][]operation.Operation{}
	seen := map[uuid.UUID]bool{}
	for _, a := range list {
		if a.Status != account.StatusActive || (scope.AccountID != nil && a.ID != *scope.AccountID) {
			continue
		}
		ops, err := s.journal.ListForEngine(ctx, spaceID, a.ID)
		if err != nil {
			return nil, err
		}
		journals[a.ID] = ops
		for _, op := range ops {
			if op.InstrumentID != nil && (scope.InstrumentID == nil || *op.InstrumentID == *scope.InstrumentID) {
				seen[*op.InstrumentID] = true
			}
		}
	}
	ids := make([]uuid.UUID, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	bonds, err := s.schedules.BondEventsBetween(ctx, ids, from, today)
	if err != nil {
		return nil, err
	}
	dividends, err := s.schedules.DividendsOf(ctx, ids)
	if err != nil {
		return nil, err
	}
	dues := map[uuid.UUID][]due{}
	for _, id := range ids {
		for _, e := range bonds[id] {
			if e.Kind == marketdata.BondOffer {
				continue
			}
			record := e.On.AddDate(0, 0, -1)
			if e.RecordOn != nil {
				record = *e.RecordOn
			}
			dues[id] = append(dues[id], due{on: e.On, record: record, kind: Kind(e.Kind), perUnit: e.Value, currency: e.Currency})
		}
		for _, d := range declared(dividends[id], from, today) {
			on := d.RecordDate
			if d.PaymentDate != nil {
				on = *d.PaymentDate
			}
			per := d.PerShare
			dues[id] = append(dues[id], due{on: on, record: d.RecordDate, kind: KindDividend, perUnit: &per, currency: d.Currency})
		}
	}

	var out []Check
	for accountID, ops := range journals {
		held := heldBefore(ops)
		used := map[uuid.UUID]bool{}
		for id, list := range dues {
			for _, d := range list {
				qty := held(id, d.record)
				if !qty.IsPositive() {
					continue
				}
				c := Check{Event: Event{
					On: d.on, RecordOn: &d.record, Kind: d.kind, InstrumentID: id, AccountID: accountID,
					Quantity: qty, PerUnit: d.perUnit, Currency: d.currency,
				}}
				if d.perUnit != nil {
					amount, err := money.Minor(d.perUnit.Mul(qty).Shift(2))
					if err != nil {
						return nil, fmt.Errorf("payouts: %s of %s: %w", d.kind, id, err)
					}
					c.Amount = &amount
				}
				match(&c, ops, used, today)
				out = append(out, c)
			}
		}
	}
	slices.SortStableFunc(out, func(a, b Check) int {
		if c := b.On.Compare(a.On); c != 0 {
			return c
		}
		if c := strings.Compare(a.InstrumentID.String(), b.InstrumentID.String()); c != 0 {
			return c
		}
		return strings.Compare(a.AccountID.String(), b.AccountID.String())
	})
	return out, nil
}

// heldBefore answers how much of a paper the journal held at the end of the
// day before a record day — a purchase settles a day later, so one on the
// record day itself is too late — replaying the journal once per day asked.
func heldBefore(ops []operation.Operation) func(id uuid.UUID, record time.Time) decimal.Decimal {
	memo := map[time.Time]map[uuid.UUID]*portfolio.Position{}
	return func(id uuid.UUID, record time.Time) decimal.Decimal {
		positions, ok := memo[record]
		if !ok {
			var before []operation.Operation
			for _, op := range ops {
				if op.OccurredOn.Before(record) {
					before = append(before, op)
				}
			}
			computed, err := portfolio.Compute(before)
			if err != nil {
				computed = map[uuid.UUID]*portfolio.Position{}
			}
			positions = computed
			memo[record] = positions
		}
		if p, ok := positions[id]; ok {
			return p.Quantity
		}
		return decimal.Zero
	}
}

// match finds the journal's rows of the payout's kind for its paper about its
// day, each row counted for one payout only, and says what came.
func match(c *Check, ops []operation.Operation, used map[uuid.UUID]bool, today time.Time) {
	want := rowType[c.Kind]
	first, last := c.On.AddDate(0, 0, -early), c.On.AddDate(0, 0, window(c.Kind))
	for _, op := range ops {
		if used[op.ID] || op.Type != want || op.InstrumentID == nil || *op.InstrumentID != c.InstrumentID ||
			op.OccurredOn.Before(first) || op.OccurredOn.After(last) {
			continue
		}
		if c.GotOn == nil {
			on := op.OccurredOn
			c.GotOn, c.GotCurrency = &on, op.Currency
		}
		if op.Currency == c.GotCurrency {
			c.Got += op.AmountMinor
		}
		used[op.ID] = true
	}
	switch {
	case c.GotOn == nil && today.After(last):
		c.Status = Missing
	case c.GotOn == nil:
		c.Status = Awaited
	case c.Amount != nil && c.GotCurrency == c.Currency &&
		decimal.NewFromInt(c.Got).LessThan(decimal.NewFromInt(*c.Amount).Mul(shortShare)):
		c.Status = Short
	default:
		c.Status = Received
	}
}
