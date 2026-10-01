package moex_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"babki.my/babki/internal/marketdata/moex"
)

const bondHistoryPath = "/iss/history/engines/stock/markets/bonds/boards/TQCB/securities/RU000A103AP6.json"

// exchange is a stand-in ISS that answers the corporate-bond board with one
// bond and that bond's session history, counts the history requests and keeps
// the last one's query. Both answers can be changed between calls.
type exchange struct {
	srv *httptest.Server

	mu            sync.Mutex
	board         string
	history       string
	historyStatus int
	historyAsked  int
	historyQuery  string
}

func newExchange(t *testing.T, prevDate, history string) *exchange {
	t.Helper()
	e := &exchange{history: history, historyStatus: http.StatusOK}
	e.session(prevDate)
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == bondHistoryPath:
			e.historyAsked++
			e.historyQuery = r.URL.RawQuery
			w.WriteHeader(e.historyStatus)
			_, _ = w.Write([]byte(e.history))
		case r.URL.Path == corpPath:
			_, _ = w.Write([]byte(e.board))
		case strings.HasPrefix(r.URL.Path, "/iss/history/"):
			t.Errorf("history asked for a security nobody requested a price of: %s", r.URL.Path)
			_, _ = w.Write([]byte(noHistory))
		default:
			_, _ = w.Write([]byte(`{"securities":{"columns":["SECID","PREVPRICE","PREVDATE","CURRENCYID"],"data":[["OTHER",1,"2026-09-30","SUR"]]}}`))
		}
	}))
	t.Cleanup(e.srv.Close)
	return e
}

// session moves the board to a new previous session.
func (e *exchange) session(prevDate string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.board = `{"securities":{"columns":["SECID","PREVPRICE","PREVDATE","CURRENCYID"],` +
		`"data":[["RU000A103AP6",77.5,"` + prevDate + `","SUR"]]}}`
}

func (e *exchange) asked() (int, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.historyAsked, e.historyQuery
}

// sessions builds a history answer, newest first, from "day:trades" pairs.
func sessions(rows ...string) string {
	cells := make([]string, 0, len(rows))
	for _, r := range rows {
		day, trades, _ := strings.Cut(r, ":")
		cells = append(cells, `["`+day+`",`+trades+`]`)
	}
	return `{"history":{"columns":["TRADEDATE","NUMTRADES"],"data":[` + strings.Join(cells, ",") + `]}}`
}

func bondQuoteDay(t *testing.T, c *moex.Client) time.Time {
	t.Helper()
	quotes, err := c.QuotesFor(context.Background(), []string{"RU000A103AP6"})
	if err != nil {
		t.Fatalf("QuotesFor: %v", err)
	}
	if len(quotes) != 1 {
		t.Fatalf("got %d quotes, want the one bond: %+v", len(quotes), quotes)
	}
	return quotes[0].On
}

func day(s string) time.Time {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return d
}

// A price the exchange carried into the session is dated by the day its trade
// was made, not by the session. The figures are the bond's own, read from ISS
// on 2026-10-01: 77.5 beside PREVDATE 2026-09-30, no trade since 2026-08-24.
func TestACarriedPriceIsDatedByItsLastTrade(t *testing.T) {
	e := newExchange(t, "2026-09-30",
		sessions("2026-09-30:0", "2026-09-29:0", "2026-08-25:0", "2026-08-24:1", "2026-08-21:3"))
	c := moex.New(e.srv.Client(), e.srv.URL, nil)

	if got := bondQuoteDay(t, c); !got.Equal(day("2026-08-24")) {
		t.Errorf("On = %s, want 2026-08-24 — the last session with a trade", got.Format(time.DateOnly))
	}
	_, query := e.asked()
	for _, want := range []string{"till=2026-09-30", "sort_order=desc", "limit=100", "history.columns=TRADEDATE,NUMTRADES"} {
		if !strings.Contains(query, want) {
			t.Errorf("history was asked with %q, want it to carry %s", query, want)
		}
	}
}

