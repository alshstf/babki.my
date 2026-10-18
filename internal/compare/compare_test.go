package compare

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/deposit"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/structure"
	"babki.my/babki/internal/taxyear"
)

type fakeKeys struct{}

func (fakeKeys) KeyRateOn(context.Context, time.Time) (decimal.Decimal, bool, error) {
	return decimal.NewFromInt(14), true, nil
}

type fakeAccounts []account.WithBalance

func (f fakeAccounts) ListWithBalance(context.Context, uuid.UUID) ([]account.WithBalance, error) {
	return f, nil
}

type fakeStructures structure.Structure

func (f fakeStructures) Of(context.Context, uuid.UUID, structure.Valuation) (structure.Structure, error) {
	return structure.Structure(f), nil
}

type fakeTaxes taxyear.Year

func (f fakeTaxes) Of(context.Context, uuid.UUID, int) (taxyear.Year, error) {
	return taxyear.Year(f), nil
}

type fakeSpaces struct{}

func (fakeSpaces) SpaceByID(context.Context, uuid.UUID) (family.Space, error) {
	return family.Space{TaxResidency: "RU"}, nil
}

type fakeMembers []family.Member

func (f fakeMembers) ListMembers(context.Context, uuid.UUID) ([]family.Member, error) { return f, nil }

// The comparison over the real catalog: 1 000 000 ₽ for a year from 19
// October 2026, today's key 14 %, Мария already holding 1 000 000 ₽ in Сбер.
func service(t *testing.T) (*Service, uuid.UUID) {
	t.Helper()
	maria := uuid.New()
	rate := decimal.NewFromInt(16)
	sber := "sber"
	svc := NewService(fakeKeys{}, nil,
		fakeAccounts{{Account: account.Account{ID: uuid.New(), Name: "Карта", Type: account.TypeChecking, Status: account.StatusActive, Intermediary: &sber}}},
		fakeStructures{Insurance: []structure.InsuranceRow{{Person: maria, Bank: "sber", Held: 1_000_000_00, Known: true}}, InsuredLimit: 1_400_000_00},
		fakeTaxes{Year: 2026, Currency: "RUB", KeyRate: &rate, People: []taxyear.Person{{UserID: maria, Name: "Мария"}}},
		fakeSpaces{}, fakeMembers{{User: family.User{ID: maria, DisplayName: "Мария"}}})
	svc.now = func() time.Time { return day("2026-10-19") }
	return svc, maria
}

func TestTheComparison(t *testing.T) {
	svc, maria := service(t)
	ask := Ask{Amount: 1_000_000_00, Months: 12, Person: maria, Preset: StateNow}
	started := time.Now()
	r, err := svc.Compare(t.Context(), uuid.New(), ask)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d rows, %d left out, in %s", len(r.Rows), r.Skipped, time.Since(started))
	if len(r.Rows) < 30 || r.Person != "Мария" || !r.KeyPercent.Equal(decimal.NewFromInt(14)) {
		t.Fatalf("rows %d, person %q, key %s", len(r.Rows), r.Person, r.KeyPercent)
	}
	var vtb *Row
	for i, row := range r.Rows {
		if !row.Variant.OpenToAll() {
			t.Errorf("%s %s: open to %v only", row.Programme.ID, row.Variant.ID, row.Variant.Audience)
		}
		if i > 0 && r.Rows[i-1].Net < row.Net {
			t.Errorf("not by what is left: %s before %s", r.Rows[i-1].Programme.ID, row.Programme.ID)
		}
		if row.Tax == nil || row.Insured == nil {
			t.Fatalf("%s: no tax or insurance", row.Programme.ID)
		}
		// Сбер: 400 000 of the limit is left; elsewhere the whole sum.
		want := int64(1_000_000_00)
		if row.Programme.Bank == "sber" {
			want = 400_000_00
		}
		if *row.Insured != want {
			t.Errorf("%s: insured %d, want %d", row.Programme.ID, *row.Insured, want)
		}
		if row.Programme.ID == "vtb-schet" && row.Variant.ID == "minimum" {
			vtb = &r.Rows[i]
		}
	}
	if vtb == nil {
		t.Fatal("no ВТБ-Счёт")
	}
	// No data on ВТБ: no bonus as now, the welcome rate — not a client of
	// ВТБ — counts. Wages switched on for this row alone: more.
	before := vtb.Net
	ask.Choices = map[CondKey]Choice{{"vtb-schet", "minimum", "salary"}: {State: StateYes, Level: -1}}
	r, err = svc.Compare(t.Context(), uuid.New(), ask)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range r.Rows {
		if row.Programme.ID == "vtb-schet" && row.Variant.ID == "minimum" {
			if row.Net <= before {
				t.Errorf("wages to ВТБ: %d, before %d", row.Net, before)
			}
			for _, c := range row.Conditions {
				if c.ID == "salary" && (!c.Met || c.Used.State != StateYes) || c.ID == "new_client" && (!c.Now.Known || !c.Now.Met) {
					t.Errorf("condition %+v", c)
				}
			}
		}
		if row.Programme.ID == "vtb-schet" && row.Variant.ID == "daily" {
			for _, c := range row.Conditions {
				if c.ID == "salary" && c.Met {
					t.Error("the choice spilled over to another variant")
				}
			}
		}
	}
	// A pensioner sees the pensioners' deposits too.
	ask.Audiences = []deposit.Audience{deposit.AudiencePensioner}
	r, err = svc.Compare(t.Context(), uuid.New(), ask)
	if err != nil {
		t.Fatal(err)
	}
	pensioners := 0
	for _, row := range r.Rows {
		if !row.Variant.OpenToAll() {
			pensioners++
		}
	}
	if pensioners == 0 {
		t.Error("no pensioners' deposits for a pensioner")
	}
}

