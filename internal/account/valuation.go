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

	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/money"
)

// JournalValue is what a brokerage account is worth by its operations journal,
// as the portfolio engine values it (portfolio.Handler.ValueFromJournal).
// cmd/babki hands the engine in, so this package does not import it.
type JournalValue struct {
	// Currency is the space's base currency, the one Minor is in.
	Currency string
	// Minor is the holdings at market value plus the cash, converted.
	Minor int64
	// ByCurrency is the same worth before conversion, by the currency it is
	// struck in.
	ByCurrency map[string]int64
	// Operations is how many operations the journal holds; nought means the
	// account is not kept by its operations at all.
	Operations int
	// Unpriced counts the holdings with no valuation, counted as nothing.
	Unpriced int
	// MissingRates names the currencies left out of Minor for want of a rate.
	MissingRates []string
	// NegativeCash names the currencies whose cash by the journal is below zero.
	NegativeCash []string
}

type journalValuer interface {
	ValueFromJournal(ctx context.Context, spaceID, accountID uuid.UUID) (JournalValue, error)
	// ValueOn is the same worth as the journal stood at the end of day, at
	// that day's prices and rates.
	ValueOn(ctx context.Context, spaceID, accountID uuid.UUID, day time.Time) (JournalValue, error)
	// ValuesOn is ValueOn for several days.
	ValuesOn(ctx context.Context, spaceID, accountID uuid.UUID, days []time.Time) ([]JournalValue, error)
	// ReturnBasis is what the account's period from (exclusive) to to
	// (inclusive) is reckoned from.
	ReturnBasis(ctx context.Context, spaceID, accountID uuid.UUID, from, to time.Time) (ReturnBasis, error)
}

// ReturnBasis mirrors the engine's: an account's worth at a period's two ends
// and the money that crossed its edge in between, signed from the investor's
// side and in the base currency.
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

// How far the journal may stand from the balance and still be said to agree
// with it. A broker strikes its figure at the last trade and this program at
// the previous session's close, so a gap of a percent or two is normal and is
// named as such; past five percent the likelier cause is operations missing
// from the journal. Thresholds are this program's own choice (the owner left
// them to it, Р-2, 2026-10-02), not anybody's standard.
const (
	agreesWithinPercent = 1
	closeWithinPercent  = 5
	// A balance mark older than this is compared with the journal as it stood
	// on the mark's own day, at that day's prices: the market has moved since,
	// and comparing it with today's worth would blame the journal for that.
	staleAfterDays = 3
)

// valuation is how one account stands against the family total.
type valuation struct {
	// journal is nil for an account not kept by its operations.
	journal        *JournalValue
	byJournal      bool
	reconciliation *apitypes.AccountReconciliation
}

// valuations values from their journals the active brokerage accounts that
// have one, and reconciles each against its latest balance mark. An account
// with no entry here is counted by its balance.
func (h *Handler) valuations(ctx context.Context, spaceID uuid.UUID, accounts []WithBalance, baseCurrency string, now time.Time, rates map[rateKey]*rateLookup) (map[uuid.UUID]valuation, error) {
	out := make(map[uuid.UUID]valuation)
	if h.journals == nil {
		return out, nil
	}
	for _, a := range accounts {
		if a.Type != TypeBrokerage || a.Status != StatusActive {
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
		out[a.ID] = valuation{journal: &v, byJournal: !a.ValuedByBalance, reconciliation: rec}
	}
	return out, nil
}

// reconcile sets the journal's figure against a's latest balance mark, both in
// the base currency. A recent mark is compared with today's worth at today's
// rate; an older one with the journal's worth on the mark's own day, at that
// day's prices and rate — and when the journal cannot be valued whole on that
// day (no operations yet, a paper with no price, a currency with no rate) no
// verdict is given. Nil when there is no mark, or no rate for it.
func (h *Handler) reconcile(ctx context.Context, a WithBalance, v JournalValue, baseCurrency string, now time.Time, rates map[rateKey]*rateLookup) (*apitypes.AccountReconciliation, error) {
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

// reconciliationStatus grades a difference against the balance it is a
// difference from. A balance of nought agrees only with a journal of nought.
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
		AmountMinor:       v.journal.Minor,
		Currency:          v.journal.Currency,
		UnpricedPositions: v.journal.Unpriced,
		MissingRates:      nonNil(v.journal.MissingRates),
		NegativeCash:      nonNil(v.journal.NegativeCash),
		Reconciliation:    nullable.NewNullNullable[apitypes.AccountReconciliation](),
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

// addJournals folds the accounts counted by their journal into the per-currency
// totals of the ones counted by their balance: what an account holds in a
// currency goes among the assets, or among the debts where its cash in that
// currency is below zero by more than its holdings in it.
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
	var out apitypes.SummaryJournal
	for id, v := range vals {
		if !v.byJournal {
			out.PinnedToBalance++
			continue
		}
		out.Accounts++
		out.UnpricedPositions += v.journal.Unpriced
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
