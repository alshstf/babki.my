package account

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/money"
)

// JournalValue is what a brokerage account is worth by its journal, as the
// portfolio engine values it; cmd/babki wires the engine in. It is struck
// twice (decision Р-11): liquid (Minor and the fields beside it) and full.
type JournalValue struct {
	// Currency is the space's base currency, the one Minor is in.
	Currency string
	// Minor is the holdings the market prices now plus the cash, converted.
	Minor int64
	// ByCurrency is that worth by currency, before conversion.
	ByCurrency map[string]int64
	// Operations is the journal's size; zero means the account has no journal.
	Operations int
	// Unpriced counts the holdings Minor counts as nothing.
	Unpriced int
	// NotTraded is how many of Unpriced the full worth still values.
	NotTraded int
	// MissingRates names the currencies left out of Minor for want of a rate.
	MissingRates []string
	// NegativeCash names the currencies whose cash by the journal is below zero.
	NegativeCash []string
	// FullMinor and the fields after it are the full worth's.
	FullMinor        int64
	FullByCurrency   map[string]int64
	FullUnpriced     int
	FullMissingRates []string
}

type journalValuer interface {
	ValueFromJournal(ctx context.Context, spaceID, accountID uuid.UUID) (JournalValue, error)
	// ValueOn is the worth as of the end of day, at that day's prices and rates.
	ValueOn(ctx context.Context, spaceID, accountID uuid.UUID, day time.Time) (JournalValue, error)
	// ValuesOn is ValueOn for several days.
	ValuesOn(ctx context.Context, spaceID, accountID uuid.UUID, days []time.Time) ([]JournalValue, error)
	// ReturnBasis is what the period (from, to] is reckoned from.
	ReturnBasis(ctx context.Context, spaceID, accountID uuid.UUID, from, to time.Time) (ReturnBasis, error)
}

// ReturnBasis is an account's worth at a period's two ends and the money that
// crossed its edge, from the investor's side, in the base currency.
type ReturnBasis struct {
	Start, End JournalValue
	Flows      []ReturnFlow
	Complete   bool
}

// ReturnFlow is one such crossing.
type ReturnFlow struct {
	Day   time.Time
	Minor int64
}

// How far the journal may be from the balance and still agree: brokers price
// at the last trade and this program at the previous close, so a percent or
// two is normal; past five, operations are likelier missing. The thresholds
// are this program's choice (Р-2).
const (
	agreesWithinPercent = 1
	closeWithinPercent  = 5
	// A balance mark older than this is compared with the journal as of the mark's
	// own day, so market moves since are not blamed on the journal.
	staleAfterDays = 3
)

// valuation is how one account stands against the family total.
type valuation struct {
	// journal is nil for an account not kept by its operations.
	journal        *JournalValue
	byJournal      bool
	reconciliation *apitypes.AccountReconciliation
	// everyday is an account other than a broker's: kept by its balance
	// unless the family says otherwise, and never part of the family's return.
	everyday bool
}

// CountedByJournal is whether the total counts a's journal rather than its
// balance: a broker's account unless pinned to its balance, an everyday one
// only when the family keeps it by its operations. An account with no
// operations is counted by its balance whatever this says.
func CountedByJournal(a Account) bool {
	if a.Type == TypeBrokerage {
		return !a.ValuedByBalance
	}
	return a.KeptByOperations
}

// valuations values the active accounts that have a journal and reconciles
// each with its latest balance. Others are counted by balance.
func (h *Handler) valuations(ctx context.Context, spaceID uuid.UUID, accounts []WithBalance, baseCurrency string, now time.Time, rates *marketdata.RateMemo) (map[uuid.UUID]valuation, error) {
	out := make(map[uuid.UUID]valuation)
	if h.journals == nil {
		return out, nil
	}
	for _, a := range accounts {
		if a.Status != StatusActive {
			continue
		}
		v, err := h.journals.ValueFromJournal(ctx, spaceID, a.ID)
		if err != nil {
			return nil, fmt.Errorf("value account %s from its journal: %w", a.ID, err)
		}
		if v.Operations == 0 {
			continue
		}
		rec, err := h.reconcile(ctx, a, v, baseCurrency, now, rates)
		if err != nil {
			return nil, err
		}
		out[a.ID] = valuation{
			journal: &v, byJournal: CountedByJournal(a.Account), reconciliation: rec,
			everyday: a.Type != TypeBrokerage,
		}
	}
	return out, nil
}

