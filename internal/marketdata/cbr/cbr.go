// Package cbr implements marketdata.FxProvider against the Bank of Russia's
// daily FX rate feed (XML_daily.asp).
package cbr

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"golang.org/x/text/encoding/charmap"

	"babki.my/babki/internal/marketdata"
)

// DefaultBaseURL is the Bank of Russia's daily FX rates endpoint.
const DefaultBaseURL = "https://www.cbr.ru/scripts/XML_daily.asp"

// sourceName is used both as the provider's Name() and as FxRate.Source for
// every rate this provider returns.
const sourceName = "cbr"

// dateLayout is the format cbr.ru uses for both the ?date_req= request
// parameter and the <ValCurs Date="..."> response attribute.
const dateLayout = "02.01.2006"

// Client fetches and parses the Bank of Russia's daily FX rate feed.
type Client struct {
	http    *http.Client
	baseURL string
}

// New returns a Client. client may be nil, in which case http.DefaultClient
// is used; baseURL may be empty, in which case DefaultBaseURL is used.
// baseURL is parameterized (rather than hardcoded) so tests can point it at
// an httptest.Server instead of the real cbr.ru.
func New(client *http.Client, baseURL string) *Client {
	if client == nil {
		client = http.DefaultClient
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{http: client, baseURL: baseURL}
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

// fetchDaily requests and parses the daily rates document for on's date. It
// is shared by RatesOn and CurrencyIDs, which both start from the same
// document and both treat a response with zero currencies as an error: an
// empty <ValCurs> is not a "no rates today" signal from cbr.ru (which
// instead falls back to the most recent business day), it indicates a
// malformed or unexpected response.
func (c *Client) fetchDaily(ctx context.Context, on time.Time) (valCurs, error) {
	url := c.baseURL + "?date_req=" + on.Format(dateLayout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return valCurs{}, fmt.Errorf("cbr: build request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return valCurs{}, fmt.Errorf("cbr: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return valCurs{}, fmt.Errorf("cbr: unexpected status %d", resp.StatusCode)
	}

	var doc valCurs
	dec := xml.NewDecoder(resp.Body)
	dec.CharsetReader = charsetReader
	if err := dec.Decode(&doc); err != nil {
		return valCurs{}, fmt.Errorf("cbr: decode xml: %w", err)
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
		rate, err := parseRate(v)
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

// parseRate normalizes v.Value's comma decimal separator into a decimal.Decimal
// and divides by Nominal, so the result is "RUB per 1 unit of the currency"
// regardless of whether cbr.ru quotes it per 1, 10, 100, or 1000 units.
func parseRate(v valute) (decimal.Decimal, error) {
	value, err := decimal.NewFromString(strings.ReplaceAll(v.Value, ",", "."))
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("cbr: parse value %q for %s: %w", v.Value, v.CharCode, err)
	}
	nominal := v.Nominal
	if nominal == 0 {
		// Defensive default: every real cbr.ru response sets Nominal, but a
		// missing element unmarshals to the zero value and would otherwise
		// divide by zero.
		nominal = 1
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
