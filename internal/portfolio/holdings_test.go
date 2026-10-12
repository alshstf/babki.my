package portfolio_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// What a rebalance's estimate reads (Р-37 В): what each paper sells for now —
// nothing for a frozen fund or one priced only by a person — and its lots
// with their cost in roubles: a dollar lot at its day's rate, one bought
// before any rate is known not struck.
func TestPapersForSale(t *testing.T) {
	f := setupTwoValuations(t)
	usd := uuid.MustParse(createInstrument(t, f.c, f.url, `{"type":"share","name":"Доллары","ticker":"USDS","currency":"USD"}`).ID)
	for _, buy := range []struct{ on, amount string }{{"2021-01-10", "-1000"}, {"2023-01-10", "-2000"}} {
		createOperation(t, f.c, f.url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
			"occurred_on":%q,"quantity":"1","price":"10","amount_minor":%s,"currency":"USD"}`, f.account, usd, buy.on, buy.amount))
	}
	got, err := f.h.ForSale(t.Context(), f.space, uuid.MustParse(f.account))
	if err != nil {
		t.Fatal(err)
	}
	by := map[uuid.UUID]int{}
	for i, h := range got {
		by[h.InstrumentID] = i
	}
	fxit := got[by[f.papers["fxit"]]]
	if fxit.LiquidInBase == nil || *fxit.LiquidInBase != 65000 || len(fxit.Lots) != 1 || !fxit.Lots[0].CostKnown ||
		fxit.Lots[0].CostInBase != 100000 || fxit.Lots[0].AcquiredOn.Format("2006-01-02") != "2021-06-01" {
		t.Errorf("fxit = %+v", fxit)
	}
	for _, name := range []string{"tech", "house"} {
		if h := got[by[f.papers[name]]]; h.LiquidInBase != nil {
			t.Errorf("%s sells now for %d, want nothing", name, *h.LiquidInBase)
		}
	}
	dollars := got[by[usd]]
	if len(dollars.Lots) != 2 || dollars.Lots[0].CostKnown || !dollars.Lots[1].CostKnown || dollars.Lots[1].CostInBase != 180000 {
		t.Errorf("dollar lots = %+v", dollars.Lots)
	}
}
