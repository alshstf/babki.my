package table

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
)

type oneAccount struct{ acc account.WithBalance }

func (o oneAccount) ByID(context.Context, uuid.UUID, uuid.UUID) (account.WithBalance, error) {
	return o.acc, nil
}

type noPapers struct{}

func (noPapers) ByISIN(context.Context, string) (instrument.Instrument, error) {
	return instrument.Instrument{}, pgx.ErrNoRows
}

func (noPapers) ByTickerTradable(context.Context, string) (instrument.Instrument, error) {
	return instrument.Instrument{}, pgx.ErrNoRows
}

func (noPapers) Create(context.Context, instrument.Instrument) (instrument.Instrument, error) {
	panic("a preview files nothing")
}

// noCategories is a family with no categories and no rules.
type noCategories struct{}

func (noCategories) List(context.Context, uuid.UUID) ([]category.Category, error) {
	return nil, nil
}

func (noCategories) Rules(context.Context, uuid.UUID) ([]category.Rule, error) {
	return nil, nil
}

type storedJournal []operation.Operation

func (s storedJournal) ListForEngine(context.Context, uuid.UUID, uuid.UUID) ([]operation.Operation, error) {
	return s, nil
}

type takesAll struct{ asked []operation.Operation }

func (t *takesAll) CheckImportDelta(_ context.Context, _ uuid.UUID, d operation.ImportDelta) ([]operation.Operation, []operation.ImportRefusal, error) {
	t.asked = d.Add
	return d.Add, nil, nil
}

func (t *takesAll) ApplyImportDeltaWith(context.Context, uuid.UUID, operation.ImportDelta, operation.AfterImport) ([]operation.Operation, []operation.ImportRefusal, error) {
	panic("a preview writes nothing")
}

// A row imported from a table before is recognized by its content when the
// file — or one covering the same days — is loaded again, and is not offered
// to the journal a second time. Of two identical rows, the second is new when
// only one was imported.
func TestARowImportedBeforeIsADuplicate(t *testing.T) {
	acc := account.WithBalance{Account: account.Account{ID: uuid.New(), Currency: "RUB"}}
	csv := "Дата;Операция;Сумма\n01.07.2026;Пополнение;500\n01.07.2026;Пополнение;500\n02.07.2026;Пополнение;700\n"

	first := &takesAll{}
	svc := NewService(oneAccount{acc}, noPapers{}, storedJournal(nil), first, nil, nil, noCategories{})
	p, err := svc.Preview(t.Context(), uuid.New(), acc.ID, csv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.asked) != 3 {
		t.Fatalf("asked the journal about %d rows, want 3", len(first.asked))
	}
	// The first of the two identical deposits was imported, and nothing else.
	imported := first.asked[0]
	imported.Source = Source

	again := &takesAll{}
	svc = NewService(oneAccount{acc}, noPapers{}, storedJournal{imported}, again, nil, nil, noCategories{})
	p, err = svc.Preview(t.Context(), uuid.New(), acc.ID, csv, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := []Verdict{p.Rows[0].Verdict, p.Rows[1].Verdict, p.Rows[2].Verdict}
	if got[0] != VerdictDuplicate || got[1] != VerdictNew || got[2] != VerdictNew {
		t.Errorf("verdicts = %v, want duplicate, new, new", got)
	}
	if len(again.asked) != 2 {
		t.Errorf("asked the journal about %d rows, want the 2 new ones", len(again.asked))
	}

	// A hand entry with the same content is not a table's row.
	byHand := imported
	byHand.Source = "manual"
	svc = NewService(oneAccount{acc}, noPapers{}, storedJournal{byHand}, &takesAll{}, nil, nil, noCategories{})
	if p, _ = svc.Preview(t.Context(), uuid.New(), acc.ID, csv, nil); p.Rows[0].Verdict != VerdictNew {
		t.Errorf("a hand entry made the row a duplicate: %s", p.Rows[0].Verdict)
	}
}
