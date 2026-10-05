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

// The exchange's spot gold is the only gold rate source; these tests cover the
// board read, the rows left out, and the unit (grams, not ounces).

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

// Only CETS is read: LICU and SPEC report zeros, CNGD a few trades, and a zero
// rate would also answer for every later day.
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
	// Per gram (~8 500 ₽ in October 2024), not per troy ounce (~250 000 ₽).
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

// A null or zero close is not stored: a zero would answer for every later day.
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

// Requests carry this program's User-Agent.
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

// Every page is read (ISS caps answers at a hundred rows): pages of different
// sizes, the last one not empty.
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

// A server ignoring `start` repeats a page forever; the cursor's TOTAL ends
// the loop.
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
