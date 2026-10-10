package loan_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/loan"
)

func terms(kind loan.Kind, rate string) loan.Terms {
	return loan.Terms{
		AccountID: uuid.New(), Principal: 1_000_000_00, AnnualRate: decimal.RequireFromString(rate),
		TermMonths: 12, IssuedOn: time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC), Kind: kind,
	}
}

// A million at 12 % for a year: the annuity is 88 848.79 a month, interest
// first; the differentiated payment repays 83 333.33 a month plus the interest
// on what is left; either way the debt ends at nought, the first payment a
// month after the loan.
func TestTheScheduleIsWorkedOutMonthByMonth(t *testing.T) {
	rows, err := loan.Schedule(terms(loan.Annuity, "12"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 12 || rows[0].On.Format(time.DateOnly) != "2026-02-15" || rows[11].Left != 0 {
		t.Fatalf("annuity rows = %d, first %v, last left %d", len(rows), rows[0].On, rows[11].Left)
	}
	if rows[0].Payment != 88_848_79 || rows[0].Interest != 10_000_00 || rows[0].Principal != 78_848_79 {
		t.Errorf("first annuity payment = %+v", rows[0])
	}
	var repaid int64
	for _, r := range rows {
		repaid += r.Principal
		if r.Payment-r.Interest != r.Principal {
			t.Errorf("payment %d ≠ interest %d + principal %d", r.Payment, r.Interest, r.Principal)
		}
	}
	if repaid != 1_000_000_00 {
		t.Errorf("principal repaid = %d, want the whole loan", repaid)
	}
	if last := rows[11].Payment; last < 88_848_00 || last > 88_849_50 {
		t.Errorf("the last annuity payment = %d, want about the same as the others", last)
	}

	diff, err := loan.Schedule(terms(loan.Differentiated, "12"))
	if err != nil {
		t.Fatal(err)
	}
	if diff[0].Principal != 83_333_33 || diff[0].Interest != 10_000_00 || diff[11].Left != 0 || diff[11].Interest >= diff[0].Interest {
		t.Errorf("differentiated = first %+v, last %+v", diff[0], diff[11])
	}

	free, err := loan.Schedule(terms(loan.Annuity, "0"))
	if err != nil || free[0].Interest != 0 || free[0].Payment != 83_333_33 || free[11].Left != 0 {
		t.Errorf("interest-free = %+v, %v", free[0], err)
	}
	bad := terms(loan.Annuity, "12")
	bad.TermMonths = 0
	if _, err := loan.Schedule(bad); err == nil {
		t.Error("a zero-month loan was accepted")
	}
}

// A million at 12% for a year, 300 000 paid ahead after the third payment:
// lowering the payment keeps the twelve months with a smaller one; shortening
// the term keeps the payment and ends sooner. Either way the debt ends at
// nought and every rouble borrowed is repaid once.
func TestAPrepaymentReshapesTheSchedule(t *testing.T) {
	base := terms(loan.Annuity, "12")
	plain, err := loan.Schedule(base)
	if err != nil {
		t.Fatal(err)
	}
	check := func(name string, rows []loan.Row) {
		t.Helper()
		var repaid int64
		for _, r := range rows {
			repaid += r.Principal
		}
		if repaid != base.Principal || rows[len(rows)-1].Left != 0 {
			t.Errorf("%s: repaid %d of %d, left %d", name, repaid, base.Principal, rows[len(rows)-1].Left)
		}
	}
	for _, mode := range []loan.Mode{loan.Lower, loan.Shorter} {
		withPrepayment := base
		withPrepayment.Prepayments = []loan.Prepayment{{On: time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC), Amount: 30_000_000, Mode: mode}}
		rows, err := loan.Schedule(withPrepayment)
		if err != nil {
			t.Fatal(err)
		}
		check(string(mode), rows)
		if !rows[3].Prepaid || rows[3].Principal != 30_000_000 || rows[3].Interest != 0 || rows[3].Left != plain[2].Left-30_000_000 {
			t.Fatalf("%s: the prepayment row = %+v", mode, rows[3])
		}
		regular := rows[4:]
		switch mode {
		case loan.Lower:
			if len(regular) != 9 || regular[0].Payment >= plain[3].Payment || regular[0].Payment != regular[7].Payment {
				t.Errorf("lower: %d months, payment %d then %d (was %d)", len(regular), regular[0].Payment, regular[7].Payment, plain[3].Payment)
			}
		case loan.Shorter:
			if len(regular) >= 9 || regular[0].Payment != plain[3].Payment {
				t.Errorf("shorter: %d months, payment %d (was %d)", len(regular), regular[0].Payment, plain[3].Payment)
			}
		}
	}

	// More than the debt ends the loan there; one before the first payment
	// comes off at once.
	early := base
	early.Prepayments = []loan.Prepayment{
		{On: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), Amount: 10_000_000, Mode: loan.Lower},
		{On: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), Amount: 999_999_999, Mode: loan.Shorter},
	}
	rows, err := loan.Schedule(early)
	if err != nil {
		t.Fatal(err)
	}
	check("early and too much", rows)
	if !rows[0].Prepaid || rows[0].Left != 90_000_000 || !rows[len(rows)-1].Prepaid || len(rows) != 6 {
		t.Errorf("rows = %+v", rows)
	}
}
