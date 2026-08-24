package operation_test

import (
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/operation"
)

// seedSplit records a split the way the only writer of splits does: through the
// importer's door, carrying the corporate-actions registry's source.
//
// IT EXISTS BECAUSE A SPLIT IS NO LONGER SOMETHING A PERSON ENTERS. The rule
// used to be the opposite — source=manual only — and the tests that exercise
// split ARITHMETIC (the scale a ratio is stored at, a sell after a reverse
// split, a transfer of what a split left) went through Service.Create because
// that was the door. The arithmetic they check is unchanged; the door moved,
// because a split happens to the PAPER and belongs in the registry once rather
// than in each account by hand (see internal/corporateaction).
//
// It writes through ApplyImportDelta rather than reaching into the store, so
// those tests keep going through the SAME checks the registry's own writes go
// through: the engine replays the journal the delta leaves, and the stored rows
// are replayed once more before the commit.
//
// The external id is the caller's line number's worth of uniqueness — a fresh
// UUID — because the journal holds one row per (account, source, external id)
// and a test seeding two splits must not collide with itself.
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

// trySplit is seedSplit for a test that expects the write to be REFUSED: it
// hands back the error the journal gave, whether that came as a failure of the
// whole delta or as a refusal of the one candidate in it.
//
// The two are different answers in the import path — a delta that contradicts
// its own contract is fatal, an operation the journal cannot hold comes back in
// refused — and a test about a rejected split should not have to know which
// shape its own rejection takes to see that it happened.
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
