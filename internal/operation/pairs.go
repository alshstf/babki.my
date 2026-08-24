package operation

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/portfolio"
)

// BuildExchange and BuildSpinoff are the arithmetic of a corporate action's
// journal pair, with no database under them: given an account's journal and
// what the registry says happened, they answer with the two legs — the parcels
// released, the basis they carry, the currency, and the counts on the scale the
// columns store.
//
// THEY EXIST BECAUSE TWO PATHS WRITE THESE PAIRS AND ONLY ONE ARITHMETIC MAY
// DECIDE THEM. CreateExchange and CreateSpinoff write one pair against one
// locked account; the corporate-actions materializer writes many, for every
// account that held the paper, as one difference against the journal (see
// internal/corporateaction). Before these functions the second path had no way
// to reach the first's reasoning except by restating it, and a restated FIFO
// release is exactly the pair of independent computations of one figure this
// codebase has watched drift more than once.
//
// WHAT THEY DO NOT DO is take a lock, read a journal, check the result against
// the engine, or name the row: the journal is the caller's to supply and to
// have locked, and Source, ExternalID, TransferGroupID and Note are the
// caller's to set. What they return is not yet a row anyone may write.

// BuildExchange returns the exchange_out/exchange_in pair for a conversion of
// p.Quantity units of one paper into p.ToQuantity units of another, resolved
// against journal as it stood on p.OccurredOn.
//
// The counts are truncated to the journal's scale first, and DOWNWARDS: "convert
// everything I hold" must never round up past the position it empties.
func BuildExchange(journal []Operation, p ExchangeParams) (out, in Operation, err error) {
	if err := checkExchangeParams(p); err != nil {
		return Operation{}, Operation{}, err
	}
	quantity := p.Quantity.Truncate(quantityScale)
	toQuantity := p.ToQuantity.Truncate(quantityScale)
	if !quantity.IsPositive() || !toQuantity.IsPositive() {
		return Operation{}, Operation{}, fmt.Errorf("%w: a quantity is finer than the %d decimal places the journal records",
			family.ErrValidation, quantityScale)
	}
	if err := checkQuantityBound(quantity); err != nil {
		return Operation{}, Operation{}, err
	}
	if err := checkQuantityBound(toQuantity); err != nil {
		return Operation{}, Operation{}, err
	}

	// The currency the old paper's cost is denominated in, read off the
	// account's own history exactly as a transfer reads it. The new paper
	// inherits it, and must: what arrives is the money that was paid, and that
	// money has a currency of its own regardless of what the new paper is quoted
	// in. If the account already holds the new paper in a different currency the
	// engine refuses the pair, loudly, rather than mixing two currencies inside
	// one basis.
	currency := currencyOf(journal, p.FromInstrumentID)
	if currency == "" {
		return Operation{}, Operation{}, fmt.Errorf("%w: no history for the instrument being converted", family.ErrValidation)
	}

	// Resolved against the journal as it stood on the conversion's own date, not
	// against the end state: a backdated conversion is replayed at its
	// chronological place, where the FIFO front is a different one.
	lots, err := portfolio.ReleasedLots(journalUpTo(journal, p.OccurredOn), p.FromInstrumentID, quantity)
	if err != nil {
		return Operation{}, Operation{}, fmt.Errorf("%w: %v", ErrInconsistent, err)
	}
	lots = quantizeLots(lots, quantity)
	cost := portfolio.LotsCost(lots)
	arriving := rescaleLots(lots, quantity, toQuantity)
	// NOT AN ARGUMENT ABOUT rescaleLots BUT A CHECK ON IT. The two legs carry one
	// and the same amount_minor, and portfolio.CheckTransferLots holds each leg's
	// pieces to the amount of the row carrying them — so a basis lost or invented
	// in the restating would surface as a refusal on every later read of this
	// account rather than here. Saying it outright, on the write that caused it,
	// is the difference between a bug reported against the request that made it
	// and one reported against whoever next opens the screen.
	if got := portfolio.LotsCost(arriving); got != cost {
		return Operation{}, Operation{}, fmt.Errorf(
			"restating the breakdown in the new paper's units changed the basis from %d to %d minor units", cost, got)
	}

	out = Operation{
		AccountID: p.AccountID, InstrumentID: &p.FromInstrumentID, Type: TypeExchangeOut,
		OccurredOn: p.OccurredOn, Quantity: &quantity, AmountMinor: cost,
		Currency: currency, Note: p.Note, Source: p.Source,
		TransferLots: lots,
	}
	in = Operation{
		AccountID: p.AccountID, InstrumentID: &p.ToInstrumentID, Type: TypeExchangeIn,
		OccurredOn: p.OccurredOn, Quantity: &toQuantity, AmountMinor: cost,
		Currency: currency, Note: p.Note, Source: p.Source,
		TransferLots: arriving,
	}
	return out, in, nil
}

