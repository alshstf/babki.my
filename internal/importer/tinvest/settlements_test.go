package tinvest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/logtest"
)

const rpcBrokerReport = "OperationsService/GetBrokerReport"

// reportRow is one trade of a broker report as the gateway sends it — the
// three fields TradeSettlement reads, beside one it does not, so a decoder
// that refused unknown fields would be caught.
func reportRow(tradeID, tradedAt, clearValueDate string) string {
	return fmt.Sprintf(`{"tradeId":%q,"tradeDatetime":%q,"clearValueDate":%q,"ticker":"SBER"}`,
		tradeID, tradedAt, clearValueDate)
}

// brokerReport answers both requests of the report method: the task id and
// a finished one-page report; each decoder reads its own half.
func brokerReport(rows ...string) string {
	return `{"generateBrokerReportResponse":{"taskId":"task-1"},` +
		`"getBrokerReportResponse":{"brokerReport":[` + strings.Join(rows, ",") + `],` +
		`"itemsCount":` + fmt.Sprint(len(rows)) + `,"pagesCount":1,"page":0}}`
}

// buyWithTrades is buy.json with the trades it was filled in, in the shape the
// owner's live account lists them (tradesInfo.trades[].num, checked against
// the broker report on 2026-10-05). Two fills, as a large order often has.
func buyWithTrades(t *testing.T) string {
	t.Helper()
	page := opFixture(t, "buy.json")
	trades := `"quantityDone": "100", "tradesInfo": {"trades": [` +
		`{"num": "901", "date": "2026-03-14T21:30:00.120Z", "quantity": "60"},` +
		`{"num": "902", "date": "2026-03-14T21:30:01.004Z", "quantity": "40"}]}`
	out := strings.Replace(page, `"quantityDone": "100"`, trades, 1)
	if out == page {
		t.Fatal("buy.json no longer has the field the trades are attached after")
	}
	return out
}

// on is a calendar day, as every journal date is held.
func on(s string) time.Time {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return d
}

// -------------------------------------------------------------------------
// the report, on the wire
// -------------------------------------------------------------------------

// The report is ordered, waited for (the client's sleep, at the stated
// interval) and read page by page; a trade without a settlement day is
// skipped.
func TestTradeSettlementsWaitsForTheReportAndReadsEveryPage(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readRequestBody(r)
		calls = append(calls, string(body))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(string(body), "generateBrokerReportRequest"):
			_, _ = w.Write([]byte(`{"generateBrokerReportResponse":{"taskId":"task-7"}}`))
		case len(calls) == 2:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":3,"message":"Task not completed yet, please try again later","description":"30058"}`))
		case strings.Contains(string(body), `"page":0`):
			_, _ = w.Write([]byte(`{"getBrokerReportResponse":{"brokerReport":[` +
				reportRow("901", "2026-03-16T07:00:00Z", "2026-03-17T00:00:00Z") + `,` +
				reportRow("903", "2026-03-16T07:01:00Z", "") +
				`],"pagesCount":2,"page":0}}`))
		default:
			_, _ = w.Write([]byte(`{"getBrokerReportResponse":{"brokerReport":[` +
				reportRow("902", "2026-03-20T07:00:00Z", "2026-03-23T00:00:00Z") +
				`],"pagesCount":2,"page":1}}`))
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.Client(), srv.URL, "tok", nil)
	var slept []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}

	got, err := c.TradeSettlements(context.Background(), "2000000001",
		on("2026-03-01"), on("2026-04-01"))
	if err != nil {
		t.Fatalf("TradeSettlements: %v", err)
	}

	want := []TradeSettlement{
		{TradeID: "901", TradedAt: time.Date(2026, 3, 16, 7, 0, 0, 0, time.UTC), SettledOn: on("2026-03-17")},
		{TradeID: "902", TradedAt: time.Date(2026, 3, 20, 7, 0, 0, 0, time.UTC), SettledOn: on("2026-03-23")},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d trades %+v, want %+v", len(got), got, want)
	}
	for i := range want {
		if got[i].TradeID != want[i].TradeID || !got[i].TradedAt.Equal(want[i].TradedAt) || !got[i].SettledOn.Equal(want[i].SettledOn) {
			t.Errorf("trade %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(calls) != 4 {
		t.Fatalf("made %d calls, want 4 (order, not ready, page 0, page 1): %q", len(calls), calls)
	}
	if !strings.Contains(calls[0], `"from":"2026-03-01T00:00:00Z"`) || !strings.Contains(calls[0], `"to":"2026-04-01T00:00:00Z"`) ||
		!strings.Contains(calls[0], `"accountId":"2000000001"`) {
		t.Errorf("the order asked for %s, want the account and the month as asked", calls[0])
	}
	if !strings.Contains(calls[1], `"taskId":"task-7"`) {
		t.Errorf("the report was asked for by %s, want the task the order named", calls[1])
	}
	if len(slept) != 1 || slept[0] != brokerReportPollInterval {
		t.Errorf("slept %v, want one wait of %s for the one report that was not ready", slept, brokerReportPollInterval)
	}
}

// A report that is never built ends the wait after the stated number of polls
// with an error, rather than polling for as long as the job lives.
func TestTradeSettlementsGivesUpOnAReportThatIsNeverBuilt(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readRequestBody(r)
		calls++
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "generateBrokerReportRequest") {
			_, _ = w.Write([]byte(`{"generateBrokerReportResponse":{"taskId":"task-7"}}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":3,"message":"Task not completed yet","description":"30058"}`))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.Client(), srv.URL, "tok", nil)
	c.sleep = func(context.Context, time.Duration) error { return nil }

	_, err := c.TradeSettlements(context.Background(), "2000000001", on("2026-03-01"), on("2026-04-01"))
	if !errors.Is(err, errReportNotReady) {
		t.Fatalf("err = %v, want one wrapping errReportNotReady", err)
	}
	if calls != 1+brokerReportMaxPolls {
		t.Errorf("made %d calls, want the order and %d polls", calls, brokerReportMaxPolls)
	}
}

