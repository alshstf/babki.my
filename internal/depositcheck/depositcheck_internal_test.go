package depositcheck

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/deposit"
)

func day(s string) time.Time {
	d, _ := time.Parse(time.DateOnly, s)
	return d
}

// The balance kept in the bank in October: of each of its accounts the
// least of the marks in the month and the last one before it — 400 000 on
// the first account (it fell from 500 000), 200 000 on the second (marked in
// September only); the other bank's and the dollar account aside; none
// before any mark.
func TestTheBalanceKeptInTheBank(t *testing.T) {
	a, b, c, d := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	list := []account.WithBalance{
		{Account: account.Account{ID: a, Currency: "RUB"}},
		{Account: account.Account{ID: b, Currency: "RUB"}},
		{Account: account.Account{ID: c, Currency: "RUB"}},
		{Account: account.Account{ID: d, Currency: "USD"}},
	}
	bankOf := map[uuid.UUID]string{a: "gazprombank", b: "gazprombank", c: "vtb", d: "gazprombank"}
	history := map[uuid.UUID][]account.BalancePoint{
		a: {{AsOf: day("2026-09-20"), AmountMinor: 500_000_00}, {AsOf: day("2026-10-12"), AmountMinor: 400_000_00}, {AsOf: day("2026-11-02"), AmountMinor: 100}},
		b: {{AsOf: day("2026-09-01"), AmountMinor: 200_000_00}},
		c: {{AsOf: day("2026-10-01"), AmountMinor: 900_000_00}},
		d: {{AsOf: day("2026-10-01"), AmountMinor: 900_000_00}},
	}
	if got, ok := balanceIn(list, history, bankOf, "gazprombank", day("2026-10-01")); !ok || got != 600_000_00 {
		t.Errorf("October = %d %v, want 600 000", got, ok)
	}
	if _, ok := balanceIn(list, history, bankOf, "gazprombank", day("2026-08-01")); ok {
		t.Error("August told without a mark")
	}
	if _, ok := measureFlows(nil, "vtb", deposit.CheckCardSpend, bankOf, nil, nil); ok {
		t.Error("purchases told without a single operation")
	}
}
