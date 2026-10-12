// Package cbr reads the Bank of Russia's FX rates — the daily document
// (XML_daily.asp) and the date-range series (XML_dynamic.asp) — and its
// accounting prices of precious metals (xml_metall.asp).
package cbr

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"golang.org/x/text/encoding/charmap"

	"babki.my/babki/internal/marketdata"
)

// DefaultBaseURL is the Bank of Russia's daily FX rates endpoint.
const DefaultBaseURL = "https://www.cbr.ru/scripts/XML_daily.asp"

// DefaultDynamicURL returns one currency's rates over a date range.
const DefaultDynamicURL = "https://www.cbr.ru/scripts/XML_dynamic.asp"

// DefaultMetalsURL returns the metals' accounting prices over a date range.
const DefaultMetalsURL = "https://www.cbr.ru/scripts/xml_metall.asp"

// DefaultKeyRateURL is the key rate's page: a table of the rate by day over
// a date range. The bank publishes it as a page, not a feed.
const DefaultKeyRateURL = "https://www.cbr.ru/hd_base/KeyRate/"

// userAgent identifies this program; the bank refuses Go's default agent.
const userAgent = "babki.my/1.0 (+https://github.com/alshstf/babki.my)"

// sourceName is the provider's Name and every rate's Source.
const sourceName = "cbr"

// dateLayout is cbr.ru's date format in requests and responses.
const dateLayout = "02.01.2006"

// rangeDateLayout is XML_dynamic.asp's slash-separated parameter format.
const rangeDateLayout = "02/01/2006"

// Client fetches and parses the Bank of Russia's rate feeds.
type Client struct {
	http       *http.Client
	dailyURL   string
	dynamicURL string
	metalsURL  string
	keyRateURL string
}

// New returns a Client. A nil client means http.DefaultClient; a non-empty
// baseURL stands in for all of cbr.ru (tests tell the endpoints apart by query
// parameters).
func New(client *http.Client, baseURL string) *Client {
	if client == nil {
		client = http.DefaultClient
	}
	if baseURL == "" {
		return &Client{http: client, dailyURL: DefaultBaseURL, dynamicURL: DefaultDynamicURL, metalsURL: DefaultMetalsURL, keyRateURL: DefaultKeyRateURL}
	}
	return &Client{http: client, dailyURL: baseURL, dynamicURL: baseURL, metalsURL: baseURL, keyRateURL: baseURL}
}

// Name implements marketdata.FxProvider.
func (c *Client) Name() string { return sourceName }

// valCurs mirrors the root element of cbr.ru's daily rates XML response.
type valCurs struct {
	XMLName xml.Name `xml:"ValCurs"`
	Date    string   `xml:"Date,attr"`
	Valutes []valute `xml:"Valute"`
}

// valute is one <Valute>. Value is comma-decimal. ID is the bank's opaque
// currency identifier ("R01235", "R01700J"): look it up, never parse it.
type valute struct {
	ID       string `xml:"ID,attr"`
	CharCode string `xml:"CharCode"`
	Nominal  int    `xml:"Nominal"`
	Value    string `xml:"Value"`
}

// valCursRange is XML_dynamic.asp's root: one currency, named only by the
// bank's ID, with a record per published day. The daily document's root has
// no ID, so a response from the wrong endpoint shows as an empty ID.
type valCursRange struct {
	XMLName xml.Name     `xml:"ValCurs"`
	ID      string       `xml:"ID,attr"`
	Records []rateRecord `xml:"Record"`
}

// rateRecord is one <Record>. Each carries its own Nominal, which changes over
// long series.
type rateRecord struct {
	Date    string `xml:"Date,attr"`
	Nominal int    `xml:"Nominal"`
	Value   string `xml:"Value"`
}

// fetchXML GETs reqURL and decodes the windows-1251 XML body into dst.
func (c *Client) fetchXML(ctx context.Context, reqURL string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("cbr: build request: %w", err)
	}
	// The bank answers Go's default agent with 403 (checked 2026-08-21). Without
	// this header the rate table held eleven days and most base-currency figures
	// were missing.
	req.Header.Set("User-Agent", userAgent)

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

// fetchDaily fetches the daily document. An empty one is an error: cbr.ru
// answers a non-business day with the latest business day's rates.
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

// RatesOn implements marketdata.FxProvider. FxRate.On is the response's own
// date, since cbr.ru answers weekends and holidays with the latest business
// day.
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

// CurrencyIDs maps ISO codes to the bank's identifiers, which XML_dynamic.asp
// requires. A currency the bank does not quote is absent.
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

// RatesRange implements marketdata.FxHistoryProvider: one currency's series
// over [from, to] in one request. The request takes the bank's identifier and
// the response carries no ISO code, so code labels the rates; the response's
// ID is checked against currencyID. Each rate is divided by its own record's
// nominal. Days without a record are left missing.
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

	// The root ID is all that says which currency came back; an empty one means
	// the daily endpoint answered.
	if doc.ID != currencyID {
		return nil, fmt.Errorf("cbr: response ID %q does not match requested currency %s (%s)", doc.ID, currencyID, code)
	}

	// No records is a valid answer here: nothing quoted in the range.
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

