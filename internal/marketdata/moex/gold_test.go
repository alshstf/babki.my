package moex_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"babki.my/babki/internal/marketdata/moex"
)

// The exchange's spot gold is the only source this program has for what gold is
// worth: the central bank publishes no rate for it at all. These tests are about
// reading that answer — which board it is read from, which rows are left out,
// and the unit, which is the one thing here that could be wrong by a factor of
// thirty-one.

const goldAnswer = `{"history":{
  "columns":["BOARDID","TRADEDATE","CLOSE"],
  "data":[
    ["CETS","2024-10-21",8422.2],
    ["CNGD","2024-10-21",8440],
    ["LICU","2024-10-21",0],
    ["SPEC","2024-10-21",0],
    ["CETS","2024-10-22",8479],
    ["CETS","2024-10-23",null],
    ["CETS","2024-10-24",0]
  ]},
  "history.cursor":{"columns":["INDEX","TOTAL","PAGESIZE"],"data":[[0,7,100]]}}`

func goldServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestGoldRatesReadsTheTradedBoardAndSkipsTheFormalities. ISS answers this
// security on four boards and three of them are formalities: LICU and SPEC
// report zeros, CNGD a handful of trades a day. A run over every row would take
// whichever came last, which is a zero often enough — and a rate of nought
// values every gram at nothing, for that day AND every day after it, since a
// lookup takes the nearest earlier date.
func TestGoldRatesReadsTheTradedBoardAndSkipsTheFormalities(t *testing.T) {
	srv := goldServer(t, goldAnswer)
	c := moex.New(srv.Client(), srv.URL, nil)

	got, err := c.GoldRates(context.Background(),
		time.Date(2024, 10, 20, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 10, 30, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("GoldRates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rates %+v, want 2: the two CETS days that closed at a real price", len(got), got)
	}
	if got[0].Rate.String() != "8422.2" {
		t.Errorf("first rate = %s, want 8422.2 — CETS's close, not CNGD's 8440", got[0].Rate)
	}
	// THE UNIT, pinned as a magnitude rather than as a comment. One gram of
	// gold cost about eight and a half thousand rubles in October 2024; one
	// troy OUNCE — which is what ISO 4217 says the code XAU means — cost about
	// a quarter of a million. The owner's own broker reports an average of
	// 8654.19 for purchases made around then, and his purchases are of this
	// instrument.
	if got[0].Rate.IntPart() > 100_000 {
		t.Errorf("first rate = %s, which is an OUNCE and not a gram: every figure in the journal counts grams", got[0].Rate)
	}
	if got[0].Base != "XAU" || got[0].Quote != "RUB" {
		t.Errorf("rate is %s/%s, want XAU/RUB", got[0].Base, got[0].Quote)
	}
	if !got[0].On.Equal(time.Date(2024, 10, 21, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("first rate is dated %s, want 2024-10-21", got[0].On.Format(time.DateOnly))
	}
	if got[1].Rate.String() != "8479" {
		t.Errorf("second rate = %s, want 8479", got[1].Rate)
	}
}

// TestGoldRatesLeavesOutADayWithNoPrice: a null close and a zero close are both
// "the exchange did not price it that day", and neither may be stored. Nought
// is the dangerous one — it is a number, so nothing downstream would question
// it, and the nearest-earlier lookup would answer with it for every later day
// until a real price arrived.
func TestGoldRatesLeavesOutADayWithNoPrice(t *testing.T) {
	srv := goldServer(t, goldAnswer)
	c := moex.New(srv.Client(), srv.URL, nil)

	got, err := c.GoldRates(context.Background(),
		time.Date(2024, 10, 20, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 10, 30, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("GoldRates: %v", err)
	}
	for _, r := range got {
		if !r.Rate.IsPositive() {
			t.Errorf("a rate of %s was stored for %s", r.Rate, r.On.Format(time.DateOnly))
		}
		if r.On.Equal(time.Date(2024, 10, 23, 0, 0, 0, 0, time.UTC)) ||
			r.On.Equal(time.Date(2024, 10, 24, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("the day with no close (%s) was stored anyway", r.On.Format(time.DateOnly))
		}
	}
}

// TestGoldRatesNamesItselfToTheExchange: the same header the rest of this
// program sends, and for the reason recorded on cbr.userAgent — the Bank of
// Russia answers Go's default agent with 403, and being served on sufferance by
// an anonymous client is not a thing to rely on anywhere.
func TestGoldRatesNamesItselfToTheExchange(t *testing.T) {
	var agent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent = r.Header.Get("User-Agent")
		_, _ = io.WriteString(w, `{"history":{"columns":[],"data":[]}}`)
	}))
	defer srv.Close()

	c := moex.New(srv.Client(), srv.URL, nil)
	if _, err := c.GoldRates(context.Background(), time.Now().AddDate(0, 0, -1), time.Now()); err != nil {
		t.Fatalf("GoldRates: %v", err)
	}
	if agent == "" || len(agent) > 6 && agent[:6] == "Go-htt" {
		t.Errorf("User-Agent = %q, want this program's own", agent)
	}
}

// TestGoldRatesReadsEveryPage is the defect the owner's own data found within
// minutes of the first run: ISS caps a history answer at a hundred rows and
// mentions it only in a cursor block, so six years of gold came back as five
// weeks of 2020 — and the nearest-earlier lookup then answered every day since
// with an October-2020 price.
//
// The pages here are deliberately of different sizes, and the last one is not
// empty: a reader that stopped at the first short page, or one that needed an
// empty page to stop, would each miss something.
func TestGoldRatesReadsEveryPage(t *testing.T) {
	pages := map[string]string{
		"0": `{"history":{"columns":["BOARDID","TRADEDATE","CLOSE"],
			"data":[["CETS","2024-10-21",8422.2],["CNGD","2024-10-21",8440]]},
			"history.cursor":{"columns":["INDEX","TOTAL","PAGESIZE"],"data":[[0,3,2]]}}`,
		"2": `{"history":{"columns":["BOARDID","TRADEDATE","CLOSE"],
			"data":[["CETS","2024-10-22",8479]]},
			"history.cursor":{"columns":["INDEX","TOTAL","PAGESIZE"],"data":[[2,3,2]]}}`,
	}
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := r.URL.Query().Get("start")
		asked = append(asked, start)
		body, ok := pages[start]
		if !ok {
			t.Errorf("asked for start=%q, which this fixture does not have", start)
			body = `{"history":{"columns":[],"data":[]}}`
		}
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	c := moex.New(srv.Client(), srv.URL, nil)
	got, err := c.GoldRates(context.Background(),
		time.Date(2024, 10, 20, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 10, 30, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("GoldRates: %v", err)
	}
	if len(asked) != 2 {
		t.Fatalf("asked for %v, want two pages: the first says three rows exist and hands back two", asked)
	}
	if len(got) != 2 || got[1].Rate.String() != "8479" {
		t.Fatalf("got %+v, want both CETS days — the second one lives on the second page", got)
	}
}

// TestGoldRatesStopsWhenTheAnswerRepeatsItself is the loop's own safety. A
// server that ignores `start` — a proxy, a stub, a version that changes its
// mind — answers the same page for ever, and the first version of this reader
// stopped only on an empty page and hung its own test. The cursor's TOTAL is
// what ends it instead.
func TestGoldRatesStopsWhenTheAnswerRepeatsItself(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{"history":{"columns":["BOARDID","TRADEDATE","CLOSE"],
			"data":[["CETS","2024-10-21",8422.2]]},
			"history.cursor":{"columns":["INDEX","TOTAL","PAGESIZE"],"data":[[0,1,100]]}}`)
	}))
	defer srv.Close()

	done := make(chan struct{})
	go func() {
		c := moex.New(srv.Client(), srv.URL, nil)
		_, _ = c.GoldRates(context.Background(),
			time.Date(2024, 10, 20, 0, 0, 0, 0, time.UTC),
			time.Date(2024, 10, 30, 0, 0, 0, 0, time.UTC))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("GoldRates did not finish: it asked %d times and is still going", calls)
	}
	if calls != 1 {
		t.Errorf("asked %d times, want 1 — the answer said there is one row and handed it over", calls)
	}
}
