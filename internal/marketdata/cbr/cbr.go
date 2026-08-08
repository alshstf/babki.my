// Package cbr implements marketdata.FxProvider and marketdata.FxHistoryProvider
// against the Bank of Russia's FX rate feeds: the daily one (XML_daily.asp)
// and the historical, date-range one (XML_dynamic.asp).
package cbr

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"golang.org/x/text/encoding/charmap"

	"babki.my/babki/internal/marketdata"
)

// DefaultBaseURL is the Bank of Russia's daily FX rates endpoint.
const DefaultBaseURL = "https://www.cbr.ru/scripts/XML_daily.asp"

// DefaultDynamicURL is the Bank of Russia's historical ("dynamic") endpoint:
// one request returns a whole date range of one currency's rates.
const DefaultDynamicURL = "https://www.cbr.ru/scripts/XML_dynamic.asp"

// sourceName is used both as the provider's Name() and as FxRate.Source for
// every rate this provider returns.
const sourceName = "cbr"

// dateLayout is the format cbr.ru uses for the ?date_req= request parameter,
// the <ValCurs Date="..."> response attribute of the daily document, and the
// <Record Date="..."> attribute of the historical one.
const dateLayout = "02.01.2006"

// rangeDateLayout is the format XML_dynamic.asp expects for its date_req1 and
// date_req2 parameters. It is slash-separated, unlike everything else cbr.ru
// exchanges dates in.
const rangeDateLayout = "02/01/2006"

// Client fetches and parses the Bank of Russia's FX rate feeds: the daily
// document (all currencies on one date) and the historical one (one currency
// over a date range).
type Client struct {
	http       *http.Client
	dailyURL   string
	dynamicURL string
}

// New returns a Client. client may be nil, in which case http.DefaultClient
// is used; baseURL may be empty, in which case the two default endpoint URLs
// are used. baseURL is parameterized (rather than hardcoded) so tests can
// point it at an httptest.Server instead of the real cbr.ru: a non-empty
// baseURL stands in for the whole of cbr.ru and therefore serves both
// endpoints, which a test server tells apart by query parameters (date_req
// vs. VAL_NM_RQ) rather than by path.
func New(client *http.Client, baseURL string) *Client {
	if client == nil {
		client = http.DefaultClient
	}
	if baseURL == "" {
		return &Client{http: client, dailyURL: DefaultBaseURL, dynamicURL: DefaultDynamicURL}
	}
	return &Client{http: client, dailyURL: baseURL, dynamicURL: baseURL}
}

// Name implements marketdata.FxProvider.
func (c *Client) Name() string { return sourceName }

// valCurs mirrors the root element of cbr.ru's daily rates XML response.
type valCurs struct {
	XMLName xml.Name `xml:"ValCurs"`
	Date    string   `xml:"Date,attr"`
	Valutes []valute `xml:"Valute"`
}

// valute mirrors a single <Valute> element. Value is left as a raw string
// here (rather than a decimal) because it arrives comma-decimal ("92,5678")
// and needs normalizing before it can be parsed.
//
// ID is the Bank of Russia's own internal identifier for the currency (e.g.
// "R01235" for USD). It is opaque: it is not always "R" followed by digits
// (Turkish lira is "R01700J"), so it must only be looked up, never parsed or
// reconstructed.
type valute struct {
	ID       string `xml:"ID,attr"`
	CharCode string `xml:"CharCode"`
	Nominal  int    `xml:"Nominal"`
	Value    string `xml:"Value"`
}

// valCursRange mirrors the root element of XML_dynamic.asp's response: one
// currency, one <Record> per day the bank published a rate on. It shares the
// <ValCurs> element name with the daily document but nothing else — the
// currency is named only by the bank's internal ID attribute, there is no ISO
// code anywhere in it, and there are no records at all for days the bank did
// not publish on.
//
// ID is the bank's internal identifier for the currency this whole series
// belongs to (e.g. "R01235" for USD) — the only thing in the response that
// says which currency was actually returned. The daily document has no such
// attribute on its root element at all, so a response decoded from the wrong
// endpoint also surfaces here as an empty ID.
type valCursRange struct {
	XMLName xml.Name     `xml:"ValCurs"`
	ID      string       `xml:"ID,attr"`
	Records []rateRecord `xml:"Record"`
}

