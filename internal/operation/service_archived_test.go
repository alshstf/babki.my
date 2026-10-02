package operation_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
)

// An archived account is out of every total and offers no balance on the
// screen; a hand entry into it would change a history nobody is looking at.
// Every hand door is asked here, each one about a row it could otherwise
// write, so that a door switched back to the plain lock is named by the case
// that goes through. The importer is asked too, and must still write: what a
// broker reports about an account is a fact whatever the family did with it.
func TestHandEntriesRefuseAnArchivedAccount(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	deposit, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit, OccurredOn: date("2026-07-01"),
		AmountMinor: 1_000_000, Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if _, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-02"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}); err != nil {
		t.Fatalf("buy: %v", err)
	}
	arrival, err := svc.CreateArrival(f.ctx, f.spaceID, operation.ArrivalParams{
		AccountID: f.accountID, InstrumentID: f.sberID, OccurredOn: date("2026-07-03"),
		Quantity: decimal.NewFromInt(5), Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("arrival: %v", err)
	}
	if err := f.accStore.Archive(f.ctx, f.spaceID, f.accountID); err != nil {
		t.Fatalf("archive: %v", err)
	}

	price := decimal.NewFromInt(90)
	doors := map[string]func() error{
		"create": func() error {
			_, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
				AccountID: f.accountID, Type: operation.TypeDeposit, OccurredOn: date("2026-07-04"),
				AmountMinor: 1_000, Currency: "RUB",
			})
			return err
		},
		"update": func() error {
			edited := deposit
			edited.AmountMinor = 2_000_000
			_, err := svc.Update(f.ctx, f.spaceID, deposit.ID, edited)
			return err
		},
		"delete": func() error { return svc.Delete(f.ctx, f.spaceID, deposit.ID) },
		"transfer out of it": func() error {
			_, _, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
				FromAccountID: f.accountID, ToAccountID: f.account2ID, InstrumentID: f.sberID,
				Quantity: decimal.NewFromInt(1), OccurredOn: date("2026-07-04"),
			})
			return err
		},
		"transfer into it": func() error {
			_, _, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
				FromAccountID: f.account2ID, ToAccountID: f.accountID, InstrumentID: f.sberID,
				Quantity: decimal.NewFromInt(1), OccurredOn: date("2026-07-04"),
			})
			return err
		},
		"arrival": func() error {
			_, err := svc.CreateArrival(f.ctx, f.spaceID, operation.ArrivalParams{
				AccountID: f.accountID, InstrumentID: f.sberID, OccurredOn: date("2026-07-04"),
				Quantity: decimal.NewFromInt(1), Currency: "RUB",
			})
			return err
		},
		"stated purchases": func() error {
			_, err := svc.StatePurchases(f.ctx, f.spaceID, arrival.ID, []operation.StatedPurchase{
				{Quantity: decimal.NewFromInt(5), Price: &price},
			})
			return err
		},
	}
	for name, write := range doors {
		err := write()
		if !errors.Is(err, operation.ErrAccountArchived) || !errors.Is(err, family.ErrValidation) {
			t.Errorf("%s into an archived account: err = %v, want ErrAccountArchived (a 400)", name, err)
		}
	}

	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{importedDeposit(f, "late-report", "2026-07-05", 3_000)},
	})
	if err != nil || len(applied) != 1 || len(refused) != 0 {
		t.Errorf("an import into an archived account: applied %d, refused %+v, err %v — want it written",
			len(applied), refused, err)
	}

	active := account.StatusActive
	if _, err := f.accStore.Update(f.ctx, f.spaceID, f.accountID, account.Update{Status: &active}); err != nil {
		t.Fatalf("bring back: %v", err)
	}
	if err := doors["create"](); err != nil {
		t.Errorf("a hand entry once the account is back from the archive: %v", err)
	}
}

// The note ceiling counts characters, not bytes: a thousand Cyrillic letters
// are two thousand bytes and still a note of a thousand characters. It holds
// at every hand door that takes a note, and not at the importer's, whose notes
// are the broker's own wording.
func TestHandEnteredNotesAreBoundedInCharacters(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	full := strings.Repeat("ж", operation.MaxNoteRunes)
	over := full + "ж"
	deposit := func(note string) operation.Operation {
		return operation.Operation{
			AccountID: f.accountID, Type: operation.TypeDeposit, OccurredOn: date("2026-07-01"),
			AmountMinor: 1_000_000, Currency: "RUB", Note: note,
		}
	}
	created, err := svc.Create(f.ctx, f.spaceID, deposit(full))
	if err != nil {
		t.Fatalf("a note of exactly %d characters: %v", operation.MaxNoteRunes, err)
	}
	if _, err := svc.Create(f.ctx, f.spaceID, deposit(over)); !errors.Is(err, family.ErrValidation) {
		t.Errorf("create with a note one character too long: err = %v, want a validation refusal", err)
	}
	edited := created
	edited.Note = over
	if _, err := svc.Update(f.ctx, f.spaceID, created.ID, edited); !errors.Is(err, family.ErrValidation) {
		t.Errorf("update with a note one character too long: err = %v, want a validation refusal", err)
	}
	if _, err := svc.CreateArrival(f.ctx, f.spaceID, operation.ArrivalParams{
		AccountID: f.accountID, InstrumentID: f.sberID, OccurredOn: date("2026-07-02"),
		Quantity: decimal.NewFromInt(10), Currency: "RUB", Note: over,
	}); !errors.Is(err, family.ErrValidation) {
		t.Errorf("arrival with a note one character too long: err = %v, want a validation refusal", err)
	}
	if _, _, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID, InstrumentID: f.sberID,
		Quantity: decimal.NewFromInt(1), OccurredOn: date("2026-07-03"), Note: over,
	}); err == nil || !strings.Contains(err.Error(), "note must be at most") {
		t.Errorf("transfer with a note one character too long: err = %v, want the note refused", err)
	}

	reported := importedDeposit(f, "long-note", "2026-07-04", 5_000)
	reported.Note = over
	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{Add: []operation.Operation{reported}})
	if err != nil || len(applied) != 1 || len(refused) != 0 {
		t.Errorf("an imported row with a long note: applied %d, refused %+v, err %v — want it written", len(applied), refused, err)
	}
}
