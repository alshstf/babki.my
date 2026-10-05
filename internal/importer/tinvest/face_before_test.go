package tinvest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

// fakeFaces answers the outstanding face of a bond by its ISIN, as the
// exchange's schedule would.
type fakeFaces struct {
	byISIN map[string]decimal.Decimal
	err    error
}

func (f fakeFaces) FaceBeforeByISIN(_ context.Context, isin string, _ time.Time) (decimal.Decimal, string, bool, error) {
	if f.err != nil {
		return decimal.Zero, "", false, f.err
	}
	face, ok := f.byISIN[isin]
	return face, "RUB", ok, nil
}

// Decision Р-4 through the import: eight bonds bought at par for 8 000 ₽, then
// 200 ₽ of their principal repaid while 1 000 ₽ a bond is outstanding. The
// repayment carries that face, retires 2,5 % of the basis — 200 ₽ — and leaves
// no result, as at par it should; the position keeps 7 800 ₽ of cost.
func TestRebuildMeasuresARepaymentAgainstTheExchangesSchedule(t *testing.T) {
	f := newRebuildFixture(t)
	f.reb.WithFaceSchedule(fakeFaces{byISIN: map[string]decimal.Decimal{"RU000A1075J3": decimal.RequireFromString("1000")}})
	repaid := loadOperationItem(t, "bond_repayment.json")
	f.sync(t, f.link, loadOperationItem(t, "bond_buy.json"), repaid)
	f.rebuild(t)

	entry := byExternalID(t, f.journalOf(t, f.accountID), externalIDFor(f.mirrorRow(t, f.link, repaid.ID), 1))
	if entry.Type != operation.TypeAmortization || entry.FaceBeforeMinor == nil || *entry.FaceBeforeMinor != 100_000 {
		t.Fatalf("the repayment is %s with face %v, want amortization at 100000 a bond", entry.Type, entry.FaceBeforeMinor)
	}
	positions, err := portfolio.Compute(mustListForEngine(t, f, f.accountID))
	if err != nil {
		t.Fatalf("the journal does not replay: %v", err)
	}
	if p := positions[*entry.InstrumentID]; p == nil || p.CostMinor != 780_000 {
		t.Errorf("the bond's cost is %v, want 780000 — 2,5 %% of 800000 retired", p)
	}
}

// A schedule the exchange cannot be asked for stops the rebuild, rather than
// measuring the repayment by the old rule on this run and the new one on the
// next.
func TestRebuildStopsWhenTheScheduleCannotBeRead(t *testing.T) {
	f := newRebuildFixture(t)
	f.reb.WithFaceSchedule(fakeFaces{err: errors.New("exchange unreachable")})
	f.sync(t, f.link, loadOperationItem(t, "bond_buy.json"), loadOperationItem(t, "bond_repayment.json"))
	if _, err := f.reb.Rebuild(f.ctx, f.conn, []AccountLink{f.link}, f.src); err == nil {
		t.Error("the rebuild went ahead without the schedule")
	}
}