func TestAPriceTradedInItsSessionKeepsTheSessionsDate(t *testing.T) {
	e := newExchange(t, "2026-09-30", sessions("2026-09-30:12", "2026-09-29:0"))
	c := moex.New(e.srv.Client(), e.srv.URL, nil)

	if got := bondQuoteDay(t, c); !got.Equal(day("2026-09-30")) {
		t.Errorf("On = %s, want 2026-09-30", got.Format(time.DateOnly))
	}
}

// With no trade in anything read the price is at least as old as the oldest
// session read, and that is the date it gets: old enough for every reader that
// asks how old a price is, and not a day later than the truth.
func TestAPriceWithNoTradeInSightIsDatedByTheOldestSessionRead(t *testing.T) {
	e := newExchange(t, "2026-09-30", sessions("2026-09-30:0", "2026-09-29:0", "2026-05-13:0"))
	c := moex.New(e.srv.Client(), e.srv.URL, nil)

	if got := bondQuoteDay(t, c); !got.Equal(day("2026-05-13")) {
		t.Errorf("On = %s, want 2026-05-13", got.Format(time.DateOnly))
	}
}

// History that has not caught up with the board cannot say whether the session
// traded. The session's date stands, and the question is asked again next time
// rather than remembered as answered.
func TestHistoryThatDoesNotReachTheSessionIsAskedAgain(t *testing.T) {
	for name, history := range map[string]string{
		"behind": sessions("2026-09-29:4"),
		"empty":  noHistory,
	} {
		t.Run(name, func(t *testing.T) {
			e := newExchange(t, "2026-09-30", history)
			c := moex.New(e.srv.Client(), e.srv.URL, nil)

			if got := bondQuoteDay(t, c); !got.Equal(day("2026-09-30")) {
				t.Errorf("On = %s, want 2026-09-30", got.Format(time.DateOnly))
			}
			bondQuoteDay(t, c)
			if n, _ := e.asked(); n != 2 {
				t.Errorf("history was asked %d times over two refreshes, want 2", n)
			}
		})
	}
}

// The refresh runs every half hour and the session moves once a day: history
// is read once per session, and again when the session changes.
func TestHistoryIsReadOncePerSession(t *testing.T) {
	e := newExchange(t, "2026-09-30", sessions("2026-09-30:0", "2026-08-24:1"))
	c := moex.New(e.srv.Client(), e.srv.URL, nil)

	bondQuoteDay(t, c)
	bondQuoteDay(t, c)
	if n, _ := e.asked(); n != 1 {
		t.Fatalf("history was asked %d times for one session, want 1", n)
	}

	e.session("2026-10-01")
	e.mu.Lock()
	e.history = sessions("2026-10-01:2", "2026-09-30:0")
	e.mu.Unlock()
	if got := bondQuoteDay(t, c); !got.Equal(day("2026-10-01")) {
		t.Errorf("On = %s after the session moved, want 2026-10-01", got.Format(time.DateOnly))
	}
	if n, _ := e.asked(); n != 2 {
		t.Errorf("history was asked %d times across two sessions, want 2", n)
	}
}

// A history request that fails fails the call: publishing the price under the
// session's date instead would store the very date this lookup exists to
// correct.
func TestAFailingHistoryFailsTheCall(t *testing.T) {
	e := newExchange(t, "2026-09-30", `{}`)
	e.historyStatus = http.StatusInternalServerError
	c := moex.New(e.srv.Client(), e.srv.URL, nil)

	quotes, err := c.QuotesFor(context.Background(), []string{"RU000A103AP6"})
	if err == nil {
		t.Fatalf("QuotesFor returned %+v and no error", quotes)
	}
	if !strings.Contains(err.Error(), "history of RU000A103AP6") {
		t.Errorf("error = %v, want it to name the security whose history failed", err)
	}
}