// A settlement day that is not a calendar day is refused rather than rounded
// to one it might not be.
func TestTradeSettlementsRefusesASettlementDayThatIsNotADay(t *testing.T) {
	srv, _ := serve(t, map[string]route{
		rpcPathPrefix + rpcBrokerReport: {status: http.StatusOK, body: []byte(brokerReport(
			reportRow("901", "2026-03-16T07:00:00Z", "2026-03-16T21:00:00Z")))},
	})
	c := NewClient(srv.Client(), srv.URL, "tok", nil)

	if _, err := c.TradeSettlements(context.Background(), "2000000001", on("2026-03-01"), on("2026-04-01")); err == nil ||
		!strings.Contains(err.Error(), "not a calendar day") {
		t.Fatalf("err = %v, want a refusal naming the day that is not a day", err)
	}
}

// -------------------------------------------------------------------------
// which months to read
// -------------------------------------------------------------------------

func TestDueSettlementMonths(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	march, september, october := on("2026-03-01"), on("2026-09-01"), on("2026-10-01")
	cases := []struct {
		name      string
		unsettled []time.Time
		read      map[time.Time]time.Time
		want      []time.Time
	}{
		{
			"never read, newest first",
			[]time.Time{march, october, september},
			nil,
			[]time.Time{october, september, march},
		},
		{
			"read once it was final: done for good",
			[]time.Time{march},
			map[time.Time]time.Time{march: on("2026-04-11")},
			nil,
		},
		{
			"read before it was final, a day ago: read again",
			[]time.Time{september},
			map[time.Time]time.Time{september: now.Add(-settlementRecheck)},
			[]time.Time{september},
		},
		{
			"read before it was final, an hour ago: not yet",
			[]time.Time{october},
			map[time.Time]time.Time{october: now.Add(-time.Hour)},
			nil,
		},
		{
			"read just before the grace ended: read again",
			[]time.Time{march},
			map[time.Time]time.Time{march: on("2026-04-11").Add(-time.Second)},
			[]time.Time{march},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := dueSettlementMonths(c.unsettled, c.read, now)
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// -------------------------------------------------------------------------
// the day, onto the trade
// -------------------------------------------------------------------------

func TestSettlementDay(t *testing.T) {
	raw := json.RawMessage(`{"tradesInfo":{"trades":[{"num":"901"},{"num":"902"}]}}`)
	cases := []struct {
		name  string
		raw   json.RawMessage
		known map[string]time.Time
		want  string
	}{
		{
			"every trade known and agreeing", raw,
			map[string]time.Time{"901": on("2026-03-17"), "902": on("2026-03-17")},
			"2026-03-17",
		},
		{"one trade not known yet", raw, map[string]time.Time{"901": on("2026-03-17")}, ""},
		{
			"two days for one entry", raw,
			map[string]time.Time{"901": on("2026-03-17"), "902": on("2026-03-18")},
			"",
		},
		{
			"no trades listed", json.RawMessage(`{"id":"op-1"}`),
			map[string]time.Time{"901": on("2026-03-17")},
			"",
		},
		{
			"trades that are not a list", json.RawMessage(`{"tradesInfo":{"trades":{"num":"901"}}}`),
			map[string]time.Time{"901": on("2026-03-17")},
			"",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := settlementDay(c.raw, c.known)
			switch {
			case c.want == "" && got != nil:
				t.Errorf("got %s, want no day", got.Format(time.DateOnly))
			case c.want != "" && (got == nil || !got.Equal(on(c.want))):
				t.Errorf("got %v, want %s", got, c.want)
			}
		})
	}
}

func TestSettleOnTakesOnlyTheTradeAndOnlyAPlausibleDay(t *testing.T) {
	settled := on("2026-03-17")
	cases := []struct {
		name string
		typ  operation.Type
		on   string
		day  *time.Time
		want bool
	}{
		{"a purchase", operation.TypeBuy, "2026-03-16", &settled, true},
		{"a sale", operation.TypeSell, "2026-03-16", &settled, true},
		{"a currency exchange", operation.TypeConversion, "2026-03-16", &settled, true},
		{"a commission charged beside the trade", operation.TypeFee, "2026-03-16", &settled, false},
		{"settled on the trade's own day", operation.TypeBuy, "2026-03-17", &settled, true},
		{"settled before it was traded", operation.TypeBuy, "2026-03-18", &settled, false},
		{"settled over a month later", operation.TypeBuy, "2026-02-10", &settled, false},
		{"no day known", operation.TypeBuy, "2026-03-16", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			op := operation.Operation{Type: c.typ, OccurredOn: on(c.on)}
			settleOn(&op, c.day)
			if got := op.SettledOn != nil; got != c.want {
				t.Fatalf("settled = %v, want %v", op.SettledOn, c.want)
			}
			if c.want && op.SettledOn == c.day {
				t.Error("the entry points at the caller's day, want a copy of its own")
			}
		})
	}
}

// -------------------------------------------------------------------------
// the store
// -------------------------------------------------------------------------

// The months still to read are the TRADES' months, and a trade drops out of
// them once its day is stored — whichever month's report brought it.
func TestStoreListsTheMonthsOfTradesWithNoSettlementDay(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	// Ordered on the last evening of March, filled in the first minutes of
	// April: the trade's month is April.
	late := op("op-late", "OPERATION_TYPE_BUY", "uid-1", time.Date(2026, 3, 31, 23, 59, 0, 0, time.UTC), "rub", -100, 0, 1)
	late.Raw = json.RawMessage(`{"id":"op-late","tradesInfo":{"trades":[{"num":"901","date":"2026-04-01T00:01:00Z"}]}}`)
	// A trade whose own date does not read takes its operation's.
	undated := op("op-undated", "OPERATION_TYPE_BUY", "uid-1", time.Date(2026, 2, 10, 9, 0, 0, 0, time.UTC), "rub", -200, 0, 1)
	undated.Raw = json.RawMessage(`{"id":"op-undated","tradesInfo":{"trades":[{"num":"902","date":"yesterday"}]}}`)
	deposit := op("op-in", "OPERATION_TYPE_INPUT", "", time.Date(2026, 1, 10, 9, 0, 0, 0, time.UTC), "rub", 300, 0, 0)
	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{late, undated, deposit}, now); err != nil {
		t.Fatalf("SyncMirror: %v", err)
	}

	due, err := f.store.dueSettlementMonthsOf(f.ctx, f.link.ID, now)
	if err != nil {
		t.Fatalf("dueSettlementMonthsOf: %v", err)
	}
	if fmt.Sprint(due) != fmt.Sprint([]time.Time{on("2026-04-01"), on("2026-02-01")}) {
		t.Fatalf("due = %v, want April then February", due)
	}

	if err := f.store.saveTradeSettlements(f.ctx, f.link.ID, on("2026-04-01"), []TradeSettlement{
		{TradeID: "901", TradedAt: time.Date(2026, 4, 1, 0, 1, 0, 0, time.UTC), SettledOn: on("2026-04-02")},
		{TradeID: "901", TradedAt: time.Date(2026, 4, 1, 0, 1, 0, 0, time.UTC), SettledOn: on("2026-04-03")},
	}, now); err != nil {
		t.Fatalf("saveTradeSettlements: %v", err)
	}
	due, err = f.store.dueSettlementMonthsOf(f.ctx, f.link.ID, now)
	if err != nil {
		t.Fatalf("dueSettlementMonthsOf: %v", err)
	}
	if fmt.Sprint(due) != fmt.Sprint([]time.Time{on("2026-02-01")}) {
		t.Fatalf("due = %v, want February alone once April's trade has its day", due)
	}
	known, err := f.store.tradeSettlementsByLink(f.ctx, f.link.ID)
	if err != nil {
		t.Fatalf("tradeSettlementsByLink: %v", err)
	}
	if len(known) != 1 || !known["901"].Equal(on("2026-04-03")) {
		t.Errorf("stored %v, want trade 901 on the day stated last", known)
	}

	// A month read long after it ended is done for good, even with a trade its
	// report did not list.
	if err := f.store.saveTradeSettlements(f.ctx, f.link.ID, on("2026-02-01"), nil, now); err != nil {
		t.Fatalf("saveTradeSettlements: %v", err)
	}
	if due, err := f.store.dueSettlementMonthsOf(f.ctx, f.link.ID, now.Add(time.Hour)); err != nil || len(due) != 0 {
		t.Errorf("due an hour after reading February = %v, %v; want nothing — February was read after it was final", due, err)
	}
}