// rateRecord mirrors a single <Record> element. Nominal sits inside every
// record, not once per document, and does change over a long enough series
// (the bank re-scales how many units it quotes a currency in), so each
// record's value must be divided by its own.
//
// Value arrives comma-decimal, exactly as in the daily document.
type rateRecord struct {
	Date    string `xml:"Date,attr"`
	Nominal int    `xml:"Nominal"`
	Value   string `xml:"Value"`
}

// fetchXML GETs reqURL and decodes the windows-1251 XML body into dst.
// Shared by every endpoint this client talks to; what counts as an
// empty-but-valid document differs per endpoint and is left to the callers.
func (c *Client) fetchXML(ctx context.Context, reqURL string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("cbr: build request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cbr: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cbr: unexpected status %d", resp.StatusCode)
	}

	dec := xml.NewDecoder(resp.Body)
	dec.CharsetReader = charsetReader
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("cbr: decode xml: %w", err)
	}

	return nil
}

// fetchDaily requests and parses the daily rates document for on's date. It
// is shared by RatesOn and CurrencyIDs, which both start from the same
// document and both treat a response with zero currencies as an error: an
// empty <ValCurs> is not a "no rates today" signal from cbr.ru (which
// instead falls back to the most recent business day), it indicates a
// malformed or unexpected response.
func (c *Client) fetchDaily(ctx context.Context, on time.Time) (valCurs, error) {
	var doc valCurs
	if err := c.fetchXML(ctx, c.dailyURL+"?date_req="+on.Format(dateLayout), &doc); err != nil {
		return valCurs{}, err
	}

	if len(doc.Valutes) == 0 {
		return valCurs{}, fmt.Errorf("cbr: response has no currencies")
	}

	return doc, nil
}

// RatesOn implements marketdata.FxProvider. It requests the rates for date
// on, but the returned FxRate.On is taken from the response's Date
// attribute rather than from on: cbr.ru does not publish rates on weekends
// and holidays, and silently returns the most recent business day's data
// instead.
func (c *Client) RatesOn(ctx context.Context, on time.Time) ([]marketdata.FxRate, error) {
	doc, err := c.fetchDaily(ctx, on)
	if err != nil {
		return nil, err
	}

	respDate, err := time.Parse(dateLayout, doc.Date)
	if err != nil {
		return nil, fmt.Errorf("cbr: parse response date %q: %w", doc.Date, err)
	}

	rates := make([]marketdata.FxRate, 0, len(doc.Valutes))
	for _, v := range doc.Valutes {
		rate, err := parseRate(v.Value, v.Nominal, v.CharCode)
		if err != nil {
			return nil, err
		}
		rates = append(rates, marketdata.FxRate{
			Base:   v.CharCode,
			Quote:  "RUB",
			On:     respDate,
			Rate:   rate,
			Source: sourceName,
		})
	}

	return rates, nil
}

// CurrencyIDs returns the mapping from ISO currency code (e.g. "USD") to the
// Bank of Russia's internal currency identifier (e.g. "R01235"), read from
// today's daily rates document. It is the identifier XML_dynamic.asp needs
// to fetch a currency's historical range, since that endpoint identifies
// currencies by the bank's own code rather than by ISO code.
//
// A currency the Bank of Russia does not currently quote is simply absent
// from the returned map; that is not an error.
func (c *Client) CurrencyIDs(ctx context.Context) (map[string]string, error) {
	doc, err := c.fetchDaily(ctx, time.Now())
	if err != nil {
		return nil, err
	}

	ids := make(map[string]string, len(doc.Valutes))
	for _, v := range doc.Valutes {
		ids[v.CharCode] = v.ID
	}

	return ids, nil
}

