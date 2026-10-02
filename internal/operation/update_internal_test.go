package operation

import (
	"testing"

	"github.com/google/uuid"
)

// Rows that travel in pairs, or carry a parcel of basis, are not edited in
// place even when they were entered by hand: a request body for one is
// refused before it gets here, and this is the guard behind that refusal.
func TestPairsAndParcelsAreNotEditable(t *testing.T) {
	group := uuid.New()
	for _, old := range []Operation{
		{Source: "manual", Type: TypeTransferIn, TransferGroupID: &group},
		{Source: "manual", Type: TypeTransferIn},
		{Source: "manual", Type: TypeExchangeOut, TransferGroupID: &group},
	} {
		if err := editable(old, old); err == nil {
			t.Errorf("%s (group %v) is editable, want refused", old.Type, old.TransferGroupID)
		}
	}
	if err := editable(Operation{Source: "manual", Type: TypeBuy}, Operation{Type: TypeBuy}); err != nil {
		t.Errorf("a hand-entered buy is refused: %v", err)
	}
}
