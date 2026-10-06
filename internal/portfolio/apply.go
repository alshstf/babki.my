package portfolio

import (
	"fmt"
)

// applier folds one operation of its type into the position it concerns.
type applier func(p *Position, o Operation) error

// appliers is every type the engine folds into a position; a new type is a new
// entry here and its function below. Compute refuses a type with none.
var appliers = map[Type]applier{
	TypeBuy:          applyBuy,
	TypeSell:         applyDisposal,
	TypeRedemption:   applyDisposal,
	TypeDividend:     applyIncome,
	TypeCoupon:       applyIncome,
	TypeTax:          applyIncome,
	TypeFee:          applyFee,
	TypeAmortization: applyAmortization,
	TypeTransferOut:  applyDeparture,
	TypeExchangeOut:  applyDeparture,
	TypeTransferIn:   applyArrival,
	TypeExchangeIn:   applyArrival,
	TypeSpinoffIn:    applyArrival,
	TypeSpinoffOut:   applySpinoffOut,
	TypeSplit:        applySplitOp,
}

// checkShape refuses an entry missing what its type needs, before any position
// is touched.
func checkShape(o Operation) error {
	switch o.Type {
	case TypeBuy, TypeSell, TypeRedemption,
		TypeTransferIn, TypeTransferOut,
		TypeExchangeOut, TypeExchangeIn, TypeSpinoffIn:
		if o.Quantity == nil || !o.Quantity.IsPositive() {
			return badOp(o, "positive quantity required")
		}
	case TypeSpinoffOut:
		// A spin-off's departing leg moves no units; a quantity would read as units
		// leaving. Its pieces carry the lots' counts as identity.
		if o.Quantity != nil {
			return badOp(o, "a spin-off moves no units, so it must carry no quantity")
		}
	case TypeSplit:
		if o.SplitRatio == nil || !o.SplitRatio.IsPositive() {
			return badOp(o, "positive split_ratio required")
		}
	}
	return nil
}

// wrapOp names the entry an error was reached at.
func wrapOp(o Operation, err error) error {
	return fmt.Errorf("%s %s %s: %w", o.Type, o.InstrumentID, o.OccurredOn.Format("2006-01-02"), err)
}

func applyBuy(p *Position, o Operation) error {
	if o.AmountMinor >= 0 {
		return badOp(o, "buy amount must be negative")
	}
	// A purchase dates its lot; the copy keeps the lot off the journal entry.
	boughtOn := o.OccurredOn
	p.addLot(*o.Quantity, -o.AmountMinor+o.FeeMinor, &boughtOn, settledCopy(o))
	if err := p.addFee(o.Currency, o.FeeMinor); err != nil {
		return wrapOp(o, err)
	}
	return nil
}

// applyDisposal is a sale or a redemption: they differ in what happened, not
// in the computation.
func applyDisposal(p *Position, o Operation) error {
	if o.AmountMinor <= 0 {
		return badOp(o, fmt.Sprintf("%s amount must be positive", o.Type))
	}
	pieces, err := p.releaseFIFO(*o.Quantity)
	if err != nil {
		return wrapOp(o, err)
	}
	p.realize(Realization{
		OccurredOn:    o.OccurredOn,
		RateOn:        settledCopy(o),
		ProceedsMinor: o.AmountMinor,
		Currency:      o.Currency,
		FeeMinor:      o.FeeMinor,
		Released:      pieces,
	})
	if err := p.addFee(o.Currency, o.FeeMinor); err != nil {
		return wrapOp(o, err)
	}
	return nil
}

// applyIncome books a payment in the currency it arrived in; taxes arrive as
// negative amounts.
func applyIncome(p *Position, o Operation) error {
	if err := p.addIncome(o.Currency, o.AmountMinor); err != nil {
		return wrapOp(o, err)
	}
	return nil
}

// applyFee books a commission charged on the paper outside a trade (a
// depositary's, a transfer's): a negative amount is a positive fee, and it is
// what holding the paper cost, so it comes off its income as a tax does.
func applyFee(p *Position, o Operation) error {
	if err := p.addFee(o.Currency, -o.AmountMinor); err != nil {
		return wrapOp(o, err)
	}
	if err := p.addIncome(o.Currency, o.AmountMinor); err != nil {
		return wrapOp(o, err)
	}
	return nil
}

