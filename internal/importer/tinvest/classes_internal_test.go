package tinvest

import (
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/platform/logtest"
)

// The catalog classified by the broker's passports, as they read on
// 2026-10-13: Сбербанк — Russia, financials; FinEx's American shares fund —
// the United States, though Irish by ISIN; a bond the broker does not know —
// recorded with neither, and not asked again while fresh.
func TestPapersAreClassifiedByTheBrokersPassports(t *testing.T) {
	qf := newQuotesFixture(t)
	cat := instrument.NewStore(qf.pool)
	mk := func(kind instrument.Type, name, isin string) instrument.Instrument {
		i, err := cat.Create(qf.ctx, instrument.Instrument{Type: kind, Name: name, Ticker: name, ISIN: isin, Currency: "RUB"})
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	sber := mk(instrument.TypeShare, "SBER", "RU0009029540")
	fxus := mk(instrument.TypeETF, "FinEx Акции американских компаний", "IE00BD3QHZ91")
	lost := mk(instrument.TypeBond, "LOST", "RU000A0ZZZZ0")

	qf.broker.answer("InstrumentsService/FindInstrument", http.StatusOK, `{"instruments":[
		{"uid":"u-sber","isin":"RU0009029540","ticker":"SBER","instrumentKind":"INSTRUMENT_TYPE_SHARE"},
		{"uid":"u-fxus","isin":"IE00BD3QHZ91","ticker":"FXUS","instrumentKind":"INSTRUMENT_TYPE_ETF"}]}`)
	qf.broker.answer("InstrumentsService/ShareBy", http.StatusOK, `{"instrument":{"countryOfRisk":"RU","sector":"financial"}}`)
	qf.broker.answer("InstrumentsService/EtfBy", http.StatusOK, `{"instrument":{"countryOfRisk":"US","sector":"other","focusType":"equity"}}`)

	log := slog.New(&logtest.Capture{})
	newClient := func(token string) (*Client, error) {
		return NewClient(qf.broker.srv.Client(), qf.broker.srv.URL, token, log), nil
	}
	w := NewClassesWorker(qf.store, cat, qf.sealer, newClient, log, func() time.Time { return qf.now })
	run := func() {
		t.Helper()
		if err := w.Work(qf.ctx, &river.Job[RefreshClassesArgs]{JobRow: &rivertype.JobRow{ID: 1}, Args: RefreshClassesArgs{}}); err != nil {
			t.Fatal(err)
		}
	}
	run()
	got, err := cat.Classes(qf.ctx, []uuid.UUID{sber.ID, fxus.ID, lost.ID})
	if err != nil {
		t.Fatal(err)
	}
	if c := got[sber.ID]; c.Country != "RU" || c.Sector != instrument.SectorFinancials || c.Source != instrument.ClassFromTInvest || c.Raw != "financial" {
		t.Errorf("SBER = %+v", c)
	}
	if c := got[fxus.ID]; c.Country != "US" || c.Sector != instrument.SectorFundsEquity || c.Raw != "other equity" {
		t.Errorf("FXUS = %+v", c)
	}
	if c, ok := got[lost.ID]; !ok || c.Country != "" || c.Sector != instrument.SectorUnknown || !c.FetchedAt.Equal(qf.now) {
		t.Errorf("unknown bond = %+v, %v", c, ok)
	}
	asked := qf.broker.callCount("InstrumentsService/FindInstrument")
	run()
	if again := qf.broker.callCount("InstrumentsService/FindInstrument"); again != asked {
		t.Errorf("fresh classes asked again: %d → %d", asked, again)
	}
}
