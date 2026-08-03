// Package moex implements marketdata.QuoteProvider against the Moscow
// Exchange ISS (Informational & Statistical Server) securities endpoints.
package moex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
)

// DefaultBaseURL is the Moscow Exchange ISS API root.
const DefaultBaseURL = "https://iss.moex.com"

// sourceName is used both as the provider's Name() and as Quote.Source for
// every quote this provider returns.
const sourceName = "moex"

// rubCurrencyID is the ISS code for Russian roubles on the CURRENCYID
// column. ISS reports it as "SUR" (a legacy ISO 4217 code for the pre-1998
// redenominated rouble) rather than the current "RUB"; every other
// CURRENCYID value observed on the boards below is already a live ISO 4217
// code and is passed through unchanged.
const rubCurrencyID = "SUR"

// board describes one ISS securities listing to query. ISS has no single
// endpoint covering all instrument kinds, so QuotesFor queries one board per
// kind and merges the results.
type board struct {
	// label identifies the board in error messages (e.g. "shares/TQBR").
	label string
	// path is the endpoint path appended to baseURL.
	path string
}

// boards lists every board QuotesFor queries, in precedence order: when two
// boards report the same ticker, the earlier one here wins (see QuotesFor).
// Adding coverage means adding a line here and nothing else.
//
// Board membership was checked against iss.moex.com on 2026-08-03; the
// security counts quoted below are from that check.
//
//   - shares/TQBR, "Т+: Акции и ДР" (502 securities) — ordinary shares,
//     depositary receipts, and exchange-traded funds. Funds are NOT on a
//     fund-specific board: shares/TQTF, the dedicated ETF board, returns
//     zero securities from ISS today, and for every fund checked (TMOS,
//     SBMX, EQMX, LQDT), ISS's own per-security board list marks TQTF as
//     not traded (is_traded=0) and TQBR as its primary traded board
//     (is_traded=1, is_primary=1) — see e.g. /iss/securities/TMOS.json.
//     (The market-wide /iss/engines/stock/markets/shares/boards.json lists
//     TQTF itself as is_traded=1; that flag describes the board in
//     general, not any given security's listing on it, and does not
//     contradict the above.) TQTF is therefore deliberately not queried —
//     it would cost one request per refresh and return nothing.
//
//   - bonds/TQOB, "Т+: Гособлигации" (62 securities) — government bonds
//     (OFZ) only. It does not carry corporate bonds.
//
//   - bonds/TQCB, "Т+: Облигации" (3021 securities) — the main corporate
//     bond board.
//
//   - bonds/TQRD, "Т+: Облигации Д" (47 securities) — a second corporate
//     bond board, all but one of whose securities (RU000A108CE5) are absent
//     from TQCB, and all 47 of which have a SUR face value. What the "Д"
//     abbreviates is not stated in the ISS board listing and is not guessed
//     at here; neither is a settlement regime of its own, which ISS gives no
//     sign of — all 47 rows carry SETTLEDATE 2026-08-04, a subset of TQCB's
//     own {2026-08-04, 2026-08-05}.
//
//     These are NOT ordinary corporate bonds, and "ordinary" is exactly the
//     word a reader would go by when deciding how far to trust a price from
//     here, so it is worth being precise about. Checked on
//     2026-08-03: all 47 are LISTLEVEL 3, the lowest tier ISS reports (TQCB
//     is mixed — 478 at level 1, 570 at level 2, 1973 at level 3 — and
//     TQOB's 62 OFZ are all level 1). 45 of the 47 quote a PREVPRICE
//     between 2.23 and 46.9, which on this board is percent of face value:
//     two to forty-seven kopecks on each rouble of principal outstanding.
//     Many also carry a FACEVALUE well under the INITIALFACEVALUE of 1000
//     they were issued at (130, 160, 300, 494, 615.40, 666.68), so that
//     percentage is of a reduced principal rather than the original.
//     SHORTNAMEs read КВС, СИБАВТО, МЛФТ, НафттрнБО, РоялКапБ, ЧИСТПЛАН.
//     Whatever the story behind each one, this board is priced like
//     distressed paper, and the prices are real quotes on it.
//
// TQOB, TQCB, and TQRD — the three bonds-market boards queried here — all
// quote PREVPRICE as a percentage of face value, and every shares-market
// board quotes it as money per unit. ISS states this itself rather than
// leaving it to be inferred from the numbers: the per-security board list at
// /iss/securities/<secid>.json carries a `unit` column, and on 2026-08-03 it
// read "%" for TQOB, TQCB and TQRD (checked on RU000A0ZZWQ8 and
// SU26238RMFS4) and "M" for TQBR (checked on SBER).
// portfolio.marketValue, which picks between the two readings by instrument
// type rather than by board, needs no change for the two boards added here.
//
// That percentage-of-face-value convention is not a blanket property of
// "markets/bonds" as a whole, so it does not automatically extend to a
// future board added there. markets/bonds also hosts bonds/TQTC and
// bonds/EQTC ("Т+: ETC" / "Т0 ETC" — exchange-traded commodities, which are
// not bonds); both are is_traded=0 with zero securities today, so nothing
// is wrong yet, but if ETC trading resumes, its PREVPRICE convention needs
// checking on its own before a line for it is added here.
//
// Boards deliberately left out, and why. shares/SMAL (odd lots, 175
// securities) and shares/TQTY (fund units settled in CNY, 6) republish
// tickers that are all already on TQBR, at a different price or in a
// different currency. bonds/TQOD (USD, 221) and bonds/TQOY (CNY, 54)
// likewise republish 210 and 42 TQCB tickers respectively under a different
// settlement currency. A ticker alone does not say which settlement
// currency a holding is in, so querying those boards would hand the
// precedence rule below a choice it has no basis to make.
//
// The above is not a partial list standing in for an exhaustive one. Every
// other board that the stock engine's shares and bonds markets mark
// is_traded=1 today, and that is not already named above, was checked too,
// on 2026-08-03: bonds/TQUD, bonds/TQOE, bonds/AUCT, bonds/PACT, bonds/PAYT,
// and shares/TQIF ("Т+: Паи") all return zero securities, the same as
// shares/TQTF; shares/SPEQ's 84 tickers are all already on TQBR; and
// bonds/SPOB's 41 are all already inside TQOB ∪ TQCB ∪ TQRD. None of them
// need a line here.
var boards = []board{
	{label: "shares/TQBR", path: "/iss/engines/stock/markets/shares/boards/TQBR/securities.json"},
	{label: "bonds/TQOB", path: "/iss/engines/stock/markets/bonds/boards/TQOB/securities.json"},
	{label: "bonds/TQCB", path: "/iss/engines/stock/markets/bonds/boards/TQCB/securities.json"},
	{label: "bonds/TQRD", path: "/iss/engines/stock/markets/bonds/boards/TQRD/securities.json"},
}

