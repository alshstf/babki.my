package portfolio

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
)

// SaleLots is a paper an account holds, as far as an estimate of selling part
// of it needs (the rebalance of decision Р-37 В): what it can be sold for now
// in the base currency — nil when it cannot (a frozen paper, one without a
// recent price, no rate) — and its lots in release order.
type SaleLots struct {
	InstrumentID uuid.UUID
	Type         instrument.Type
	Quantity     decimal.Decimal
	LiquidInBase *int64
	Lots         []HeldLot
}

// HeldLot is a lot still held: its quantity, its day — nil when unknown — and
// its cost in the base currency at that day's rate (the purchase's settlement
// day when known, НК РФ ст. 210 п. 5). CostKnown is false when the cost
// cannot be struck: an undated lot in another currency, a missing rate.
type HeldLot struct {
	Quantity   decimal.Decimal
	AcquiredOn *time.Time
	CostInBase int64
	CostKnown  bool
}

// ForSale are the papers the account holds, for an estimate of a sale. The
// liquid value is the positions screen's, so the two never disagree.
func (s *Service) ForSale(ctx context.Context, spaceID, accountID uuid.UUID) ([]SaleLots, error) {
	resp, _, err := s.Positions(ctx, spaceID, accountID)
	if err != nil {
		return nil, err
	}
	sp, err := s.spaces.SpaceByID(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	ops, err := s.ops.ListForEngine(ctx, spaceID, accountID)
	if err != nil {
		return nil, err
	}
	positions, err := Compute(ops)
	if err != nil {
		return nil, journalDoesNotCompute{err}
	}
	rates := marketdata.NewRateMemo(s.conv)
	out := make([]SaleLots, 0, len(resp.Positions))
	for _, ap := range resp.Positions {
		p, ok := positions[ap.Instrument.Id]
		if !ok || !p.Quantity.IsPositive() {
			continue
		}
		h := SaleLots{InstrumentID: p.InstrumentID, Type: instrument.Type(ap.Instrument.Type), Quantity: p.Quantity}
		if ap.LiquidValueInBaseMinor.IsSpecified() && !ap.LiquidValueInBaseMinor.IsNull() {
			v := ap.LiquidValueInBaseMinor.MustGet()
			h.LiquidInBase = &v
		}
		for _, l := range p.Lots {
			lot := HeldLot{Quantity: l.Quantity, AcquiredOn: l.AcquiredOn}
			switch {
			case p.Currency == sp.BaseCurrency:
				lot.CostInBase, lot.CostKnown = l.CostMinor, true
			case DatelessBasis(l.AcquiredOn, l.CostMinor):
				// No day whose rate could value it.
			case l.AcquiredOn == nil:
				// Bought for nothing: nought at any rate.
				lot.CostKnown = true
			default:
				on := *l.AcquiredOn
				if l.RateOn != nil {
					on = *l.RateOn
				}
				lot.CostInBase, lot.CostKnown, err = convertAt(ctx, rates, l.CostMinor, p.Currency, sp.BaseCurrency, on)
				if err != nil {
					return nil, err
				}
			}
			h.Lots = append(h.Lots, lot)
		}
		out = append(out, h)
	}
	return out, nil
}
