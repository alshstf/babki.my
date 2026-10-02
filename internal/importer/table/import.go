package table

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/db"
)

// Import reads content against the account with mapping and writes the rows
// the journal takes, recording the load in the same transaction. Rows already
// imported are skipped; the rest of the verdicts are the journal's at the
// moment of writing, which may differ from a preview made earlier. A load that
// writes nothing is not recorded: the Import comes back with no id.
func (s *Service) Import(ctx context.Context, spaceID, userID, accountID uuid.UUID, content string, mapping Mapping, fileName string) (Import, Preview, error) {
	p, err := s.Preview(ctx, spaceID, accountID, content, &mapping)
	if err != nil {
		return Import{}, Preview{}, err
	}
	imp := Import{AccountID: accountID, FileName: fileName, Mapping: mapping}
	var offered []operation.Operation
	byID := map[string]int{}
	for i, row := range p.Rows {
		switch row.Verdict {
		case VerdictDuplicate:
			imp.Duplicate++
		case VerdictUnparsed:
			imp.Unparsed++
		default:
			byID[*row.Operation.ExternalID] = i
			offered = append(offered, *row.Operation)
		}
	}
	if len(offered) == 0 {
		return imp, p, nil
	}
	applied, refused, err := s.ops.ApplyImportDeltaWith(ctx, spaceID, operation.ImportDelta{Add: offered},
		func(ctx context.Context, q db.Executor, applied []operation.Operation) error {
			if len(applied) == 0 {
				return nil
			}
			imp.Written = len(applied)
			imp.Refused = len(offered) - len(applied)
			id, err := record(ctx, q, spaceID, userID, imp, applied)
			imp.ID = id
			return err
		})
	if err != nil {
		return Import{}, Preview{}, err
	}
	for _, i := range byID {
		p.Rows[i].Verdict, p.Rows[i].Reason = VerdictNew, ""
	}
	for _, ref := range refused {
		if i, ok := byID[ref.ExternalID]; ok {
			p.Rows[i].Verdict, p.Rows[i].Reason = VerdictRefused, ref.Err.Error()
		}
	}
	imp.Written, imp.Refused = len(applied), len(refused)
	if imp.ID != uuid.Nil {
		if imp, err = s.imports.ByID(ctx, spaceID, imp.ID); err != nil {
			return Import{}, Preview{}, err
		}
	}
	return imp, p, nil
}

// Imports lists an account's loads, newest first.
func (s *Service) Imports(ctx context.Context, spaceID, accountID uuid.UUID) ([]Import, error) {
	if _, err := s.accounts.ByID(ctx, spaceID, accountID); err != nil {
		return nil, err
	}
	return s.imports.List(ctx, spaceID, accountID)
}

// RollBack takes a load's operations out of the journal, if the journal
// replays without them, and marks it rolled back.
func (s *Service) RollBack(ctx context.Context, spaceID, id uuid.UUID) (Import, error) {
	imp, err := s.imports.ByID(ctx, spaceID, id)
	if err != nil {
		return Import{}, err
	}
	if imp.RolledBackAt != nil {
		return Import{}, fmt.Errorf("%w: this import is already rolled back", family.ErrValidation)
	}
	ids, err := s.imports.operations(ctx, id)
	if err != nil {
		return Import{}, err
	}
	mark := func(ctx context.Context, q db.Executor, _ []operation.Operation) error {
		return markRolledBack(ctx, q, id)
	}
	if len(ids) == 0 {
		if err := mark(ctx, s.imports.db, nil); err != nil {
			return Import{}, err
		}
	} else if _, _, err := s.ops.ApplyImportDeltaWith(ctx, spaceID, operation.ImportDelta{Remove: ids}, mark); err != nil {
		return Import{}, err
	}
	return s.imports.ByID(ctx, spaceID, id)
}