// requestedColumns is sent as the securities.columns query parameter on
// every board request, asking ISS to return only the columns this provider
// understands. ISS is free to return them in any order, so responses are
// still mapped by column name, not by position.
//
// securities.columns alone does not stop ISS from also sending the
// marketdata and marketdata_yields blocks alongside securities — those are
// dropped by iss.only=securities instead (see fetchBoard). Measured on
// 2026-08-03, bonds/TQCB (3021 rows) is ~1.85 MB with both blocks attached
// and ~102 KB with only securities requested — the same 3021 rows, same
// columns, same values throughout, just without the two blocks nothing
// here parses.
const requestedColumns = "SECID,PREVPRICE,CURRENCYID"

// Client fetches instrument prices from the Moscow Exchange ISS API.
type Client struct {
	http    *http.Client
	baseURL string
	log     *slog.Logger
}

// New returns a Client. client may be nil, in which case http.DefaultClient
// is used; baseURL may be empty, in which case DefaultBaseURL is used.
// baseURL is parameterized (rather than hardcoded) so tests can point it at
// an httptest.Server instead of the real iss.moex.com.
// log may be nil, in which case slog.Default is used — which is the
// configured logger, since cmd/babki installs it at startup. It is a
// parameter rather than a package-level read so a test can watch the
// empty-board warning without swapping a global out from under every other
// test running beside it.
func New(client *http.Client, baseURL string, log *slog.Logger) *Client {
	if client == nil {
		client = http.DefaultClient
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if log == nil {
		log = slog.Default()
	}
	return &Client{http: client, baseURL: baseURL, log: log}
}

// Name implements marketdata.QuoteProvider.
func (c *Client) Name() string { return sourceName }

// QuotesFor implements marketdata.QuoteProvider. It queries every board in
// boards, in order, and merges the results.
//
// # What PREVPRICE is, and what On therefore means
//
// PREVPRICE is the PREVIOUS TRADING DAY'S CLOSE. It is not a live price and
// it is not "the last price ISS knows right now". Measured on 2026-08-03 at
// 22:30 Moscow time, with the evening session trading (TRADINGSTATUS=T):
// SBER on TQBR had PREVPRICE 276.52 while LAST was 280.86, and the PREVDATE
// column beside it read 2026-07-31 — the previous Friday. So a quote taken
// from here moves at most once per trading day, whatever hour it is fetched.
//
// Every returned quote's On is nonetheless the caller-supplied on, not
// anything read from the response, and quotesWorker passes today. That means
// the stored quote carries today's date on the previous session's price, and
// nothing downstream can tell how old that price is. ISS would say: PREVDATE
// is present on all four boards above (on 2026-08-03 all of them read
// 2026-07-31). This provider does not request that column and does not read
// it, which is a gap rather than a decision, and it is filed as #90 — along
// with the larger question of whether a column that moves intraday should be
// read instead.
//
// Tickers not present on any board, and tickers whose PREVPRICE is null (the
// instrument recorded no close in the previous session), are silently absent
// from the result — not an error, per the marketdata.QuoteProvider contract.
//
// A board that answers with no securities at all is a different matter and
// gets a Warn: see the empty-board branch below for why zero rows means the
// path has stopped pointing at a live board rather than that the board is
// quiet today.
//
// # A board that fails fails the whole call
//
// If any board errors, QuotesFor returns that error and no quotes at all,
// discarding whatever the boards before it already produced. This is
// deliberate, and it is the more useful behaviour rather than merely the
// simpler one.
//
// The QuoteProvider contract gives an absent ticker exactly one meaning:
// no price is available for it. Callers act on that — quotesWorker upserts
// what came back and logs the rest at debug level, and a position with no
// fresh quote is reported to the user as having no quote. So a partial
// result would be indistinguishable from a genuine absence of prices, and
// would quietly attribute a broken request to every instrument on the board
// that failed, naming a cause that is not the cause. Failing loudly instead
// lets River retry the job and leaves the previous quotes standing: stale
// and known to be stale, rather than silently wrong.
//
// # A ticker reported by more than one board
//
// The first board in boards order that reports a usable price for a ticker
// wins; later boards' rows for that same ticker are ignored, as are repeat
// rows within one board. A null PREVPRICE is not a win — it means no trade
// was recorded, not that a value was found — so a later board may still
// supply a price for a ticker an earlier board listed as null.
//
// Some rule is required here, because a ticker can legitimately appear on
// two boards at two different prices in two different currencies, and our
// catalog has exactly one instrument to attach a quote to. Without a rule
// both quotes are returned, and which one lands in the database is decided
// by upsert ordering — a coin flip between two numbers, published as fact.
func (c *Client) QuotesFor(ctx context.Context, tickers []string, on time.Time) ([]marketdata.TickerQuote, error) {
	want := make(map[string]bool, len(tickers))
	for _, t := range tickers {
		want[t] = true
	}

	var quotes []marketdata.TickerQuote
	quoted := make(map[string]bool, len(tickers))
	for _, b := range boards {
		rows, err := c.fetchBoard(ctx, b)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			// A board that answers with no securities at all is not a board
			// with nothing to say — every board queried here lists hundreds or
			// thousands of instruments on any day of the week, holidays
			// included, because securities.json lists what is LISTED rather
			// than what has traded. Zero rows means the path stopped pointing
			// at a live board: ISS answers 200 with an empty securities array
			// for a board that has been renamed or retired, exactly as it does
			// for one that never existed. Seven such paths were found while
			// choosing this board list, so the shape is not hypothetical.
			//
			// Every instrument on that board would then quietly have no price,
			// which reads on screen as "no quote" — a real, expected answer
			// about the instrument rather than a fact about our URL.
			//
			// Warn and carry on, rather than failing the call: the response was
			// valid and the other boards' prices are good. Failing here would
			// throw away three boards' worth of correct data over the fourth,
			// and the log line is what makes the cause findable.
			//
			c.log.Warn("moex: board returned no securities at all, everything listed on it will have no price",
				"board", b.label, "path", b.path)
		}
		for _, row := range rows {
			if !want[row.ticker] || row.price == nil || quoted[row.ticker] {
				continue
			}
			quoted[row.ticker] = true
			quotes = append(quotes, marketdata.TickerQuote{
				Ticker:   row.ticker,
				Price:    *row.price,
				Currency: normalizeCurrency(row.currency),
				On:       on,
			})
		}
	}

	return quotes, nil
}

