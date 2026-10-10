package recurring

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/operation"
)

func d(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

// Rent, a phone bill that moves a little, a yearly subscription and a salary
// are regular; groceries at one shop on uneven days are not; a subscription
// that stopped is not; a shop's number does not split a payee.
func TestRegularPaymentsAreFoundByPaceAndAmount(t *testing.T) {
	card := uuid.New()
	var ops []operation.Operation
	add := func(on, who string, amount int64) {
		ops = append(ops, operation.Operation{AccountID: card, OccurredOn: d(on), Counterparty: who, AmountMinor: amount, Currency: "RUB"})
	}
	for _, m := range []string{"2026-06-06", "2026-07-06", "2026-08-06", "2026-09-06"} {
		add(m, "ИП Смирнова", -45_000_00)
	}
	for i, m := range []string{"2026-07-12", "2026-08-12", "2026-09-12"} {
		add(m, "МТС", -int64(890_00+i*30_00))
	}
	add("2024-11-03", "Яндекс Плюс годовая 1234", -2_990_00)
	add("2025-11-03", "Яндекс Плюс годовая 5678", -2_990_00)
	for _, m := range []string{"2026-07-05", "2026-08-05", "2026-09-05"} {
		add(m, "ООО «Ромашка»", 180_000_00)
	}
	for _, m := range []string{"2026-09-01", "2026-09-03", "2026-09-17", "2026-09-18", "2026-10-02"} {
		add(m, "Пятёрочка 4411", -1_200_00)
	}
	for _, m := range []string{"2026-01-15", "2026-02-15", "2026-03-15"} {
		add(m, "Кинопоиск", -299_00)
	}

	got := find(ops, d("2026-10-10"))
	names := map[string]Payment{}
	for _, p := range got {
		names[p.Name] = p
	}
	for _, want := range []struct {
		name    string
		cadence Cadence
		amount  int64
		next    string
		overdue bool
	}{
		{"ИП Смирнова", Monthly, -45_000_00, "2026-10-06", true},
		{"МТС", Monthly, -920_00, "2026-10-12", false},
		{"Яндекс Плюс годовая", Yearly, -2_990_00, "2026-11-03", false},
		{"ООО «Ромашка»", Monthly, 180_000_00, "2026-10-05", true},
	} {
		p, ok := names[want.name]
		if !ok {
			t.Errorf("%s was not found among %v", want.name, got)
			continue
		}
		if p.Cadence != want.cadence || p.Amount != want.amount || p.Next.Format(time.DateOnly) != want.next || p.Overdue != want.overdue {
			t.Errorf("%s = %+v, want %s %d next %s overdue %v", want.name, p, want.cadence, want.amount, want.next, want.overdue)
		}
	}
	for _, not := range []string{"Пятёрочка", "Кинопоиск"} {
		if _, ok := names[not]; ok {
			t.Errorf("%s was taken for a regular payment", not)
		}
	}
	if len(got) != 4 {
		t.Errorf("found %d, want 4", len(got))
	}
	if got[0].Name != "ООО «Ромашка»" {
		t.Errorf("the soonest first: %s", got[0].Name)
	}
}

// A salary paid twice a month — the 5th and, as an advance, the 25th, a
// weekend moving either by a day or two — is two monthly payments from the
// same payee (#428); one shop on scattered days stays nothing.
func TestTwiceAMonthIsTwoMonthlyPayments(t *testing.T) {
	card := uuid.New()
	var ops []operation.Operation
	add := func(on, who string, amount int64) {
		ops = append(ops, operation.Operation{AccountID: card, OccurredOn: d(on), Counterparty: who, AmountMinor: amount, Currency: "RUB"})
	}
	for _, on := range []string{"2026-07-03", "2026-08-05", "2026-09-05"} {
		add(on, "ООО «Ромашка»", 180_000_00)
	}
	for _, on := range []string{"2026-07-24", "2026-08-25", "2026-09-25"} {
		add(on, "ООО «Ромашка»", 60_000_00)
	}
	for _, on := range []string{"2026-07-02", "2026-07-21", "2026-08-09", "2026-08-30", "2026-09-14", "2026-09-28"} {
		add(on, "Перекрёсток", -2_500_00)
	}

	got := find(ops, d("2026-10-01"))
	if len(got) != 2 {
		t.Fatalf("found %+v, want the salary and the advance", got)
	}
	for i, want := range []struct {
		amount int64
		next   string
	}{{180_000_00, "2026-10-05"}, {60_000_00, "2026-10-25"}} {
		p := got[i]
		if p.Name != "ООО «Ромашка»" || p.Cadence != Monthly || p.Amount != want.amount || p.Next.Format(time.DateOnly) != want.next {
			t.Errorf("payment %d = %+v, want %d next %s", i, p, want.amount, want.next)
		}
	}
}
