package operation

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/money"
	"babki.my/babki/internal/portfolio"
)

// The tax a foreign dividend lost abroad (decision Р-14). The broker reports
// what arrived; the dividend calendar has the declared amount per share and
// the journal the shares that carried the right, so the gross is per share ×
// shares and the tax is the gross less what arrived. It is an estimate and is
// published as one; where it cannot be made honestly the answer says why.

// dividendCalendar is the stored dividend calendar (marketdata.Store).
type dividendCalendar interface {
	DividendsOf(ctx context.Context, instrumentIDs []uuid.UUID) (map[uuid.UUID][]marketdata.Dividend, error)
}

// instrumentCatalog reads the papers a page names (instrument.Store).
type instrumentCatalog interface {
	ByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]instrument.Instrument, error)
}

// WithDividendCalendar lets the journal estimate the tax withheld abroad from
// foreign dividends; without it none is published.
func (h *Handler) WithDividendCalendar(calendar dividendCalendar, catalog instrumentCatalog) *Handler {
	h.calendar, h.catalog = calendar, catalog
	return h
}

// maxPaymentLag: a payment is matched to a declared dividend whose record date
// is at most this long before it. Payments follow the record date by weeks.
const maxPaymentLag = 120 * 24 * time.Hour

// maxPlausibleRate: no withholding rate reaches half the dividend, so an
// estimate above it says the inputs do not describe this payment.
var maxPlausibleRate = decimal.New(5, -1)