// reconcile compares the journal with a's latest balance in the base currency:
// a recent mark against today's worth, an older one against the worth on its
// own day. No verdict when the journal cannot be valued whole that day; nil
// when there is no mark or no rate.
func (h *Handler) reconcile(ctx context.Context, a WithBalance, v JournalValue, baseCurrency string, now time.Time, rates *marketdata.RateMemo) (*apitypes.AccountReconciliation, error) {
	if a.Balance == nil {
		return nil, nil
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	asOf := a.Balance.AsOf
	comparedOn, journal, complete := today, v.Minor, true
	balanceOn := now
	if asOf.Before(today.AddDate(0, 0, -staleAfterDays)) {
		past, err := h.journals.ValueOn(ctx, a.SpaceID, a.ID, asOf)
		if err != nil {
			return nil, fmt.Errorf("value account %s on %s: %w", a.ID, asOf.Format(time.DateOnly), err)
		}
		comparedOn, journal, balanceOn = asOf, past.Minor, asOf
		complete = past.Operations > 0 && past.Unpriced == 0 && len(past.MissingRates) == 0
	}
	balance := a.Balance.AmountMinor
	if a.Currency != baseCurrency {
		inBase, err := h.balanceInBase(ctx, a, baseCurrency, balanceOn, rates)
		if err != nil || inBase == nil {
			return nil, err
		}
		balance = inBase.AmountMinor
	}
	diff, err := money.Sub(journal, balance)
	if err != nil {
		return nil, fmt.Errorf("%w: account %s, its journal against its balance", err, a.ID)
	}
	status := reconciliationStatus(diff, balance)
	if !complete {
		status = apitypes.Stale
	}
	return &apitypes.AccountReconciliation{
		Status:             status,
		BalanceAsOf:        asOf.Format("2006-01-02"),
		ComparedOn:         comparedOn.Format("2006-01-02"),
		BalanceInBaseMinor: balance,
		DifferenceMinor:    diff,
	}, nil
}

// reconciliationStatus grades a difference against its balance; a zero
// balance agrees only with a zero journal.
func reconciliationStatus(diff, balance int64) apitypes.AccountReconciliationStatus {
	gap := decimal.NewFromInt(diff).Abs().Mul(decimal.NewFromInt(100))
	of := decimal.NewFromInt(balance).Abs()
	switch {
	case gap.LessThanOrEqual(of.Mul(decimal.NewFromInt(agreesWithinPercent))):
		return apitypes.Agrees
	case gap.LessThanOrEqual(of.Mul(decimal.NewFromInt(closeWithinPercent))):
		return apitypes.Close
	default:
		return apitypes.Differs
	}
}

// describe writes v onto the account's row.
func (v valuation) describe(row *apitypes.AccountWithBalance) {
	row.CountedBy = apitypes.AccountWithBalanceCountedByBalance
	if v.byJournal {
		row.CountedBy = apitypes.AccountWithBalanceCountedByJournal
	}
	if v.journal == nil {
		row.Journal = nullable.NewNullNullable[apitypes.AccountJournal]()
		return
	}
	j := apitypes.AccountJournal{
		AmountMinor:        v.journal.Minor,
		FullAmountMinor:    v.journal.FullMinor,
		Currency:           v.journal.Currency,
		UnpricedPositions:  v.journal.Unpriced,
		NotTradedPositions: v.journal.NotTraded,
		MissingRates:       nonNil(v.journal.MissingRates),
		NegativeCash:       nonNil(v.journal.NegativeCash),
		Reconciliation:     nullable.NewNullNullable[apitypes.AccountReconciliation](),
	}
	if v.reconciliation != nil {
		j.Reconciliation = nullable.NewNullableWithValue(*v.reconciliation)
	}
	row.Journal = nullable.NewNullableWithValue(j)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// addJournals adds journal-valued accounts to the per-currency totals: holdings
// count as assets, and cash below zero beyond the holdings as debt.
func addJournals(totals []CurrencyTotal, vals map[uuid.UUID]valuation) ([]CurrencyTotal, error) {
	byCurrency := make(map[string]CurrencyTotal, len(totals))
	for _, t := range totals {
		byCurrency[t.Currency] = t
	}
	for id, v := range vals {
		if !v.byJournal {
			continue
		}
		for currency, minor := range v.journal.ByCurrency {
			t := byCurrency[currency]
			t.Currency = currency
			side := &t.AssetsMinor
			if minor < 0 {
				side = &t.LiabilitiesMinor
			}
			var err error
			if *side, err = money.Add(*side, minor); err != nil {
				return nil, fmt.Errorf("%w: the family total in %s, adding account %s", err, currency, id)
			}
			if t.NetMinor, err = money.Add(t.AssetsMinor, t.LiabilitiesMinor); err != nil {
				return nil, fmt.Errorf("%w: the family total in %s, adding account %s", err, currency, id)
			}
			byCurrency[currency] = t
		}
	}
	out := make([]CurrencyTotal, 0, len(byCurrency))
	for _, currency := range slices.Sorted(maps.Keys(byCurrency)) {
		out = append(out, byCurrency[currency])
	}
	return out, nil
}

// journalSummary says what the total owes to journals rather than balances.
func journalSummary(vals map[uuid.UUID]valuation) (apitypes.SummaryJournal, error) {
	var (
		out apitypes.SummaryJournal
		err error
	)
	for id, v := range vals {
		if !v.byJournal {
			// An everyday account kept by its balance is the usual way, not a
			// broker's journal set aside.
			if !v.everyday {
				out.PinnedToBalance++
			}
			continue
		}
		out.Accounts++
		out.UnpricedPositions += v.journal.Unpriced
		out.NotTradedPositions += v.journal.NotTraded
		if out.FullDifferenceMinor, err = money.Add(out.FullDifferenceMinor, v.journal.FullMinor-v.journal.Minor); err != nil {
			return out, fmt.Errorf("%w: the full valuation behind the family total, adding account %s", err, id)
		}
		if v.reconciliation != nil && v.reconciliation.Status == apitypes.Differs {
			out.Differing++
			sum, err := money.Add(out.DifferingDifferenceMinor, v.reconciliation.DifferenceMinor)
			if err != nil {
				return out, fmt.Errorf("%w: the differences behind the family total, adding account %s", err, id)
			}
			out.DifferingDifferenceMinor = sum
		}
	}
	return out, nil
}
