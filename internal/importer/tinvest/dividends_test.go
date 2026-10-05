package tinvest

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
)

const rpcDividends = "InstrumentsService/GetDividends"

// The calendar's amounts are per share before tax, its days are Moscow days
// whichever midnight the broker stamps them with, a day it leaves out is
// absent, and a dividend declared as nothing is no dividend.
func TestDividendsReadsTheCalendar(t *testing.T) {
	srv, bodies := serve(t, map[string]route{
		rpcPathPrefix + rpcDividends: {status: http.StatusOK, body: []byte(`{"dividends":[
			{"dividendNet":{"currency":"usd","units":"0","nano":40000000},"recordDate":"2021-09-02T00:00:00Z",
			 "paymentDate":"2021-09-30T00:00:00Z","lastBuyDate":"2021-08-31T21:00:00Z","dividendType":"Regular Cash"},
			{"dividendNet":{"currency":"usd","units":"0","nano":0},"recordDate":"2021-06-10T00:00:00Z"},
			{"dividendNet":{"currency":"rub","units":"34","nano":840000000},"recordDate":"2025-07-18T00:00:00Z"}
		]}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "tok", nil)

	got, err := c.Dividends(context.Background(), "BBG000BBJQV0",
		time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Dividends: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d dividends %+v, want the two declared as something", len(got), got)
	}
	nvda := got[0]
	if nvda.PerShare.Currency != "USD" || !nvda.PerShare.Decimal().Equal(decimal.RequireFromString("0.04")) {
		t.Errorf("per share = %s %s, want 0.04 USD", nvda.PerShare.Decimal(), nvda.PerShare.Currency)
	}
	if !nvda.RecordDate.Equal(day(t, "2021-09-02")) || nvda.PaymentDate == nil || !nvda.PaymentDate.Equal(day(t, "2021-09-30")) {
		t.Errorf("record %s, payment %v; want 2021-09-02 and 2021-09-30", nvda.RecordDate, nvda.PaymentDate)
	}
	if nvda.LastBuyDate == nil || !nvda.LastBuyDate.Equal(day(t, "2021-09-01")) {
		t.Errorf("last buy day = %v, want 2021-09-01 — a Moscow midnight stamped in UTC is still that Moscow day", nvda.LastBuyDate)
	}
	if got[1].PaymentDate != nil || got[1].LastBuyDate != nil {
		t.Errorf("the days the broker left out came back as %v and %v, want none", got[1].PaymentDate, got[1].LastBuyDate)
	}

	var req map[string]string
	if err := json.Unmarshal(bodies[rpcPathPrefix+rpcDividends], &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if req["instrumentId"] != "BBG000BBJQV0" || req["from"] != "2021-01-01T00:00:00Z" || req["to"] != "2026-01-01T00:00:00Z" {
		t.Errorf("asked %v, want the paper and the period as given", req)
	}
}

// A dividend with no record date has nothing the estimate could hang on, and
// is refused rather than stored under a day nobody stated.
func TestDividendsRefusesADividendWithNoRecordDate(t *testing.T) {
	srv, _ := serve(t, map[string]route{
		rpcPathPrefix + rpcDividends: {status: http.StatusOK, body: []byte(`{"dividends":[
			{"dividendNet":{"currency":"usd","units":"0","nano":40000000}}]}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "tok", nil)
	if _, err := c.Dividends(context.Background(), "x", time.Time{}, time.Now()); err == nil ||
		!strings.Contains(err.Error(), "no recordDate") {
		t.Fatalf("err = %v, want a refusal naming the missing record date", err)
	}
}

// recordingDividends stands in for marketdata.Store and keeps what it is
// handed, by paper.
type recordingDividends struct {
	stored map[uuid.UUID][]marketdata.Dividend
}

func (r *recordingDividends) ReplaceDividends(_ context.Context, id uuid.UUID, _ string,
	dividends []marketdata.Dividend, _ time.Time,
) error {
	if r.stored == nil {
		r.stored = map[uuid.UUID][]marketdata.Dividend{}
	}
	r.stored[id] = dividends
	return nil
}

// dividendFixture is a space with a connection whose token opens, and a
// worker reading the calendar through the broker stub.
type dividendFixture struct {
	*quotesFixture
	stored *recordingDividends
	worker river.Worker[RefreshDividendsArgs]
}

func newDividendFixture(t *testing.T) *dividendFixture {
	t.Helper()
	qf := newQuotesFixture(t)
	df := &dividendFixture{quotesFixture: qf, stored: &recordingDividends{}}
	log := slog.New(qf.logs)
	newClient := func(token string) (*Client, error) {
		return NewClient(qf.broker.srv.Client(), qf.broker.srv.URL, token, log), nil
	}
	df.worker = NewDividendsWorker(qf.store, df.stored, qf.sealer, newClient, log, func() time.Time { return qf.now })
	return df
}

// paper puts a paper in the catalog and a dividend on it in the journal.
func (f *dividendFixture) paper(t *testing.T, ticker, isin, figi, currency string) instrument.Instrument {
	t.Helper()
	inst, err := instrument.NewStore(f.pool).Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: ticker, Ticker: ticker, ISIN: isin, FIGI: figi, Currency: currency,
	})
	if err != nil {
		t.Fatalf("create %s: %v", ticker, err)
	}
	if _, err := operation.NewService(operation.NewStore(f.pool)).Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, InstrumentID: &inst.ID, Type: operation.TypeDividend,
		OccurredOn: day(t, "2023-09-28"), AmountMinor: 11, Currency: currency,
	}); err != nil {
		t.Fatalf("dividend on %s: %v", ticker, err)
	}
	return inst
}