// withheldAbroad answers, for each dividend on the page of a foreign paper or
// with a stated tax, what was withheld abroad. It reads each such row's whole
// account journal: a payment's parts and the shares behind it may lie outside
// the page.
func (h *Handler) withheldAbroad(ctx context.Context, spaceID uuid.UUID, page []Operation, base string,
	rates *marketdata.RateMemo,
) (map[uuid.UUID]apitypes.WithheldAbroad, error) {
	out := map[uuid.UUID]apitypes.WithheldAbroad{}
	var ids, accounts []uuid.UUID
	for _, o := range page {
		if o.Type == TypeDividend && o.InstrumentID != nil {
			ids = append(ids, *o.InstrumentID)
			accounts = append(accounts, o.AccountID)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	statedList, err := h.store.StatedWithheld(ctx, spaceID, accounts)
	if err != nil {
		return nil, err
	}
	stated := make(map[paymentKey]int64, len(statedList))
	for _, w := range statedList {
		stated[w.key()] = w.TaxMinor
	}
	foreign := map[uuid.UUID]bool{}
	calendar := map[uuid.UUID][]marketdata.Dividend{}
	if h.calendar != nil && h.catalog != nil {
		papers, err := h.catalog.ByIDs(ctx, ids)
		if err != nil {
			return nil, err
		}
		var foreignIDs []uuid.UUID
		for id, p := range papers {
			if instrument.ForeignISIN(p.ISIN) {
				foreign[id] = true
				foreignIDs = append(foreignIDs, id)
			}
		}
		if calendar, err = h.calendar.DividendsOf(ctx, foreignIDs); err != nil {
			return nil, err
		}
	}

	journals := map[uuid.UUID][]Operation{}
	for _, o := range page {
		if o.Type != TypeDividend || o.InstrumentID == nil {
			continue
		}
		statedTax, isStated := stated[paymentOf(o)]
		if !isStated && !foreign[*o.InstrumentID] {
			continue
		}
		journal, ok := journals[o.AccountID]
		if !ok {
			if journal, err = h.store.ListForEngine(ctx, spaceID, o.AccountID); err != nil {
				return nil, err
			}
			journals[o.AccountID] = journal
		}
		var w apitypes.WithheldAbroad
		if isStated {
			w = statedWithheld(o, journal, statedTax)
		} else if w, err = estimateWithheld(o, journal, calendar[*o.InstrumentID]); err != nil {
			return nil, err
		}
		if tax, getErr := w.TaxMinor.Get(); getErr == nil && o.Currency != base {
			if res := rates.Rate(ctx, o.Currency, base, portfolio.RateDay(o)); res.Err == nil {
				if minor, err := money.Minor(decimal.NewFromInt(tax).Mul(res.Rate)); err == nil {
					w.TaxInBase = nullable.NewNullableWithValue(apitypes.CurrencyAmount{Currency: base, AmountMinor: minor})
				}
			} else if !errors.Is(res.Err, marketdata.ErrNoRate) {
				return nil, res.Err
			}
		}
		out[o.ID] = w
	}
	return out, nil
}

// emptyWithheld is the answer for d with nothing worked out yet.
func emptyWithheld(d Operation, journal []Operation) apitypes.WithheldAbroad {
	return apitypes.WithheldAbroad{
		Currency: d.Currency, ReceivedMinor: d.AmountMinor, BrokerTax: brokerTaxOn(journal, *d.InstrumentID, d.OccurredOn),
		GrossMinor: nullable.NewNullNullable[int64](), TaxMinor: nullable.NewNullNullable[int64](),
		RatePercent: nullable.NewNullNullable[string](), PerShare: nullable.NewNullNullable[string](),
		Shares: nullable.NewNullNullable[string](), RecordDate: nullable.NewNullNullable[string](),
		TaxInBase:     nullable.NewNullNullable[apitypes.CurrencyAmount](),
		UnknownReason: nullable.NewNullNullable[apitypes.WithheldAbroadUnknownReason](),
	}
}

// statedWithheld is the answer for d when a person stated the tax: what
// arrived that day on the paper, plus the tax, is the gross.
func statedWithheld(d Operation, journal []Operation, tax int64) apitypes.WithheldAbroad {
	w := emptyWithheld(d, journal)
	w.State = apitypes.WithheldAbroadStateStated
	w.ReceivedMinor = 0
	for _, o := range journal {
		if o.Type == TypeDividend && o.InstrumentID != nil && paymentOf(o) == paymentOf(d) {
			w.ReceivedMinor += o.AmountMinor
		}
	}
	gross := w.ReceivedMinor + tax
	w.GrossMinor = nullable.NewNullableWithValue(gross)
	w.TaxMinor = nullable.NewNullableWithValue(tax)
	if gross > 0 {
		w.RatePercent = nullable.NewNullableWithValue(
			decimal.NewFromInt(tax).Div(decimal.NewFromInt(gross)).Shift(2).StringFixed(1))
	}
	return w
}

// estimateWithheld is the answer for one dividend row d of a foreign paper,
// given its account's journal and the paper's calendar.
func estimateWithheld(d Operation, journal []Operation, calendar []marketdata.Dividend) (apitypes.WithheldAbroad, error) {
	w := emptyWithheld(d, journal)
	unknown := func(reason apitypes.WithheldAbroadUnknownReason) (apitypes.WithheldAbroad, error) {
		w.State = apitypes.WithheldAbroadStateUnknown
		w.UnknownReason = nullable.NewNullableWithValue(reason)
		return w, nil
	}
	if len(calendar) == 0 {
		return unknown(apitypes.NoCalendar)
	}
	declared, ok := declaredFor(d.OccurredOn, calendar)
	if !ok {
		return unknown(apitypes.NotInCalendar)
	}
	w.RecordDate = nullable.NewNullableWithValue(declared.RecordDate.Format(time.DateOnly))

	// Every row paying this dividend, the page's or not: a payment may come in parts.
	w.ReceivedMinor = 0
	for _, o := range journal {
		if o.Type == TypeDividend && o.InstrumentID != nil && *o.InstrumentID == *d.InstrumentID {
			if other, ok := declaredFor(o.OccurredOn, calendar); ok && other.RecordDate.Equal(declared.RecordDate) {
				w.ReceivedMinor += o.AmountMinor
			}
		}
	}
	if declared.Currency != d.Currency {
		return unknown(apitypes.AnotherCurrency)
	}

	entitledOn := declared.RecordDate.AddDate(0, 0, -1)
	if declared.LastBuyDate != nil {
		entitledOn = *declared.LastBuyDate
	}
	shares, err := heldAtEndOf(journal, *d.InstrumentID, entitledOn)
	if err != nil {
		return apitypes.WithheldAbroad{}, err
	}
	if !shares.IsPositive() {
		return unknown(apitypes.NoHolding)
	}
	gross, err := money.Minor(declared.PerShare.Mul(shares).Shift(2))
	if err != nil || gross <= 0 {
		return unknown(apitypes.Implausible)
	}
	tax := gross - w.ReceivedMinor
	if tax < 0 && tax >= -1 {
		// What arrived matches the gross to the rounding of a minor unit.
		tax = 0
	}
	rate := decimal.NewFromInt(tax).Div(decimal.NewFromInt(gross))
	if tax < 0 || rate.GreaterThan(maxPlausibleRate) {
		return unknown(apitypes.Implausible)
	}
	if tax == 0 && len(w.BrokerTax) > 0 {
		// Paid whole, the tax as the broker's own row: that row is the answer.
		w.State = apitypes.WithheldAbroadStateReported
		return w, nil
	}
	w.State = apitypes.WithheldAbroadStateEstimated
	w.GrossMinor = nullable.NewNullableWithValue(gross)
	w.TaxMinor = nullable.NewNullableWithValue(tax)
	w.RatePercent = nullable.NewNullableWithValue(rate.Shift(2).StringFixed(1))
	w.PerShare = nullable.NewNullableWithValue(declared.PerShare.String())
	w.Shares = nullable.NewNullableWithValue(shares.String())
	return w, nil
}

// declaredFor is the declared dividend a payment on day pays: the one whose
// payment date is that day, else the latest whose record date lies at most
// maxPaymentLag before it.
func declaredFor(day time.Time, calendar []marketdata.Dividend) (marketdata.Dividend, bool) {
	for _, d := range calendar {
		if d.PaymentDate != nil && d.PaymentDate.Equal(day) {
			return d, true
		}
	}
	var best marketdata.Dividend
	found := false
	for _, d := range calendar {
		if d.RecordDate.After(day) || day.Sub(d.RecordDate) > maxPaymentLag {
			continue
		}
		if !found || d.RecordDate.After(best.RecordDate) {
			best, found = d, true
		}
	}
	return best, found
}

// heldAtEndOf is how many units of the paper the journal holds once everything
// dated day or earlier is folded.
func heldAtEndOf(journal []Operation, instrumentID uuid.UUID, day time.Time) (decimal.Decimal, error) {
	var upTo []Operation
	for _, o := range journal {
		if !o.OccurredOn.After(day) {
			upTo = append(upTo, o)
		}
	}
	positions, err := portfolio.Compute(upTo)
	if err != nil {
		return decimal.Zero, err
	}
	if p, ok := positions[instrumentID]; ok {
		return p.Quantity, nil
	}
	return decimal.Zero, nil
}

// brokerTaxOn sums, per currency, the tax rows the broker wrote on the paper
// on day.
func brokerTaxOn(journal []Operation, instrumentID uuid.UUID, day time.Time) []apitypes.CurrencyAmount {
	byCurrency := map[string]int64{}
	for _, o := range journal {
		if o.Type == TypeTax && o.InstrumentID != nil && *o.InstrumentID == instrumentID && o.OccurredOn.Equal(day) {
			byCurrency[o.Currency] += o.AmountMinor
		}
	}
	out := make([]apitypes.CurrencyAmount, 0, len(byCurrency))
	for currency, minor := range byCurrency {
		out = append(out, apitypes.CurrencyAmount{Currency: currency, AmountMinor: minor})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}