// applyAmortization retires basis for returned principal; in this currency only
// the excess is a result. It is still recorded as a disposal, since in roubles
// the covered part is not neutral: principal comes back at its day's rate
// against basis struck at purchase rates. Its pieces carry cost and date, no
// quantity; no fee is attributed.
func applyAmortization(p *Position, o Operation) error {
	if o.AmountMinor <= 0 {
		return badOp(o, "amortization amount must be positive")
	}
	// Principal cannot come back from a paper this account never acquired:
	// otherwise a mistyped instrument credits the whole payment as a gain. The
	// test is "ever acquired here", not "holds shares or basis now", which would
	// refuse journals this program writes: a fully amortized bond still held, and
	// a final repayment folded after the same-day redemption.
	if !p.heldALot {
		return badOp(o, fmt.Sprintf(
			"returns principal on instrument %s, which this account has never acquired: with no purchase behind it the whole payment would be recorded as realized profit; record how the paper was acquired (a buy, or a transfer carrying its basis) first",
			o.InstrumentID))
	}
	var released []ReleasedLot
	if share, ok := amortizedShare(o, p); ok {
		// As the tax code does it (decision Р-4): the repayment retires its share of
		// the outstanding principal from every lot; the rest is this year's result.
		released = takeLotsShare(p, share)
	} else {
		// No face value: the old rule retires basis until none is left.
		released = drainLotsCost(p, min(o.AmountMinor, p.CostMinor))
	}
	p.CostMinor -= LotsCost(released)
	p.realize(Realization{
		OccurredOn:    o.OccurredOn,
		ProceedsMinor: o.AmountMinor,
		Currency:      o.Currency,
		Released:      released,
	})
	return nil
}

// applyDeparture is the leaving leg of a transfer or a conversion: it gives up
// exactly what the breakdown says went (releaseRecorded).
func applyDeparture(p *Position, o Operation) error {
	if len(o.TransferLots) == 0 {
		// A conversion always has a breakdown: the arriving leg is built from the
		// departing leg's pieces, and the registry writes both or neither.
		if o.Type == TypeExchangeOut {
			return badOp(o, "a conversion must carry the breakdown of the lots it converted")
		}
		// No breakdown: the arriving leg's basis was given by hand, so a fresh slice
		// of the queue is released and its cost discarded. The one case the legs are
		// not reconciled, legitimately.
		if _, err := p.releaseFIFO(*o.Quantity); err != nil {
			return wrapOp(o, err)
		}
		return nil
	}
	if err := CheckTransferLots(o); err != nil {
		return err
	}
	return p.releaseRecorded(o)
}

// applyArrival is the arriving leg of a transfer, a conversion or a spin-off:
// the released lots are rebuilt in order, each with its quantity, cost and day
// — a transfer is not a purchase and reprices nothing, and a conversion's
// pieces are the departing ones restated in the new paper's units.
func applyArrival(p *Position, o Operation) error {
	if o.AmountMinor < 0 {
		return badOp(o, fmt.Sprintf("%s amount (cost basis) must be >= 0", o.Type))
	}
	if len(o.TransferLots) == 0 {
		// A conversion's or a spin-off's arriving leg without pieces came from
		// nowhere.
		if o.Type == TypeExchangeIn {
			return badOp(o, "a conversion must carry the breakdown of the lots it converted")
		}
		if o.Type == TypeSpinoffIn {
			return badOp(o, "a spin-off must carry the breakdown of the lots whose basis it moved")
		}
		// No breakdown (basis given by hand, or a transfer from before breakdowns):
		// the lot's purchase date is unknown, and the transfer's own date would be a
		// false one.
		p.addLot(*o.Quantity, o.AmountMinor, nil, nil)
		return nil
	}
	if err := CheckTransferLots(o); err != nil {
		return err
	}
	for _, pc := range o.TransferLots {
		p.addLot(pc.Quantity, pc.CostMinor, pc.AcquiredOn, pc.RateOn)
	}
	return nil
}

func applySpinoffOut(p *Position, o Operation) error {
	if o.AmountMinor < 0 {
		return badOp(o, "the basis a spin-off moves must be >= 0")
	}
	if err := CheckSpinoffLots(o); err != nil {
		return err
	}
	return p.applySpinoffOut(o)
}

// applySplitOp rewrites quantities only, kept on the journal's scale.
func applySplitOp(p *Position, o Operation) error {
	p.applySplit(*o.SplitRatio)
	return nil
}
