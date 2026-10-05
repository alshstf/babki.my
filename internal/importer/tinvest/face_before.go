package tinvest

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/money"
)

// fillFaceBefore puts on every repayment of part of a bond's face value
// (amortization) the face value per unit outstanding just before it, from the
// exchange's schedule, so that the repayment retires the cost basis in
// proportion to the principal it returns (decision Р-4). A bond the exchange
// does not list, a repayment it has no date near, or one in another currency
// than the schedule's keeps the old rule. A schedule that cannot be read stops
// the rebuild, as any source this projection cannot answer without does: a
// repayment measured one way on this run and the other on the next would
// rewrite its row back and forth.
func (r *Rebuilder) fillFaceBefore(ctx context.Context, p *projected) error {
	if r.faces == nil {
		return nil
	}
	var ids []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, d := range p.want {
		if d.op.Type == operation.TypeAmortization && d.op.InstrumentID != nil && !seen[*d.op.InstrumentID] {
			seen[*d.op.InstrumentID] = true
			ids = append(ids, *d.op.InstrumentID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	papers, err := r.resolver.catalog.ByIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("tinvest: read the bonds repaid in part: %w", err)
	}
	for i := range p.want {
		d := &p.want[i]
		if d.op.Type != operation.TypeAmortization || d.op.InstrumentID == nil {
			continue
		}
		isin := papers[*d.op.InstrumentID].ISIN
		if isin == "" {
			continue
		}
		face, currency, found, err := r.faces.FaceBeforeByISIN(ctx, isin, d.op.OccurredOn)
		if err != nil {
			return fmt.Errorf("tinvest: the exchange's repayment schedule of %s: %w", isin, err)
		}
		if !found || currency != d.op.Currency {
			continue
		}
		minor, err := money.Minor(face.Shift(2))
		if err != nil || minor <= 0 {
			continue
		}
		d.op.FaceBeforeMinor = &minor
	}
	return nil
}