// metall is xml_metall.asp's root: a record per metal and published day.
type metall struct {
	XMLName xml.Name      `xml:"Metall"`
	Records []metalRecord `xml:"Record"`
}

// metalRecord is one <Record>: Code 1 gold, 2 silver, 3 platinum, 4
// palladium; Buy and Sell are both the accounting price, roubles per gram.
type metalRecord struct {
	Date string `xml:"Date,attr"`
	Code int    `xml:"Code,attr"`
	Buy  string `xml:"Buy"`
}

// metalCodes are the bank's metal codes.
var metalCodes = map[int]marketdata.Metal{1: marketdata.Gold, 2: marketdata.Silver, 3: marketdata.Platinum, 4: marketdata.Palladium}

// MetalPrices implements marketdata.MetalPriceProvider: the four metals'
// accounting prices over [from, to] in one request. Days without a record
// (weekends, holidays) are left missing; a code the bank adds is skipped.
func (c *Client) MetalPrices(ctx context.Context, from, to time.Time) ([]marketdata.MetalPrice, error) {
	if to.Before(from) {
		return nil, fmt.Errorf("cbr: invalid range for metals: to (%s) is before from (%s)", to.Format(dateLayout), from.Format(dateLayout))
	}
	var doc metall
	if err := c.fetchXML(ctx, c.metalsURL+"?date_req1="+from.Format(rangeDateLayout)+"&date_req2="+to.Format(rangeDateLayout), &doc); err != nil {
		return nil, err
	}
	out := make([]marketdata.MetalPrice, 0, len(doc.Records))
	for _, rec := range doc.Records {
		metal, ok := metalCodes[rec.Code]
		if !ok {
			continue
		}
		on, err := time.Parse(dateLayout, rec.Date)
		if err != nil {
			return nil, fmt.Errorf("cbr: parse metal record date %q: %w", rec.Date, err)
		}
		price, err := parseRate(rec.Buy, 1, string(metal))
		if err != nil {
			return nil, err
		}
		if !price.IsPositive() {
			return nil, fmt.Errorf("cbr: %s on %s priced at %s", metal, rec.Date, price)
		}
		out = append(out, marketdata.MetalPrice{Metal: metal, On: on, Price: price, Source: sourceName})
	}
	return out, nil
}

// keyRateTable finds the page's table and keyRateRow its rows: a day and the
// rate in force, comma-decimal.
var (
	keyRateTable = regexp.MustCompile(`<table class="data">`)
	keyRateRow   = regexp.MustCompile(`<td>(\d{2}\.\d{2}\.\d{4})</td>\s*<td>(\d+(?:,\d+)?)</td>`)
)

// KeyRates implements marketdata.KeyRateProvider: the key rate in force on
// each business day of [from, to], from the bank's page. A page without the
// table is an error — its layout changed — so the source shows as failing.
func (c *Client) KeyRates(ctx context.Context, from, to time.Time) ([]marketdata.KeyRate, error) {
	if to.Before(from) {
		return nil, fmt.Errorf("cbr: invalid range for the key rate: to (%s) is before from (%s)", to.Format(dateLayout), from.Format(dateLayout))
	}
	reqURL := c.keyRateURL + "?UniDbQuery.Posted=True&UniDbQuery.From=" + from.Format(dateLayout) + "&UniDbQuery.To=" + to.Format(dateLayout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cbr: build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cbr: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cbr: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("cbr: read the key rate page: %w", err)
	}
	if !keyRateTable.Match(body) {
		return nil, fmt.Errorf("cbr: the key rate page has no table of rates")
	}
	var out []marketdata.KeyRate
	for _, m := range keyRateRow.FindAllSubmatch(body, -1) {
		on, err := time.Parse(dateLayout, string(m[1]))
		if err != nil {
			return nil, fmt.Errorf("cbr: parse key rate date %q: %w", m[1], err)
		}
		rate, err := decimal.NewFromString(strings.ReplaceAll(string(m[2]), ",", "."))
		if err != nil || !rate.IsPositive() {
			return nil, fmt.Errorf("cbr: key rate %q on %s", m[2], m[1])
		}
		out = append(out, marketdata.KeyRate{On: on, Rate: rate, Source: sourceName})
	}
	return out, nil
}

// parseRate turns a comma-decimal value into rubles per one unit, dividing by
// the nominal.
func parseRate(raw string, nominal int, currency string) (decimal.Decimal, error) {
	value, err := decimal.NewFromString(strings.ReplaceAll(raw, ",", "."))
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("cbr: parse value %q for %s: %w", raw, currency, err)
	}
	if nominal <= 0 {
		// A missing nominal unmarshals to zero. Assuming 1 would inflate rates quoted
		// per 100 or 1000 units (KZT and others) into plausible wrong numbers, so it is
		// an error.
		return decimal.Decimal{}, fmt.Errorf("cbr: nominal %d for %s: the feed did not say how many units this rate is quoted per", nominal, currency)
	}
	return value.Div(decimal.NewFromInt(int64(nominal))), nil
}

// charsetReader decodes windows-1251, which encoding/xml does not support on
// its own.
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
