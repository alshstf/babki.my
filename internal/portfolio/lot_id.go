package portfolio

import (
	"fmt"
	"slices"

	"github.com/shopspring/decimal"
)

// LotID is a lot's permanent number: the operation that brought it into the
// account and the piece of that operation it is (0 for a purchase, i for the
// i-th piece of an arrival's breakdown). A partial sale leaves it, a split
// keeps it. A transfer, a conversion and a spin-off name the lots they take by
// it, so two parcels bought on one day at different prices are never confused.
type LotID struct {
	// Origin names the operation: an imported one by its source and record
	// (a correction re-inserts the row under a new id but the same record), a
	// hand entry by its id. Empty when the operation has neither yet.
	Origin string
	Seq    int
}

// IsZero reports a lot or piece with no number.
func (id LotID) IsZero() bool { return id.Origin == "" }

func (id LotID) String() string { return fmt.Sprintf("%s#%d", id.Origin, id.Seq) }

// lotOrigin is what the lots an operation brings are numbered after.
func (o Operation) lotOrigin() string {
	if o.ExternalID != nil && *o.ExternalID != "" {
		return o.Source + "/" + *o.ExternalID
	}
	if o.ID == [16]byte{} {
		return ""
	}
	return "op/" + o.ID.String()
}

// lotID is the number of the seq-th lot the operation brings; zero when the
// operation has no identity.
func (o Operation) lotID(seq int) LotID {
	origin := o.lotOrigin()
	if origin == "" {
		return LotID{}
	}
	return LotID{Origin: origin, Seq: seq}
}

// NumberLegacyPieces names, for a breakdown recorded before lots had numbers,
// the lot each piece came from, the way the day matching replays it: among the
// lots of the piece's day, front to back. ok is false when a piece would take
// from more than one lot or from none, or a lot has no number — the record is
// then left to the day matching.
func NumberLegacyPieces(lots []Lot, pieces []ReleasedLot) ([]ReleasedLot, bool) {
	left := make([]decimal.Decimal, len(lots))
	for i, l := range lots {
		left[i] = l.Quantity
	}
	out := slices.Clone(pieces)
	for k, pc := range pieces {
		at := -1
		for i, l := range lots {
			if !sameAcquisition(l.AcquiredOn, pc.AcquiredOn) {
				continue
			}
			if pc.Quantity.IsZero() {
				if l.Quantity.IsZero() && l.CostMinor > 0 {
					at = i
					break
				}
				continue
			}
			if left[i].IsPositive() {
				at = i
				break
			}
		}
		if at < 0 || lots[at].ID.IsZero() || pc.Quantity.GreaterThan(left[at]) {
			return nil, false
		}
		left[at] = left[at].Sub(pc.Quantity)
		out[k].From = lots[at].ID
	}
	return out, true
}