// RatesRange implements marketdata.FxHistoryProvider: it fetches one
// currency's whole published series between from and to (both ends included)
// in a single request, where RatesOn would need one request per day. It
// returns an error if to is before from, rather than silently sending a
// request cbr.ru would answer with an empty (but valid) series.
//
// currencyID is the Bank of Russia's internal identifier from CurrencyIDs;
// code is the ISO code the same currency is known by. Both are needed because
// the request accepts only the internal identifier while the response carries
// neither code, so FxRate.Base can only come from the caller. The response's
// root ID attribute is checked against currencyID, so a series for the wrong
// currency (or a response from the wrong endpoint) is an error rather than
// silently accepted; the ISO code itself cannot be cross-checked, since the
// response never carries one.
//
// Each FxRate.On is read from its own record, and each rate is divided by its
// own record's Nominal — the bank re-scales how many units it quotes a
// currency in, so one series can span several nominals. Days the bank did not
// publish on (weekends, holidays) have no record and are left missing rather
// than carried forward; resolving a date to the nearest earlier rate is the
// storage layer's job.
func (c *Client) RatesRange(ctx context.Context, code, currencyID string, from, to time.Time) ([]marketdata.FxRate, error) {
	if to.Before(from) {
		return nil, fmt.Errorf("cbr: invalid range for %s: to (%s) is before from (%s)",
			code, to.Format(dateLayout), from.Format(dateLayout))
	}

	reqURL := c.dynamicURL +
		"?date_req1=" + from.Format(rangeDateLayout) +
		"&date_req2=" + to.Format(rangeDateLayout) +
		"&VAL_NM_RQ=" + url.QueryEscape(currencyID)

	var doc valCursRange
	if err := c.fetchXML(ctx, reqURL, &doc); err != nil {
		return nil, err
	}

	// The response's root ID is the only thing that says which currency was
	// actually returned. If it does not match what was requested — whether
	// because the bank sent the wrong series or because this request landed
	// on the daily endpoint instead (whose root carries no ID at all, i.e.
	// ""), that must be a loud error rather than a silently-accepted series
	// for the wrong currency.
	if doc.ID != currencyID {
		return nil, fmt.Errorf("cbr: response ID %q does not match requested currency %s (%s)", doc.ID, currencyID, code)
	}

	// No records is a legitimate answer here, unlike in the daily document:
	// it means the bank quoted nothing for this currency in this range.
	rates := make([]marketdata.FxRate, 0, len(doc.Records))
	for _, rec := range doc.Records {
		on, err := time.Parse(dateLayout, rec.Date)
		if err != nil {
			return nil, fmt.Errorf("cbr: parse record date %q for %s: %w", rec.Date, code, err)
		}
		rate, err := parseRate(rec.Value, rec.Nominal, code)
		if err != nil {
			return nil, err
		}
		rates = append(rates, marketdata.FxRate{
			Base:   code,
			Quote:  "RUB",
			On:     on,
			Rate:   rate,
			Source: sourceName,
		})
	}

	return rates, nil
}

// parseRate normalizes raw's comma decimal separator into a decimal.Decimal
// and divides by nominal, so the result is "RUB per 1 unit of the currency"
// regardless of whether cbr.ru quotes it per 1, 10, 100, or 1000 units.
// currency only labels errors. Both feeds carry Value/Nominal pairs in the
// same shape, so both go through this one function.
func parseRate(raw string, nominal int, currency string) (decimal.Decimal, error) {
	value, err := decimal.NewFromString(strings.ReplaceAll(raw, ",", "."))
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("cbr: parse value %q for %s: %w", raw, currency, err)
	}
	if nominal <= 0 {
		// A missing <Nominal> unmarshals to zero, and substituting 1 for it
		// would publish a plausible number rather than refuse: the bank quotes
		// KZT per 100 units and several currencies per 1000 or 10000, so the
		// substitution inflates those rates by exactly that factor, silently,
		// into fx_rates and from there into balances, valuations, cost basis
		// and realized profit. Nothing downstream can tell such a rate from a
		// real one — it is an ordinary-looking number — which is why this is an
		// error and not a default. A non-positive nominal is refused by the
		// same rule: it cannot be a divisor, and inventing one would be the
		// same lie by another route.
		return decimal.Decimal{}, fmt.Errorf("cbr: nominal %d for %s: the feed did not say how many units this rate is quoted per", nominal, currency)
	}
	return value.Div(decimal.NewFromInt(int64(nominal))), nil
}

// charsetReader decodes windows-1251, the encoding cbr.ru declares (and
// actually uses) for XML_daily.asp. encoding/xml only understands UTF-8 and
// US-ASCII out of the box, so a CharsetReader must be wired into the decoder
// explicitly for anything else.
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(charset) {
	case "windows-1251", "cp1251":
		return charmap.Windows1251.NewDecoder().Reader(input), nil
	case "utf-8", "us-ascii", "":
		return input, nil
	default:
		return nil, fmt.Errorf("cbr: unsupported charset %q", charset)
	}
}