// -------------------------------------------------------------------------
// the sync, end to end
// -------------------------------------------------------------------------

// The hourly run reads the report for the month of a trade that has no
// settlement day, and the same run's rebuild writes the day onto the purchase.
// The next run reads nothing again and leaves the entry as it is.
func TestSyncWorkerDatesATradeByTheDayTheBrokerReportSaysItSettled(t *testing.T) {
	f := newWorkerFixture(t)
	f.broker.answer(rpcOperations, http.StatusOK, operationsPage(buyWithTrades(t)))
	f.broker.answer(rpcInstrumentB, http.StatusOK, string(readFixture(t, "instrument.json")))
	f.broker.answer(rpcBrokerReport, http.StatusOK, brokerReport(
		reportRow("901", "2026-03-14T21:30:00Z", "2026-03-17T00:00:00Z"),
		reportRow("902", "2026-03-14T21:30:01Z", "2026-03-17T00:00:00Z")))

	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("Work: %v", err)
	}
	journal := f.journal(t)
	if len(journal) != 1 || journal[0].Type != operation.TypeBuy {
		t.Fatalf("journal = %+v, want the one purchase", journal)
	}
	if journal[0].SettledOn == nil || !journal[0].SettledOn.Equal(on("2026-03-17")) {
		t.Fatalf("the purchase settled on %v, want 2026-03-17 — the broker report's day", journal[0].SettledOn)
	}
	if n := f.broker.callCount(rpcBrokerReport); n != 2 {
		t.Fatalf("the report method was called %d times, want 2 (the order and its one page)", n)
	}

	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("second Work: %v", err)
	}
	if n := f.broker.callCount(rpcBrokerReport); n != 2 {
		t.Errorf("the second run called the report method again (%d calls), want no call — every trade has its day", n)
	}
	again := f.journal(t)
	if len(again) != 1 || again[0].ID != journal[0].ID {
		t.Errorf("the second run rewrote the purchase: %+v, want the same entry kept", again)
	}
}