func TestAnAskOutOfBoundsIsRefused(t *testing.T) {
	svc, maria := service(t)
	for _, ask := range []Ask{
		{Amount: 0, Months: 12, Person: maria},
		{Amount: 100_00, Months: 0, Person: maria},
		{Amount: 100_00, Months: 61, Person: maria},
		{Amount: 100_00, Months: 12, Person: maria, Key: Scenario{Custom: true, Percent: decimal.NewFromInt(101)}},
	} {
		if _, err := svc.Compare(t.Context(), uuid.New(), ask); err == nil {
			t.Errorf("%+v: accepted", ask)
		}
	}
}

// Five years: how long the longest comparison takes.
func TestFiveYearsInTime(t *testing.T) {
	svc, maria := service(t)
	started := time.Now()
	if _, err := svc.Compare(t.Context(), uuid.New(), Ask{Amount: 1_000_000_00, Months: MaxMonths, Person: maria}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(started); d > 10*time.Second {
		t.Errorf("five years took %s", d)
	} else {
		t.Logf("five years in %s", d)
	}
}

// The Bank of Russia's forecast: a deposit tied to the key rate earns less
// as the forecast falls (11,5 % in 2027 against 14 % today), the year's path
// told; a fixed one is not moved by it.
func TestTheForecastScenario(t *testing.T) {
	svc, maria := service(t)
	ask := Ask{Amount: 1_000_000_00, Months: 24, Person: maria}
	today, err := svc.Compare(t.Context(), uuid.New(), ask)
	if err != nil {
		t.Fatal(err)
	}
	ask.Key = Scenario{Forecast: true}
	ahead, err := svc.Compare(t.Context(), uuid.New(), ask)
	if err != nil {
		t.Fatal(err)
	}
	if ahead.Forecast == nil || ahead.Forecast.Published != "2026-07-24" || len(ahead.KeyPath) < 3 ||
		!ahead.KeyPath[1].Percent.Equal(decimal.RequireFromString("11.5")) {
		t.Fatalf("path %+v, forecast %+v", ahead.KeyPath, ahead.Forecast)
	}
	income := func(r Result, programme, variant string) int64 {
		for _, row := range r.Rows {
			if row.Programme.ID == programme && row.Variant.ID == variant {
				return row.Earning.Income
			}
		}
		t.Fatalf("no %s %s", programme, variant)
		return 0
	}
	if a, b := income(ahead, "gazprombank-klyuchevoy-moment", "end"), income(today, "gazprombank-klyuchevoy-moment", "end"); a >= b {
		t.Errorf("«Ключевой момент» by the forecast %d, by today's key %d", a, b)
	}
	if a, b := income(ahead, "sber-sbervklad", "end"), income(today, "sber-sbervklad", "end"); a != b {
		t.Errorf("СберВклад by the forecast %d, by today's key %d", a, b)
	}
}