// BuildSpinoff returns the spinoff_out/spinoff_in pair for a carve-out of
// p.BasisShare of the cost onto a second paper, resolved against journal as it
// stood on p.OccurredOn.
//
// HOW MANY UNITS ARRIVE IS DECIDED HERE and is not the caller's to state: it
// follows from the holding, and a caller passing an absolute count would be a
// second computation of the same figure made against a journal that may have
// moved (see SpinoffParams).
func BuildSpinoff(journal []Operation, p SpinoffParams) (out, in Operation, err error) {
	if err := checkSpinoffParams(p); err != nil {
		return Operation{}, Operation{}, err
	}

	// The holding as it stood at the START of the day the spin-off took effect —
	// the same moment the registry decides against, and the same reason a
	// backdated conversion resolves its lots against its own date: the event is
	// replayed at its chronological place, where the parcels are the parcels of
	// that day.
	positions, err := portfolio.Compute(journalUpTo(journal, p.OccurredOn))
	if err != nil {
		return Operation{}, Operation{}, fmt.Errorf("%w: %v", ErrInconsistent, err)
	}
	held, ok := positions[p.FromInstrumentID]
	if !ok || !held.Quantity.IsPositive() {
		return Operation{}, Operation{}, fmt.Errorf("%w: this account held nothing of the paper the spin-off comes out of on %s",
			family.ErrValidation, p.OccurredOn.Format(time.DateOnly))
	}

	toQuantity := held.Quantity.Mul(p.RatioTo).Div(p.RatioFrom).Truncate(quantityScale)
	if !toQuantity.IsPositive() {
		return Operation{}, Operation{}, fmt.Errorf("%w: %s units at %s for %s comes to less than the %d decimal places the journal records",
			family.ErrValidation, held.Quantity, p.RatioTo, p.RatioFrom, quantityScale)
	}
	if err := checkQuantityBound(toQuantity); err != nil {
		return Operation{}, Operation{}, err
	}

	pieces := portfolio.SpinoffPieces(held.Lots, p.BasisShare)
	cost := portfolio.LotsCost(pieces)
	if cost <= 0 {
		return Operation{}, Operation{}, fmt.Errorf(
			"%w: %s of the %d minor this account paid for the paper rounds to nothing, so the spin-off would move no money at all",
			family.ErrValidation, p.BasisShare, held.CostMinor)
	}
	// The arriving parcel: the same money and the same days, restated in the new
	// paper's units by the one allocation this package has (see rescaleLots). The
	// departing leg's pieces are NOT restated — they name the original's own
	// parcels, which is what a later replay matches them against.
	arriving := rescaleLots(pieces, held.Quantity, toQuantity)
	if got := portfolio.LotsCost(arriving); got != cost {
		return Operation{}, Operation{}, fmt.Errorf(
			"restating the breakdown in the carved-out paper's units changed the basis from %d to %d minor units", cost, got)
	}

	out = Operation{
		AccountID: p.AccountID, InstrumentID: &p.FromInstrumentID, Type: TypeSpinoffOut,
		OccurredOn: p.OccurredOn, AmountMinor: cost,
		Currency: held.Currency, Note: p.Note, Source: p.Source,
		TransferLots: pieces,
	}
	in = Operation{
		AccountID: p.AccountID, InstrumentID: &p.ToInstrumentID, Type: TypeSpinoffIn,
		OccurredOn: p.OccurredOn, Quantity: &toQuantity, AmountMinor: cost,
		Currency: held.Currency, Note: p.Note, Source: p.Source,
		TransferLots: arriving,
	}
	return out, in, nil
}