// The owner waiting on "sync now" is not kept waiting for the report: only the
// hourly run reads it, and the trade keeps its trade day until then.
func TestSyncWorkerLeavesTheReportToTheHourlyRun(t *testing.T) {
	f := newWorkerFixture(t)
	f.broker.answer(rpcOperations, http.StatusOK, operationsPage(buyWithTrades(t)))
	f.broker.answer(rpcInstrumentB, http.StatusOK, string(readFixture(t, "instrument.json")))
	f.broker.answer(rpcBrokerReport, http.StatusOK, brokerReport(
		reportRow("901", "2026-03-14T21:30:00Z", "2026-03-17T00:00:00Z"),
		reportRow("902", "2026-03-14T21:30:01Z", "2026-03-17T00:00:00Z")))

	if err := f.work(t, "manual"); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if n := f.broker.callCount(rpcBrokerReport); n != 0 {
		t.Errorf("a manual run called the report method %d times, want none", n)
	}
	if journal := f.journal(t); len(journal) != 1 || journal[0].SettledOn != nil {
		t.Errorf("journal = %+v, want the purchase with no settlement day yet", journal)
	}
}

// A report the broker will not hand over fails nothing: the run finishes, the
// purchase is in the journal on its trade day, and the log says why.
func TestSyncWorkerFinishesARunWhoseReportFailed(t *testing.T) {
	f := newWorkerFixture(t)
	f.broker.answer(rpcOperations, http.StatusOK, operationsPage(buyWithTrades(t)))
	f.broker.answer(rpcInstrumentB, http.StatusOK, string(readFixture(t, "instrument.json")))
	f.broker.answer(rpcBrokerReport, http.StatusInternalServerError, `{"code":13,"message":"internal"}`)

	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("Work: %v, want the run to finish", err)
	}
	if journal := f.journal(t); len(journal) != 1 || journal[0].SettledOn != nil {
		t.Errorf("journal = %+v, want the purchase on its trade day", journal)
	}
	if runs := f.runs(t); len(runs) != 1 || runs[0].Status != RunOK {
		t.Errorf("runs = %+v, want one finished ok", runs)
	}
	found := false
	for _, r := range f.logs.Records() {
		if strings.Contains(r.Message, "settlement days failed") {
			found = true
		}
	}
	if !found {
		t.Errorf("no log line says the report failed: %s", logtest.Describe(f.logs.Records()))
	}
}

