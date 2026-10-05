package operation_test

import (
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/operation"
)

// seedSplit records a split as the registry does, through ApplyImportDelta
// with the registry's source, so split-arithmetic tests go through the same
// checks the registry's writes do. A fresh external id per call keeps two
// splits from colliding.
func seedSplit(t *testing.T, f fixture, svc *operation.Service, op operation.Operation) operation.Operation {
	t.Helper()
	op.Source = operation.SourceRegistry
	if op.ExternalID == nil {
		id := uuid.NewString()
		op.ExternalID = &id
	}
	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{op},
	})
	if err != nil {
		t.Fatalf("seed split on %s: %v", op.OccurredOn.Format("2006-01-02"), err)
	}
	if len(refused) > 0 {
		t.Fatalf("seed split on %s refused: %v", op.OccurredOn.Format("2006-01-02"), refused[0].Err)
	}
	if len(applied) != 1 {
		t.Fatalf("seed split on %s: applied %d rows, want 1", op.OccurredOn.Format("2006-01-02"), len(applied))
	}
	return applied[0]
}

// trySplit is seedSplit for a write expected to be refused: it returns the
// error whether the delta failed whole or refused its one candidate.
func trySplit(t *testing.T, f fixture, svc *operation.Service, op operation.Operation) error {
	t.Helper()
	op.Source = operation.SourceRegistry
	if op.ExternalID == nil {
		id := uuid.NewString()
		op.ExternalID = &id
	}
	_, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{op},
	})
	if err != nil {
		return err
	}
	if len(refused) > 0 {
		return refused[0].Err
	}
	return nil
}

// splitOf is the operation a registry event would produce for this account.
func splitOf(f fixture, instrumentID uuid.UUID, on string, ratio string) operation.Operation {
	return operation.Operation{
		AccountID:    f.accountID,
		InstrumentID: &instrumentID,
		Type:         operation.TypeSplit,
		OccurredOn:   date(on),
		SplitRatio:   dec(ratio),
		AmountMinor:  0,
		Currency:     "RUB",
	}
}
