package operation

import (
	"context"

	"github.com/google/uuid"

	"babki.my/babki/internal/portfolio"
)

// StatedBasisChanges answers, for each row of ops that is half of a move of
// shares between two of the family's accounts with a basis given by hand
// (TransferParams.CostMinorOverride), how much the family's basis of those
// shares changed across the move: the figure given, less what the departing
// account's own queue held for them at that moment. Both halves of a pair get
// the same answer. Rows of any other kind are absent from the result.
//
// The departing account gives up its own parcels whatever was typed, and the
// arriving one takes the typed figure (see Compute's transfer_out branch), so
// the family's total moves by exactly this much — the change the journal shows
// beside the pair rather than leaving it unexplained.
//
// A departing journal that does not replay up to the move leaves its pair out:
// the listing still answers, and the account's own screen says what is wrong.
func (s *Service) StatedBasisChanges(ctx context.Context, spaceID uuid.UUID, ops []Operation) (map[uuid.UUID]int64, error) {
	out := map[uuid.UUID]int64{}
	byGroup := map[uuid.UUID]*int64{}
	journals := map[uuid.UUID][]Operation{}
	for _, o := range ops {
		if !statedBasis(o) {
			continue
		}
		change, seen := byGroup[*o.TransferGroupID]
		if !seen {
			var err error
			change, err = s.statedBasisChange(ctx, spaceID, *o.TransferGroupID, journals)
			if err != nil {
				return nil, err
			}
			byGroup[*o.TransferGroupID] = change
		}
		if change != nil {
			out[o.ID] = *change
		}
	}
	return out, nil
}

// statedBasis is whether o is half of a move of shares with a basis given by
// hand: a transfer pair with no breakdown, which only a typed basis leaves.
func statedBasis(o Operation) bool {
	return o.TransferGroupID != nil && (o.Type == TypeTransferOut || o.Type == TypeTransferIn) &&
		len(o.TransferLots) == 0 && o.InstrumentID != nil && o.Quantity != nil
}

// statedBasisChange is StatedBasisChanges for one pair, nil when it cannot be
// told. journals caches the departing accounts' journals across pairs.
func (s *Service) statedBasisChange(ctx context.Context, spaceID, group uuid.UUID, journals map[uuid.UUID][]Operation) (*int64, error) {
	legs, err := s.store.ByTransferGroup(ctx, spaceID, group)
	if err != nil {
		return nil, err
	}
	for _, leg := range legs {
		if leg.Type != TypeTransferOut || leg.InstrumentID == nil || leg.Quantity == nil {
			continue
		}
		journal, ok := journals[leg.AccountID]
		if !ok {
			if journal, err = s.store.ListForEngine(ctx, spaceID, leg.AccountID); err != nil {
				return nil, err
			}
			journals[leg.AccountID] = journal
		}
		held, err := portfolio.ReleasedCost(foldedAhead(journal, leg), *leg.InstrumentID, *leg.Quantity)
		if err != nil {
			return nil, nil //nolint:nilerr // see StatedBasisChanges: such a pair is left out
		}
		change := leg.AmountMinor - held
		return &change, nil
	}
	return nil, nil
}