// normalizeCurrency maps ISS's "SUR" rouble code to the current ISO 4217
// "RUB" code; every other value is returned unchanged.
func normalizeCurrency(currencyID string) string {
	if currencyID == rubCurrencyID {
		return "RUB"
	}
	return currencyID
}

// secRow is one parsed row from an ISS securities response. price is nil
// when PREVPRICE was JSON null (no trade recorded for the instrument).
type secRow struct {
	ticker   string
	price    *decimal.Decimal
	currency string
}

// issSecuritiesResponse mirrors the shape ISS uses for securities.json
// endpoints: a "securities" block holding a column-name list and a matching
// list of positional data rows.
type issSecuritiesResponse struct {
	Securities struct {
		Columns []string `json:"columns"`
		Data    [][]any  `json:"data"`
	} `json:"securities"`
}

// fetchBoard requests and parses one board's securities.json response.
//
// iss.only=securities tells ISS to omit the marketdata and
// marketdata_yields blocks it would otherwise attach to the response —
// blocks this provider decodes into issSecuritiesResponse (which has no
// field for them) and then discards. See the note on requestedColumns for
// the measured cost of leaving it off.
func (c *Client) fetchBoard(ctx context.Context, b board) ([]secRow, error) {
	url := c.baseURL + b.path + "?iss.meta=off&iss.only=securities&securities.columns=" + requestedColumns
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("moex: %s: build request: %w", b.label, err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("moex: %s: request: %w", b.label, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("moex: %s: unexpected status %d", b.label, resp.StatusCode)
	}

	// UseNumber is required so JSON numbers decode as json.Number (the exact
	// digits ISS sent) rather than float64. Prices are money: unmarshaling
	// into float64 and converting to decimal.Decimal afterward would risk
	// losing precision for values with more significant digits than a
	// float64 mantissa can hold exactly.
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()

	var doc issSecuritiesResponse
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("moex: %s: decode response: %w", b.label, err)
	}

	return parseSecurities(b.label, doc.Securities.Columns, doc.Securities.Data)
}

