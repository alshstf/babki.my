package table

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
)

// Verdict is what importing a row would do.
type Verdict string

const (
	VerdictNew       Verdict = "new"
	VerdictDuplicate Verdict = "duplicate"
	VerdictUnparsed  Verdict = "unparsed"
	VerdictRefused   Verdict = "refused"
)

// Row is one row of the table with what importing it would do.
type Row struct {
	Line      Line
	Verdict   Verdict
	Reason    *Unreadable // why the row is unparsed or refused; nil otherwise
	Operation *operation.Operation
}

// Preview is a table read against an account.
type Preview struct {
	Mapping Mapping
	Header  []string
	Rows    []Row
	// Tracker is the program whose export the file was recognized as.
	Tracker Tracker
}

type accounts interface {
	ByID(ctx context.Context, spaceID, id uuid.UUID) (account.WithBalance, error)
}

type journal interface {
	ListForEngine(ctx context.Context, spaceID, accountID uuid.UUID) ([]operation.Operation, error)
}

// categorizer is the family's categories and filing rules, for a statement's
// category column and the rows it leaves unfiled.
type categorizer interface {
	List(ctx context.Context, spaceID uuid.UUID) ([]category.Category, error)
	Rules(ctx context.Context, spaceID uuid.UUID) ([]category.Rule, error)
}

type writer interface {
	CheckImportDelta(ctx context.Context, spaceID uuid.UUID, d operation.ImportDelta) ([]operation.Operation, []operation.ImportRefusal, error)
	ApplyImportDeltaWith(ctx context.Context, spaceID uuid.UUID, d operation.ImportDelta, after operation.AfterImport) ([]operation.Operation, []operation.ImportRefusal, error)
}

// Service reads tables into an account's journal.
type Service struct {
	accounts accounts
	papers   catalog
	catalog  catalogWriter
	exchange exchange
	journal  journal
	ops      writer
	imports  *Store
	cats     categorizer
}

// Catalog is the instrument catalog as the service reads and writes it.
type Catalog interface {
	catalog
	catalogWriter
}

func NewService(accounts accounts, papers Catalog, journal journal, ops writer, imports *Store, exchange exchange, cats categorizer) *Service {
	return &Service{
		accounts: accounts, papers: papers, catalog: papers, exchange: exchange,
		journal: journal, ops: ops, imports: imports, cats: cats,
	}
}

// Preview reads content (see Read) against the account with mapping — or
// with one guessed from the table when mapping is nil — and says what
// importing each row would do. Nothing is written.
func (s *Service) Preview(ctx context.Context, spaceID, accountID uuid.UUID, content string, format Format, mapping *Mapping) (Preview, error) {
	acc, err := s.accounts.ByID(ctx, spaceID, accountID)
	if err != nil {
		return Preview{}, err
	}
	t, err := Read(content, format)
	if err != nil {
		return Preview{}, fmt.Errorf("%w: %v", family.ErrValidation, err)
	}
	m := Guess(t)
	if mapping != nil {
		m = *mapping
	}
	for cell, typ := range m.Types {
		if !typeAllowed(typ) {
			return Preview{}, fmt.Errorf("%w: %q is mapped to %s, which a table cannot hold",
				family.ErrValidation, cell, typ)
		}
	}

	out := Preview{Mapping: m, Tracker: t.Tracker}
	if m.HasHeader && len(t.Rows) > 0 {
		out.Header = t.Rows[0].Cells
	}
	cats, err := s.cats.List(ctx, spaceID)
	if err != nil {
		return Preview{}, err
	}
	file, err := s.filer(ctx, spaceID, acc.Account, cats)
	if err != nil {
		return Preview{}, err
	}
	r := &reader{
		accountID: acc.ID, currency: acc.Currency, mapping: m, papers: s.papers,
		known: map[string]*instrument.Instrument{}, categories: cats,
	}
	seen := map[string]int{}
	var fresh []operation.Operation
	byID := map[string]int{}
	for _, line := range dataRows(t, m) {
		row := Row{Line: line}
		op, err := r.read(ctx, line)
		var bad *Unreadable
		switch {
		case errors.As(err, &bad):
			row.Verdict, row.Reason = VerdictUnparsed, bad
		case err != nil:
			return Preview{}, err
		default:
			file(&op)
			id := fingerprint(op, seen)
			op.ExternalID = &id
			row.Operation = &op
			row.Verdict = VerdictNew
			byID[id] = len(out.Rows)
			fresh = append(fresh, op)
		}
		out.Rows = append(out.Rows, row)
	}

	stored, err := s.journal.ListForEngine(ctx, spaceID, acc.ID)
	if err != nil {
		return Preview{}, err
	}
	kept := fresh[:0:0]
	for _, o := range stored {
		if o.Source == Source && o.ExternalID != nil {
			if i, dup := byID[*o.ExternalID]; dup {
				out.Rows[i].Verdict = VerdictDuplicate
			}
		}
	}
	for _, op := range fresh {
		if out.Rows[byID[*op.ExternalID]].Verdict == VerdictNew {
			kept = append(kept, op)
		}
	}
	if len(kept) == 0 {
		return out, nil
	}
	_, refused, err := s.ops.CheckImportDelta(ctx, spaceID, operation.ImportDelta{Add: kept})
	if err != nil {
		return Preview{}, err
	}
	for _, ref := range refused {
		if i, ok := byID[ref.ExternalID]; ok {
			out.Rows[i].Verdict, out.Rows[i].Reason = VerdictRefused, refusal(ref)
		}
	}
	return out, nil
}

// filer files a row the table left unfiled by the family's rules — on an
// everyday account only, as operation.Service.FileByRules does: on a broker's
// account an unfiled row is money between the family and its broker.
func (s *Service) filer(ctx context.Context, spaceID uuid.UUID, acc account.Account, cats []category.Category) (func(*operation.Operation), error) {
	if acc.Type == account.TypeBrokerage {
		return func(*operation.Operation) {}, nil
	}
	rules, err := s.cats.Rules(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	byID := make(map[uuid.UUID]category.Category, len(cats))
	for _, c := range cats {
		byID[c.ID] = c
	}
	return func(op *operation.Operation) {
		if op.CategoryID != nil || !operation.Categorizable(*op) {
			return
		}
		op.CategoryID = category.Match(rules, byID, operation.CategoryKindOf(op.Type),
			category.Text{Counterparty: op.Counterparty, Note: op.Note})
	}, nil
}

// refusal is the journal's refusal of a row, its words carried as they are.
func refusal(ref operation.ImportRefusal) *Unreadable {
	return unreadable(ReasonEngineRefused, "", ref.Err.Error())
}