// fakeReport answers TradeSettlements from a fixed list and remembers which
// months it was asked for.
type fakeReport struct {
	trades []TradeSettlement
	asked  []time.Time
	tick   func()
}

func (r *fakeReport) TradeSettlements(_ context.Context, _ string, from, _ time.Time) ([]TradeSettlement, error) {
	r.asked = append(r.asked, from)
	if r.tick != nil {
		r.tick()
	}
	return r.trades, nil
}

// One run reads months newest first until its time is spent, and leaves the
// rest to the next run.
func TestReadSettlementsStopsWhenItsTimeIsSpent(t *testing.T) {
	f := newFixture(t)
	clock := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	var items []OperationItem
	for i, month := range []time.Month{time.January, time.February, time.March} {
		item := op(fmt.Sprintf("op-%d", i), "OPERATION_TYPE_BUY", "uid-1",
			time.Date(2026, month, 10, 9, 0, 0, 0, time.UTC), "rub", int64(-100-i), 0, 1)
		item.Raw = json.RawMessage(fmt.Sprintf(`{"tradesInfo":{"trades":[{"num":"%d","date":"2026-%02d-10T09:00:00Z"}]}}`, 900+i, month))
		items = append(items, item)
	}
	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, items, clock); err != nil {
		t.Fatalf("SyncMirror: %v", err)
	}
	// Every report takes a minute; the budget is a minute and a half.
	src := &fakeReport{tick: func() { clock = clock.Add(time.Minute) }}

	readSettlements(f.ctx, f.store, src, []AccountLink{f.link}, func() time.Time { return clock },
		90*time.Second, slog.New(&logtest.Capture{}))

	if fmt.Sprint(src.asked) != fmt.Sprint([]time.Time{on("2026-03-01"), on("2026-02-01")}) {
		t.Errorf("asked for %v, want March then February and no more", src.asked)
	}
}