// checkExchangeParams is everything about a conversion that can be judged
// without a journal.
func checkExchangeParams(p ExchangeParams) error {
	if p.Source != SourceRegistry {
		return fmt.Errorf("%w: a conversion is only written by source=%s", family.ErrValidation, SourceRegistry)
	}
	if p.FromInstrumentID == uuid.Nil || p.ToInstrumentID == uuid.Nil {
		return fmt.Errorf("%w: from and to instruments are required", family.ErrValidation)
	}
	// THE SAME PAPER ON BOTH SIDES IS NOT A CONVERSION, it is a split written
	// with extra steps — and it would fold as one position releasing lots and
	// immediately re-adding them, whose result depends on the order of two rows
	// sharing a date. A split is the type for "the same paper, a different count"
	// and it already exists.
	if p.FromInstrumentID == p.ToInstrumentID {
		return fmt.Errorf("%w: from and to instruments must differ; the same paper in a new count is a split", family.ErrValidation)
	}
	if !p.Quantity.IsPositive() || !p.ToQuantity.IsPositive() {
		return fmt.Errorf("%w: both quantities must be positive", family.ErrValidation)
	}
	return checkOccurredOn(p.OccurredOn)
}

// checkSpinoffParams is everything about a spin-off that can be judged without a
// journal.
func checkSpinoffParams(p SpinoffParams) error {
	if p.Source != SourceRegistry {
		return fmt.Errorf("%w: a spin-off is only written by source=%s", family.ErrValidation, SourceRegistry)
	}
	if p.FromInstrumentID == uuid.Nil || p.ToInstrumentID == uuid.Nil {
		return fmt.Errorf("%w: from and to instruments are required", family.ErrValidation)
	}
	// THE SAME PAPER ON BOTH SIDES IS NOT A SPIN-OFF. It would take money out of
	// the position's parcels and add it back to the same position as new parcels
	// — the basis unchanged, the parcel list doubled, and the FIFO queue silently
	// rearranged.
	if p.FromInstrumentID == p.ToInstrumentID {
		return fmt.Errorf("%w: a spin-off must name a different paper than the one it comes out of", family.ErrValidation)
	}
	if !p.RatioFrom.IsPositive() || !p.RatioTo.IsPositive() {
		return fmt.Errorf("%w: both sides of the ratio must be positive", family.ErrValidation)
	}
	// STRICTLY BETWEEN NOTHING AND EVERYTHING. A share of 0 moves no money and
	// would write a pair that says nothing; a share of 1 moves ALL of it, which is
	// a conversion — the original paper would be left holding units with no basis
	// behind them, so every later sale of it would show the whole proceeds as
	// profit. The registry refuses both as well (corporateaction.Event's
	// Validate); it is stated here too because this function does not route
	// through that one, and a rule only the other door enforces is a rule this
	// door does not have.
	if !p.BasisShare.IsPositive() || !p.BasisShare.LessThan(decimal.NewFromInt(1)) {
		return fmt.Errorf("%w: the share of the basis that moves must be greater than 0 and less than 1", family.ErrValidation)
	}
	return checkOccurredOn(p.OccurredOn)
}

// currencyOf is the currency an account's cost in one paper is denominated in,
// read off the newest row that names it.
func currencyOf(journal []Operation, instrumentID uuid.UUID) string {
	for i := len(journal) - 1; i >= 0; i-- {
		o := journal[i]
		if o.InstrumentID != nil && *o.InstrumentID == instrumentID {
			return o.Currency
		}
	}
	return ""
}