// parseSecurities maps columns/data (ISS's positional row format) into
// secRow values by looking up each field's index from its column name.
// ISS does not guarantee column order, so indices are resolved once here
// rather than assumed to match requestedColumns.
func parseSecurities(boardLabel string, columns []string, data [][]any) ([]secRow, error) {
	index := make(map[string]int, len(columns))
	for i, name := range columns {
		index[name] = i
	}

	secidIdx, ok := index["SECID"]
	if !ok {
		return nil, fmt.Errorf("moex: %s: response missing SECID column", boardLabel)
	}
	priceIdx, ok := index["PREVPRICE"]
	if !ok {
		return nil, fmt.Errorf("moex: %s: response missing PREVPRICE column", boardLabel)
	}
	currencyIdx, ok := index["CURRENCYID"]
	if !ok {
		return nil, fmt.Errorf("moex: %s: response missing CURRENCYID column", boardLabel)
	}

	rows := make([]secRow, 0, len(data))
	for i, fields := range data {
		width := len(columns)
		if len(fields) < width {
			return nil, fmt.Errorf("moex: %s: row %d has %d fields, want %d", boardLabel, i, len(fields), width)
		}

		ticker, ok := fields[secidIdx].(string)
		if !ok {
			return nil, fmt.Errorf("moex: %s: row %d: SECID is not a string: %#v", boardLabel, i, fields[secidIdx])
		}

		currency, ok := fields[currencyIdx].(string)
		if !ok {
			return nil, fmt.Errorf("moex: %s: row %d (%s): CURRENCYID is not a string: %#v", boardLabel, i, ticker, fields[currencyIdx])
		}

		row := secRow{ticker: ticker, currency: currency}

		// PREVPRICE is nullable: ISS reports null when the instrument
		// recorded no close in the previous trading session (e.g. newly
		// listed, currently suspended, or simply not traded that day — two
		// of TQRD's 47 rows were null on 2026-08-03 while carrying the same
		// PREVDATE as the rest). That is a normal, expected condition, not
		// an error — the caller simply won't see this ticker in the result.
		if raw := fields[priceIdx]; raw != nil {
			num, ok := raw.(json.Number)
			if !ok {
				return nil, fmt.Errorf("moex: %s: row %d (%s): PREVPRICE is not a number: %#v", boardLabel, i, ticker, raw)
			}
			price, err := decimal.NewFromString(num.String())
			if err != nil {
				return nil, fmt.Errorf("moex: %s: row %d (%s): parse PREVPRICE %q: %w", boardLabel, i, ticker, num.String(), err)
			}
			row.price = &price
		}

		rows = append(rows, row)
	}

	return rows, nil
}
