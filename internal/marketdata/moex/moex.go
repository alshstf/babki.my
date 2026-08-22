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
//
// PREVDATE is what dates a quote, and it is not free: measured on 2026-08-03,
// the four boards together are ~117 KB without it and ~167 KB with it (TQCB
// alone goes from ~102 KB to ~141 KB), so each refresh costs about 50 KB more.
// That is the price of the quote knowing which day it belongs to, and it is
// paid on every refresh rather than once, because ISS has no endpoint that
// answers "what dates do the prices you just gave me have".
const requestedColumns = "SECID,ISIN,PREVPRICE,PREVDATE,CURRENCYID"

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
// userAgent identifies this program to the exchange.
const userAgent = "babki.my/1.0 (+https://github.com/alshstf/babki.my)"

func (c *Client) Name() string { return sourceName }

// QuotesFor implements marketdata.QuoteProvider. It queries every board in
// boards, in order, and merges the results.
//
// # What PREVPRICE is, and what On therefore means
//
// PREVPRICE is a PREVIOUS-SESSION price. It is not a live price: measured on
// 2026-08-03 at 23:19 Moscow time, with the evening session trading
// (TRADINGSTATUS=T), SBER on TQBR had PREVPRICE 276.52 while LAST was 280.85
// and PREVDATE read 2026-07-31 — the previous Friday.
//
// It is NOT the previous day's closing price, and this comment used to say it
// was. MOEX publishes that close in a different column: PREVLEGALCLOSEPRICE,
// which ISS titles "официальная цена закрытия предыдущего дня, рассчитываемая
// по методике ФСФР", and the two differ — SBER's were 276.52 and 275.60 on
// the same row, ABIO's 44.74 and 44.86. ISS's own title for PREVPRICE is
// "цена последней сделки нормального периода предыдущего торгового дня": the
// last trade of the main session, not the official close.
//
// Nor is that trade always FROM the session PREVDATE names, and anything
// reasoning about staleness has to know it. Every one of TQCB's 3021 rows was
// checked against /iss/history for 2026-07-31, the session its PREVDATE named
// on 2026-08-03: 1788 prices equal that session's CLOSE (those securities
// traded), 779 equal its LEGALCLOSEPRICE and have no close of their own (they
// did not trade, and the exchange carried the price into the session), and 58
// equal neither — RU000A0JTB96 reported PREVPRICE 119.97 where the session's
// legal close was 118.68 and no trade was recorded at all. RU000A103AP6, on
// the same board, last traded on 2026-07-09 at 75.6 and reported exactly that
// beside PREVDATE 2026-07-31. So for an instrument that did not trade,
// PREVPRICE is a carried price that can be weeks older than the date beside
// it.
//
// # What On is
//
// On is the row's own PREVDATE, parsed as a calendar day at midnight UTC —
// the exchange's statement of which session the price belongs to, never a
// date this process picked. Before #90 it was the caller-supplied argument,
// and quotesWorker passed today: the previous session's price was stored
// under today's date, so on a Monday morning the screen showed Friday's
// price as today's and nothing anywhere said otherwise.
//
// PREVDATE is a property of the BOARD's session and not of the security:
// checked on 2026-08-03, every one of TQBR's 502 rows, TQOB's 62, TQRD's 47
// and 3019 of TQCB's 3021 read the same 2026-07-31, non-traded securities
// included. (ISS's own two names for the column disagree — title "дата
// предыдущего торгового дня", short title "дата последних торгов" — and the
// data settles which one holds.) Taken together with the paragraph above,
// what a stored quote says is: as of the close of the session dated On, the
// exchange's price for this instrument was Price. It does not say a trade
// happened at that price on that day, and anything shown to a user must not
// claim it did.
//
// The two remaining rows read "0000-00-00": see the undatable-row branch in
// the loop below.
//
// Tickers not present on any board, and tickers whose PREVPRICE is null (the
// exchange has no price to report for that session at all — 396 of TQCB's
// 3021 rows on 2026-08-03, and /iss/history has neither a close nor a legal
// close for 390 of them), are silently absent from the result — not an error,
// per the marketdata.QuoteProvider contract.
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
// rows within one board. A row this code cannot publish is not a win: neither
// a null PREVPRICE (the exchange has no price for that security there) nor a
// price it did not date consumes the ticker's one slot, so a later board can
// still supply one an earlier board could not.
//
// Some rule is required here, because a ticker can legitimately appear on
// two boards at two different prices in two different currencies, and our
// catalog has exactly one instrument to attach a quote to. Without a rule
// both quotes are returned, and which one lands in the database is decided
// by upsert ordering — a coin flip between two numbers, published as fact.
func (c *Client) QuotesFor(ctx context.Context, tickers []string) ([]marketdata.TickerQuote, error) {
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
			if row.priceOn == nil {
				// A price ISS did not date. It is dropped, and the ticker is
				// left as if no price had been reported for it — which is what
				// the QuoteProvider contract already means by an absent ticker.
				//
				// Storing it is not open to us: on_date is half of the quotes
				// primary key, so every way of storing this price begins by
				// inventing its day, and an invented day is the whole of #90.
				// Failing the call is worse: one malformed row out of three
				// thousand would un-price every instrument the owner holds, and
				// River would retry into the same poison for as long as ISS
				// kept publishing it.
				//
				// ISS does publish undatable rows. On 2026-08-03 two of TQCB's
				// 3021 carried PREVDATE "0000-00-00": RU000A10EH19 and
				// RU000A10FT14, whose ISSUEDATE and STARTDATEMOEX both read
				// 2026-08-03 — securities that began trading that morning and
				// have no previous session for a date to point at. Both also
				// carried a null PREVPRICE, so today the two conditions
				// coincide and this branch is unreachable in practice; nothing
				// in ISS's contract says they always will, and the branch costs
				// one comparison.
				//
				// Warn, not Debug: an instrument silently losing its price is
				// exactly what goes unnoticed on a production instance, where
				// Debug is off. The raw cell goes into the line because
				// "0000-00-00" and a changed date format are the same failure
				// here and completely different failures to whoever reads it.
				// Only requested tickers reach this point, so the volume is
				// bounded by the catalog rather than by the size of the board.
				c.log.Warn("moex: price came without a readable date, dropping it (this instrument keeps whatever earlier quote it already has)",
					"board", b.label, "ticker", row.ticker,
					"prevdate", row.priceOnRaw, "price", row.price.String())
				continue
			}
			quoted[row.ticker] = true
			quotes = append(quotes, marketdata.TickerQuote{
				Ticker:   row.ticker,
				ISIN:     row.isin,
				Price:    *row.price,
				Currency: normalizeCurrency(row.currency),
				On:       *row.priceOn,
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
// when PREVPRICE was JSON null (the exchange has no price to report for the
// instrument).
type secRow struct {
	ticker string
	price  *decimal.Decimal
	// priceOn is the session price belongs to, read from PREVDATE, as a
	// calendar day at midnight UTC. nil when PREVDATE held nothing this code
	// can read as a day — see parsePrevDate. It is a pointer and not a zero
	// time.Time precisely so that "no date" cannot be mistaken for a date:
	// year 1 is a value the quotes table would accept without complaint.
	priceOn *time.Time
	// priceOnRaw is the PREVDATE cell exactly as ISS sent it, kept for the log
	// line that reports a dropped price. Without it that line could say a date
	// was unreadable but never which one, and "0000-00-00" (a security with no
	// previous session) would be indistinguishable from a changed format.
	priceOnRaw string
	currency   string
	// isin is what the exchange itself calls this paper, and the only field in
	// the answer that identifies a security rather than a listing of one. Empty
	// when ISS sends none — it is nullable, and a few instrument kinds carry no
	// ISIN at all — which is why the ticker is still read beside it.
	isin string
}

// parsePrevDate reads one PREVDATE cell. It returns the day it names at
// midnight UTC — the same shape pgx reads a Postgres DATE back as, so a quote
// compares equal to the row it becomes — and the cell verbatim for logging.
// A nil day means the cell named no day this code can read; the caller
// decides what that costs, and for a row carrying a price the answer is the
// price (see QuotesFor).
//
// Every unreadable form takes the same exit, deliberately. ISS declares the
// column as type "date" and sends it as a YYYY-MM-DD string — on 2026-08-03,
// 3630 of the four boards' 3632 rows read "2026-07-31" — but the remaining two
// read "0000-00-00", a security with no previous session, which fails to parse
// exactly as a changed format would. Since the legitimate case has to be
// tolerated, the malformed one cannot be told apart from it here, so both are
// reported one line per affected price rather than one guessing at the other.
//
// time.Parse is strict, which is what makes this a check and not a formality:
// it rejects "0000-00-00" (month out of range), "", "2026-7-31", "31.07.2026",
// "2026-02-30" and a trailing time-of-day. The one value it would accept and
// this function must not is the zero day, "0001-01-01": it parses cleanly and
// would then be indistinguishable from "no date at all" — the very confusion
// this returns a pointer to avoid.
func parsePrevDate(raw any) (*time.Time, string) {
	text, ok := raw.(string)
	if !ok {
		// Includes JSON null. %v rather than %#v: this ends up in a log line
		// an operator reads, and `<nil>` says more there than `interface {}(nil)`.
		return nil, fmt.Sprintf("%v", raw)
	}
	day, err := time.Parse(time.DateOnly, text)
	if err != nil || day.IsZero() {
		return nil, text
	}
	return &day, text
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

	// Named for the same reason the rate client is (see cbr.userAgent): a public
	// feed is entitled to know who is asking, and the Bank of Russia's answer to
	// Go's default agent is a 403. The exchange serves it today; being served on
	// sufferance by an anonymous client is not a thing to rely on.
	req.Header.Set("User-Agent", userAgent)

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
	dateIdx, ok := index["PREVDATE"]
	if !ok {
		// The whole board at once, and therefore an error rather than a row
		// dropped quietly: if ISS stops sending the column the dates come
		// from, the alternative is to date every price by something other
		// than the exchange's own word, which is the defect #90 closed.
		return nil, fmt.Errorf("moex: %s: response missing PREVDATE column", boardLabel)
	}
	currencyIdx, ok := index["CURRENCYID"]
	if !ok {
		return nil, fmt.Errorf("moex: %s: response missing CURRENCYID column", boardLabel)
	}
	// ISIN is asked for and NOT required: it is how a price finds the right
	// catalog row (see refreshQuotesWorker), and a board that stopped sending it
	// should cost the weaker match rather than the whole board's prices.
	isinIdx, hasISIN := index["ISIN"]

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
		if hasISIN {
			// Not an error when it is not a string: ISS sends null for a
			// security with no ISIN, and the ticker still answers for it.
			row.isin, _ = fields[isinIdx].(string)
		}
		row.priceOn, row.priceOnRaw = parsePrevDate(fields[dateIdx])

		// PREVPRICE is nullable: ISS reports null when the exchange has no
		// price for the security on that board at all. Not merely "it did not
		// trade" — a security that recorded no trade normally keeps the price
		// carried into the session (779 of TQCB's rows on 2026-08-03, checked
		// against /iss/history) and reports it here. A null is the emptier
		// case: /iss/history had no close and no legal close either for 390 of
		// TQCB's 396 nulls that day, and two of the rest were listed that very
		// morning. Normal and expected, not an error — the caller simply won't
		// see this ticker in the result.
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

// goldSecurity is the exchange's spot gold instrument, and goldBoard the board
// its real trading happens on.
//
// ONE GRAM OF GOLD, WHICH IS THE WHOLE REASON THIS EXISTS. The broker denotes
// gold with the code "XAU", and ISO 4217 says that code is a troy OUNCE — 31.1
// grams. It does not mean that here: the owner's own purchases are of
// GLDRUB_TOM, whose unit is a gram, and every figure in his journal is counted
// in those. Taking an ounce-denominated rate to them would be wrong by a factor
// of thirty-one, which is the shape of the most expensive kind of defect this
// program can have — so the rate for "XAU" comes from THIS instrument and no
// other, and the only source that could contradict it (the Bank of Russia)
// publishes no gold rate at all.
//
// Checked against the exchange on 2026-08-21: GLDRUB_TOM closed at 8422.2 on
// 2024-10-21, against the 8654.19 average the owner's own broker reports for
// purchases made around then. A price per ounce would have been near 260 000.
const (
	goldSecurity = "GLDRUB_TOM"
	goldBoard    = "CETS"
)

// GoldRates returns one rate per trading day between from and to inclusive:
// how many rubles one unit of the exchange's spot gold cost that day.
//
// THE CLOSING PRICE, and the board's own. ISS answers this security on four
// boards and three of them are formalities — LICU and SPEC report zeros, CNGD
// reports a handful of trades a day — so a run over every row would take
// whichever came last and land on a zero often enough. CETS is where the
// thousands of trades are.
//
// A day whose close is zero or absent is LEFT OUT rather than stored: a rate of
// nought would value every gram at nothing, and a lookup takes the nearest
// earlier date, so one such row would silently answer for every day after it.
func (c *Client) GoldRates(ctx context.Context, from, to time.Time) ([]marketdata.FxRate, error) {
	// ONE PAGE IS NOT THE ANSWER. ISS caps a history response at a hundred rows
	// and says so only in a cursor block; ask for six years and it hands back the
	// first five weeks without a word about the rest. Taken at face value that
	// filled the table with 2020 and left the nearest-earlier lookup answering
	// every day since with an October-2020 price — measured on the owner's own
	// data, where the first run stored 25 rates ending 2020-10-29.
	//
	// THE CURSOR IS WHAT ENDS THE LOOP, not an empty page. A server that ignores
	// `start` — a proxy, a stub, a version that changes its mind — answers the
	// same page for ever, and a loop that stops only on emptiness never stops at
	// all. The first version of this did exactly that and hung its own test.
	// TOTAL says how many rows exist; PAGESIZE how many came back.
	var out []marketdata.FxRate
	for start := 0; ; {
		page, err := c.goldPage(ctx, from, to, start)
		if err != nil {
			return nil, err
		}
		out = append(out, page.rates...)
		start += page.rows
		if page.rows == 0 || start >= page.total {
			return out, nil
		}
	}
}

// goldPageResult is one page: the rates read from it, how many RAW rows it
// held, and how many rows the whole answer has. The rows are counted raw
// because the page size is about rows — four boards answer for this security
// and only one is read, so a full page of a hundred easily yields twenty-five
// rates.
type goldPageResult struct {
	rates []marketdata.FxRate
	rows  int
	total int
}

// goldPage fetches one page, reading the cursor block ISS attaches alongside it.
func (c *Client) goldPage(ctx context.Context, from, to time.Time, start int) (goldPageResult, error) {
	url := fmt.Sprintf("%s/iss/history/engines/currency/markets/selt/securities/%s.json"+
		"?iss.meta=off&iss.only=history,history.cursor&history.columns=BOARDID,TRADEDATE,CLOSE&from=%s&till=%s&start=%d",
		c.baseURL, goldSecurity, from.Format(time.DateOnly), to.Format(time.DateOnly), start)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return goldPageResult{}, fmt.Errorf("moex: gold history: build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return goldPageResult{}, fmt.Errorf("moex: gold history: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return goldPageResult{}, fmt.Errorf("moex: gold history: unexpected status %d", resp.StatusCode)
	}

	var body struct {
		History struct {
			Columns []string `json:"columns"`
			Data    [][]any  `json:"data"`
		} `json:"history"`
		Cursor struct {
			Columns []string `json:"columns"`
			Data    [][]any  `json:"data"`
		} `json:"history.cursor"`
	}
	dec := json.NewDecoder(resp.Body)
	// The same reason fetchBoard uses it: a price is money, and float64 loses
	// digits ISS actually sent.
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		return goldPageResult{}, fmt.Errorf("moex: gold history: decode: %w", err)
	}

	// TOTAL out of the cursor, which is the only place the answer says how much
	// there is. Absent — an older ISS, a stub — it stays 0 and the loop falls
	// back to stopping on an empty page, which is right for a server that pages
	// honestly and is why this is not an error.
	total := 0
	if len(body.Cursor.Data) > 0 {
		for i, name := range body.Cursor.Columns {
			if name != "TOTAL" || i >= len(body.Cursor.Data[0]) {
				continue
			}
			if num, ok := body.Cursor.Data[0][i].(json.Number); ok {
				if n, err := num.Int64(); err == nil {
					total = int(n)
				}
			}
		}
	}

	idx := make(map[string]int, len(body.History.Columns))
	for i, name := range body.History.Columns {
		idx[name] = i
	}
	boardAt, dateAt, closeAt := idx["BOARDID"], idx["TRADEDATE"], idx["CLOSE"]
	if boardAt < 0 || dateAt < 0 || closeAt < 0 {
		return goldPageResult{}, fmt.Errorf("moex: gold history: the answer names no %s/%s/%s column", "BOARDID", "TRADEDATE", "CLOSE")
	}

	out := make([]marketdata.FxRate, 0, len(body.History.Data))
	for _, row := range body.History.Data {
		if len(row) <= boardAt || len(row) <= dateAt || len(row) <= closeAt {
			continue
		}
		if board, _ := row[boardAt].(string); board != goldBoard {
			continue
		}
		day, ok := row[dateAt].(string)
		if !ok {
			continue
		}
		on, err := time.Parse(time.DateOnly, day)
		if err != nil {
			continue
		}
		num, ok := row[closeAt].(json.Number)
		if !ok {
			continue
		}
		price, err := decimal.NewFromString(num.String())
		if err != nil || !price.IsPositive() {
			continue
		}
		out = append(out, marketdata.FxRate{
			Base: marketdata.GoldCode, Quote: "RUB", On: on, Rate: price, Source: sourceName,
		})
	}
	return goldPageResult{rates: out, rows: len(body.History.Data), total: total}, nil
}
