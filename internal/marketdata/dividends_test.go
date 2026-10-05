package marketdata_test

import (
	"testing"
	"time"

	"babki.my/babki/internal/marketdata"
)

// A source's calendar for a paper is replaced whole: a dividend it no longer
// lists is gone, another source's rows and another paper's are untouched, and
// a paper's dividends come back oldest record date first.
func TestReplaceDividendsReplacesOneSourcesCalendarOfOnePaper(t *testing.T) {
	f := newFixture(t)
	paper, other := f.insts[0], f.insts[1]
	pay := date("2023-09-28")
	div := func(record, per, source string) marketdata.Dividend {
		return marketdata.Dividend{
			InstrumentID: paper, Source: source, RecordDate: date(record),
			PerShare: dec(per), Currency: "USD",
		}
	}
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)

	first := []marketdata.Dividend{div("2023-09-07", "0.04", "tinvest"), div("2023-06-08", "0.04", "tinvest")}
	first[0].PaymentDate = &pay
	if err := f.store.ReplaceDividends(f.ctx, paper, "tinvest", first, now); err != nil {
		t.Fatalf("ReplaceDividends: %v", err)
	}
	if err := f.store.ReplaceDividends(f.ctx, paper, "yahoo", []marketdata.Dividend{div("2023-09-07", "0.04", "yahoo")}, now); err != nil {
		t.Fatalf("ReplaceDividends yahoo: %v", err)
	}
	otherDiv := div("2023-01-01", "1", "tinvest")
	otherDiv.InstrumentID = other
	if err := f.store.ReplaceDividends(f.ctx, other, "tinvest", []marketdata.Dividend{otherDiv}, now); err != nil {
		t.Fatalf("ReplaceDividends other: %v", err)
	}

	// The source withdraws June's.
	if err := f.store.ReplaceDividends(f.ctx, paper, "tinvest", first[:1], now); err != nil {
		t.Fatalf("ReplaceDividends again: %v", err)
	}

	got, err := f.store.DividendsOf(f.ctx, f.insts)
	if err != nil {
		t.Fatalf("DividendsOf: %v", err)
	}
	if len(got[paper]) != 2 || got[paper][0].Source != "tinvest" || got[paper][1].Source != "yahoo" {
		t.Fatalf("paper's dividends = %+v, want September's from each source", got[paper])
	}
	if p := got[paper][0].PaymentDate; p == nil || !p.Equal(pay) || !got[paper][0].PerShare.Equal(dec("0.04")) {
		t.Errorf("September's = %+v, want 0.04 paid on 2023-09-28", got[paper][0])
	}
	if len(got[other]) != 1 {
		t.Errorf("the other paper's dividends = %+v, want its one, untouched", got[other])
	}
}

// A row handed in under another paper or source than the calendar being
// replaced is refused, before anything is written.
func TestReplaceDividendsRefusesARowOfAnotherCalendar(t *testing.T) {
	f := newFixture(t)
	if err := f.store.ReplaceDividends(f.ctx, f.insts[0], "tinvest", []marketdata.Dividend{{
		InstrumentID: f.insts[1], Source: "tinvest", RecordDate: date("2023-01-01"), PerShare: dec("1"), Currency: "USD",
	}}, time.Now()); err == nil {
		t.Fatal("a row of another paper was accepted")
	}
}
