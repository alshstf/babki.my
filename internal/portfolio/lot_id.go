package portfolio

import "fmt"

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
