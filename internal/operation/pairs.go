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
// journal pair with no database under them. Both CreateExchange/CreateSpinoff and
// the corporate-actions materializer use them, so one arithmetic decides every
// pair. They take no lock, read nothing and check nothing against the engine:
// the journal is the caller's to supply and lock, and Source, ExternalID,
// TransferGroupID and Note are the caller's to set.

// BuildExchange returns the exchange_out/exchange_in pair converting p.Quantity
// units of one paper into p.ToQuantity of another, against journal as of
// p.OccurredOn. Counts are truncated down to the journal's scale, so "convert
// everything" never rounds past the position.
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

	// The new paper inherits the currency the old paper's cost was paid in.
	// If the account already holds it in another currency the engine refuses
	// the pair rather than mixing currencies in one basis.
	currency := currencyOf(journal, p.FromInstrumentID)
	if currency == "" {
		return Operation{}, Operation{}, fmt.Errorf("%w: no history for the instrument being converted", family.ErrValidation)
	}

	// Released as of the conversion's own date, where the replay puts it.
	lots, err := portfolio.ReleasedLots(FoldedBefore(journal, p.OccurredOn, p.Source), p.FromInstrumentID, quantity)
	if err != nil {
		return Operation{}, Operation{}, fmt.Errorf("%w: %v", ErrInconsistent, err)
	}
	lots = quantizeLots(lots, quantity)
	cost := portfolio.LotsCost(lots)
	arriving := rescaleLots(lots, quantity, toQuantity)
	// A check on rescaleLots: a basis lost in restating would otherwise
	// surface on every later read rather than on this write.
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

// BuildSpinoff returns the spinoff_out/spinoff_in pair carving p.BasisShare of
// the cost onto a second paper, against journal as of p.OccurredOn. The arriving
// count follows from the holding and is computed here (see SpinoffParams).
func BuildSpinoff(journal []Operation, p SpinoffParams) (out, in Operation, err error) {
	if err := checkSpinoffParams(p); err != nil {
		return Operation{}, Operation{}, err
	}

	// The holding the row will find on replay: a registry row folds at the
	// start of its day (see FoldedBefore).
	positions, err := portfolio.Compute(FoldedBefore(journal, p.OccurredOn, p.Source))
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
	// A share that rounds to nothing was meant to move money and moves none;
	// a share of 0 moves none on purpose (decision Р-16).
	if cost <= 0 && p.BasisShare.IsPositive() {
		return Operation{}, Operation{}, fmt.Errorf(
			"%w: %s of the %d minor this account paid for the paper rounds to nothing, so the spin-off would move no money at all",
			family.ErrValidation, p.BasisShare, held.CostMinor)
	}
	// The arriving parcel: same money and days in the new paper's units (see
	// rescaleLots). The departing pieces stay in the original's units, which
	// is what a replay matches them against.
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
	// The same paper on both sides is a split, which has its own type.
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
	// The same paper on both sides would rearrange the FIFO queue and
	// double the parcel list with the basis unchanged.
	if p.FromInstrumentID == p.ToInstrumentID {
		return fmt.Errorf("%w: a spin-off must name a different paper than the one it comes out of", family.ErrValidation)
	}
	if !p.RatioFrom.IsPositive() || !p.RatioTo.IsPositive() {
		return fmt.Errorf("%w: both sides of the ratio must be positive", family.ErrValidation)
	}
	// 0 moves no money (the broker's way, Р-16); 1 would be a conversion and
	// leave the original with no basis. corporateaction.Event.Validate
	// refuses the same, but this path does not go through it.
	if p.BasisShare.IsNegative() || !p.BasisShare.LessThan(decimal.NewFromInt(1)) {
		return fmt.Errorf("%w: the share of the basis that moves must be at least 0 and less than 1", family.ErrValidation)
	}
	return checkOccurredOn(p.OccurredOn)
}

// currencyOf is the currency of an account's cost in one paper, read off the
// newest row naming it.
func currencyOf(journal []Operation, instrumentID uuid.UUID) string {
	for i := len(journal) - 1; i >= 0; i-- {
		o := journal[i]
		if o.InstrumentID != nil && *o.InstrumentID == instrumentID {
			return o.Currency
		}
	}
	return ""
}