func (f *dividendFixture) work(t *testing.T) error {
	t.Helper()
	return f.worker.Work(f.ctx, &river.Job[RefreshDividendsArgs]{JobRow: &rivertype.JobRow{ID: 1}})
}

// A foreign paper's calendar is read through the listing the import mapped it
// to and stored whole; a Russian paper's is not read at all — its dividend tax
// is Russian and the broker sends it as a line of its own.
func TestDividendsWorkerStoresTheCalendarOfForeignPapersOnly(t *testing.T) {
	f := newDividendFixture(t)
	nvda := f.paper(t, "NVDA", "US67066G1040", "BBG000BBJQV0", "USD")
	f.paper(t, "SBER", "RU0009029540", "BBG004730N88", "RUB")
	f.mapTo(t, "uid-nvda", nvda.ID, "USD")
	f.broker.answer(rpcDividends, http.StatusOK, `{"dividends":[
		{"dividendNet":{"currency":"usd","units":"0","nano":40000000},"recordDate":"2023-09-07T00:00:00Z","paymentDate":"2023-09-28T00:00:00Z"}]}`)

	if err := f.work(t); err != nil {
		t.Fatalf("Work: %v", err)
	}
	got := f.stored.stored[nvda.ID]
	if len(got) != 1 || got[0].Source != DividendSource || got[0].Currency != "USD" ||
		!got[0].PerShare.Equal(decimal.RequireFromString("0.04")) || !got[0].RecordDate.Equal(day(t, "2023-09-07")) {
		t.Fatalf("stored for NVIDIA %+v, want the one declared dividend from the broker", got)
	}
	if len(f.stored.stored) != 1 {
		t.Errorf("stored calendars of %d papers, want NVIDIA's alone", len(f.stored.stored))
	}
	if n := f.broker.callCount(rpcDividends); n != 1 {
		t.Errorf("asked the calendar %d times, want once — through the mapped listing", n)
	}
}

// A paper no import mapped — bought at a second broker — is asked about by
// its figi, and then by the listings a search of its ISIN finds; the first
// identifier the calendar answers for is the one stored from.
func TestDividendsWorkerFindsAPaperNoImportMapped(t *testing.T) {
	f := newDividendFixture(t)
	ko := f.paper(t, "KO", "US1912161007", "", "USD")
	f.broker.answer("InstrumentsService/FindInstrument", http.StatusOK, `{"instruments":[
		{"uid":"uid-other","isin":"US0000000000","ticker":"X","instrumentKind":"INSTRUMENT_TYPE_SHARE"},
		{"uid":"uid-ko","isin":"US1912161007","ticker":"KO","instrumentKind":"INSTRUMENT_TYPE_SHARE"}]}`)
	f.broker.answer(rpcDividends, http.StatusOK, `{"dividends":[
		{"dividendNet":{"currency":"usd","units":"0","nano":510000000},"recordDate":"2023-09-15T00:00:00Z"}]}`)

	if err := f.work(t); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if got := f.stored.stored[ko.ID]; len(got) != 1 || !got[0].PerShare.Equal(decimal.RequireFromString("0.51")) {
		t.Fatalf("stored for Coca-Cola %+v, want its one dividend", got)
	}
	if n := f.broker.callCount(rpcDividends); n != 1 {
		t.Errorf("asked the calendar %d times, want once — the search's own paper, not the other one", n)
	}
}

// An empty answer for a paper the space HAS received dividends on says the
// identifier was not the paper's: the next one is tried, and with none left
// the stored calendar is not wiped.
func TestDividendsWorkerDoesNotStoreAnEmptyCalendar(t *testing.T) {
	f := newDividendFixture(t)
	nvda := f.paper(t, "NVDA", "US67066G1040", "BBG000BBJQV0", "USD")
	f.mapTo(t, "uid-nvda", nvda.ID, "USD")
	f.broker.answer("InstrumentsService/FindInstrument", http.StatusOK, `{"instruments":[]}`)
	f.broker.answer(rpcDividends, http.StatusOK, `{"dividends":[]}`)

	if err := f.work(t); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if _, ok := f.stored.stored[nvda.ID]; ok {
		t.Errorf("stored %+v, want nothing written over the calendar already kept", f.stored.stored[nvda.ID])
	}
	if n := f.broker.callCount(rpcDividends); n != 2 {
		t.Errorf("asked the calendar %d times, want twice — the mapped listing, then the figi", n)
	}
}

// A token the broker refuses parks the connection, as the price job does, and
// fails nothing else.
func TestDividendsWorkerParksAConnectionWhoseTokenIsRefused(t *testing.T) {
	f := newDividendFixture(t)
	f.paper(t, "NVDA", "US67066G1040", "BBG000BBJQV0", "USD")
	f.broker.answer(rpcDividends, http.StatusUnauthorized, `{}`)

	if err := f.work(t); err != nil {
		t.Fatalf("Work: %v, want the refusal recorded on the connection rather than returned", err)
	}
	conn, err := f.store.ConnectionByID(f.ctx, f.spaceID, f.conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if conn.Status != StatusTokenRevoked {
		t.Errorf("connection status = %s, want %s", conn.Status, StatusTokenRevoked)
	}
}
