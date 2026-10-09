package tinvest

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// DefaultBaseURL is the live T-Invest REST gateway.
const DefaultBaseURL = "https://invest-public-api.tinkoff.ru/rest"

// SandboxBaseURL is the sandbox gateway: the same API against simulated
// money.
const SandboxBaseURL = "https://sandbox-invest-public-api.tinkoff.ru/rest"

// ErrTokenInvalid means the broker rejected the token: HTTP 401 or business
// code 40003. The token needs replacing.
var ErrTokenInvalid = errors.New("tinvest: token invalid or revoked")

// ErrInstrumentNotFound means the broker has no such instrument: HTTP 404 or
// business code 50002 (live, 2026-08-05: GetInstrumentBy on an unknown uid
// answers 404 {"code":5,"message":"Instrument not found","description":"50002"}).
// A caller can refuse the one operation naming it and go on, which is the only
// way a delisted paper ever stops failing; other errors stay fatal. The wrapped
// error keeps the rpc, status and body.
var ErrInstrumentNotFound = errors.New("tinvest: the broker has no such instrument")

// errReportNotReady is the broker's "not built yet" for a broker report:
// HTTP 400, code 30058 (live, 2026-10-05). TradeSettlements waits it out.
var errReportNotReady = errors.New("tinvest: the broker report is not built yet")

// defaultHTTPTimeout bounds a single unary call well within an hourly sync's
// budget.
const defaultHTTPTimeout = 30 * time.Second

// russianTrustedRootCAPEM is the Russian Trusted Root CA, SHA-256 fingerprint
// D2:6D:2D:02:31:B7:C3:9F:92:CC:73:85:12:BA:54:10:35:19:E4:40:5D:68:B5:BD:70:3E:97:88:CA:8E:CF:31,
// verified 2026-08-04 as the root of a live chain from invest-public-api.tinkoff.ru
// (via "Russian Trusted Sub CA"). Expires 2032-02-27.
// TestEmbeddedCert_FingerprintMatchesWhatWasVerifiedOutOfBand checks it.
//
//go:embed russian_trusted_root_ca.pem
var russianTrustedRootCAPEM []byte

// NewHTTPClient returns a client that trusts the system pool plus the embedded
// Russian Trusted Root CA, on this transport only; the gateway's chain ends at
// that root, which system stores usually lack. timeout bounds every request.
func NewHTTPClient(timeout time.Duration) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		// No system pool on this platform: trust the embedded root alone rather
		// than fail to start.
		pool = x509.NewCertPool()
	}
	if ok := pool.AppendCertsFromPEM(russianTrustedRootCAPEM); !ok {
		return nil, errors.New("tinvest: embedded Russian Trusted Root CA did not parse")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool}

	return &http.Client{Timeout: timeout, Transport: transport}, nil
}

// sleepFunc waits d or until ctx ends; a field so tests skip the 429 wait.
type sleepFunc func(ctx context.Context, d time.Duration) error

// ctxSleep is the default sleepFunc: a real, context-aware wait.
func ctxSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Client is a thin REST client for the T-Invest API: it builds requests,
// parses responses and retries once on a rate limit. No state, no cache.
type Client struct {
	http    *http.Client
	baseURL string
	token   string
	log     *slog.Logger
	// sleep is the 429 backoff wait; tests in this package replace it.
	sleep sleepFunc
}

// NewClient builds a Client. A nil hc means NewHTTPClient(defaultHTTPTimeout),
// an empty baseURL DefaultBaseURL, a nil log slog.Default.
func NewClient(hc *http.Client, baseURL, token string, log *slog.Logger) *Client {
	if hc == nil {
		var err error
		hc, err = NewHTTPClient(defaultHTTPTimeout)
		if err != nil {
			// Only an unparsable embedded certificate fails here, and the cert tests
			// rule that out on every build; a client without the root would fail
			// every handshake later and less clearly.
			panic(err)
		}
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if log == nil {
		log = slog.Default()
	}
	return &Client{http: hc, baseURL: baseURL, token: token, log: log, sleep: ctxSleep}
}

// MoneyValue is an amount with a currency (the wire's units + nano); Currency
// is upper case.
type MoneyValue struct {
	Currency string
	Units    int64
	Nano     int32
}

// Decimal returns Units + Nano·10⁻⁹ by adding the two: the wire may sign them
// separately ({0, -200000000} is -0.2), which text concatenation gets wrong.
func (m MoneyValue) Decimal() decimal.Decimal {
	return decimal.NewFromInt(m.Units).Add(decimal.New(int64(m.Nano), -9))
}

// Quotation is a bare decimal quantity (the wire's units + nano).
type Quotation struct {
	Units int64
	Nano  int32
}

// Decimal returns q as a decimal; see MoneyValue.Decimal.
func (q Quotation) Decimal() decimal.Decimal {
	return decimal.NewFromInt(q.Units).Add(decimal.New(int64(q.Nano), -9))
}

// Account is one of the caller's accounts (UsersService/GetAccounts). Type and
// Status are wire strings as sent; OpenedOn is nil when absent.
type Account struct {
	ID, Name, Type, Status string
	OpenedOn               *time.Time
}

// GetAccounts calls UsersService/GetAccounts and returns every account the
// token can see, unfiltered by status.
func (c *Client) GetAccounts(ctx context.Context) ([]Account, error) {
	var resp wireGetAccountsResponse
	if err := c.do(ctx, "UsersService/GetAccounts", struct{}{}, &resp); err != nil {
		return nil, err
	}
	accounts := make([]Account, 0, len(resp.Accounts))
	for _, w := range resp.Accounts {
		acc, err := w.parse()
		if err != nil {
			return nil, fmt.Errorf("tinvest: UsersService/GetAccounts: %w", err)
		}
		accounts = append(accounts, acc)
	}
	return accounts, nil
}

// OperationItem is one entry of GetOperationsByCursor. Type and State are wire
// strings as sent; Raw is the element's own bytes, kept by the mirror.
type OperationItem struct {
	ID, ParentOperationID, Type, State                         string
	Date                                                       time.Time
	InstrumentUID, FIGI, PositionUID, AssetUID, InstrumentType string
	Payment, Commission, AccruedInt, Price                     MoneyValue
	// Quantity is the order's size and QuantityDone what was filled; they
	// differ on partial fills (an order of 11100 against a fill of 6644). A
	// trade takes its units from QuantityDone, which its money divides by
	// (#131). A transfer is not an order: both fields match and the projection
	// reads Quantity, whose sign gives the direction. A missing QuantityDone is
	// zero and the trade is refused (see projectTrade).
	Quantity, QuantityDone int64
	// Ticker is what the operation calls the paper. Needed only for an
	// instrument the broker has forgotten (passport 404), where it carries the
	// ISIN (see Resolver.resolveOne).
	Ticker string
	// ClassCode is the trading mode: the board ("TQBR", "TQCB") or an
	// off-book venue ("FINEX_OTC"). Empty means the broker sent none, as for
	// money in and out; it is stored as is (see tradingMode).
	ClassCode   string
	Description string
	Raw         json.RawMessage
}

// operationsPageLimit is the documented maximum page; limit <= 2 is
// documented to duplicate rows.
const operationsPageLimit = 1000

// OperationsAll walks GetOperationsByCursor to the end from from (zero: the
// whole history) and returns every operation in every state. Deduplication is
// the mirror's job.
func (c *Client) OperationsAll(ctx context.Context, brokerAccountID string, from time.Time) ([]OperationItem, error) {
	var all []OperationItem
	cursor := ""
	// Every requested cursor is remembered, so a cycle of any length is caught
	// on the request that would repeat it.
	seenCursors := map[string]bool{cursor: true}
	for {
		req := getOperationsByCursorRequest{
			AccountID: brokerAccountID,
			Cursor:    cursor,
			Limit:     operationsPageLimit,
		}
		if !from.IsZero() {
			req.From = from.UTC().Format(time.RFC3339Nano)
		}

		var resp wireGetOperationsByCursorResponse
		if err := c.do(ctx, "OperationsService/GetOperationsByCursor", req, &resp); err != nil {
			return nil, err
		}

		for _, raw := range resp.Items {
			var wi wireOperationItem
			if err := json.Unmarshal(raw, &wi); err != nil {
				return nil, fmt.Errorf("tinvest: OperationsService/GetOperationsByCursor: decode item: %w", err)
			}
			item, err := wi.parse(raw)
			if err != nil {
				return nil, fmt.Errorf("tinvest: OperationsService/GetOperationsByCursor: %w", err)
			}
			all = append(all, item)
		}

		if !resp.HasNext {
			return all, nil
		}
		if seenCursors[resp.NextCursor] {
			// A repeated cursor would loop forever; fail loudly instead.
			return nil, fmt.Errorf(
				"tinvest: OperationsService/GetOperationsByCursor: hasNext=true but cursor %q repeats an earlier page",
				resp.NextCursor)
		}
		seenCursors[resp.NextCursor] = true
		cursor = resp.NextCursor
	}
}

// PortfolioPosition is one holding from GetPortfolio (proto comments checked
// 2026-08-05):
//
//   - Quantity is the whole position, including units reserved by open sell
//     orders (reserved lots are a separate field not decoded here).
//   - Blocked is a bool: the depository has halted the paper. Not a quantity.
//
// InstrumentType is the broker's word ("share", "bond", "etf"; see
// brokerInstrumentTypes) and is not always a security: a cash-only sandbox account
// returned one position of type "currency" (testdata/portfolio_cash_only.json),
// so compareInstruments reads it first. Ticker is empty when absent.
type PortfolioPosition struct {
	InstrumentUID, FIGI, InstrumentType, Ticker string
	Quantity                                    Quotation
	Blocked                                     bool
}

// Portfolio is OperationsService/GetPortfolio's answer: the account's positions
// and what the broker says the whole account is worth.
type Portfolio struct {
	Positions []PortfolioPosition
	// Total is totalAmountPortfolio, securities at the broker's prices plus
	// cash, asked for in roubles (present on the owner's account, 2026-10-02).
	// Nil when absent.
	Total *MoneyValue
}

// GetPortfolio calls OperationsService/GetPortfolio and returns the account's
// current positions and its total in rubles.
func (c *Client) GetPortfolio(ctx context.Context, brokerAccountID string) (Portfolio, error) {
	var resp wireGetPortfolioResponse
	req := portfolioRequest{AccountID: brokerAccountID, Currency: rubCode}
	if err := c.do(ctx, "OperationsService/GetPortfolio", req, &resp); err != nil {
		return Portfolio{}, err
	}
	positions := make([]PortfolioPosition, 0, len(resp.Positions))
	for _, w := range resp.Positions {
		p, err := w.parse()
		if err != nil {
			return Portfolio{}, fmt.Errorf("tinvest: OperationsService/GetPortfolio: %w", err)
		}
		positions = append(positions, p)
	}
	out := Portfolio{Positions: positions}
	if resp.TotalAmountPortfolio != nil {
		total, err := resp.TotalAmountPortfolio.parse()
		if err != nil {
			return Portfolio{}, fmt.Errorf("tinvest: OperationsService/GetPortfolio: totalAmountPortfolio: %w", err)
		}
		out.Total = &total
	}
	return out, nil
}

// MoneyBalance is one currency's cash from GetPositions: Value free,
// Blocked held by open orders.
type MoneyBalance struct {
	Currency       string
	Value, Blocked decimal.Decimal
}

// GetPositions returns one MoneyBalance per currency in either the money or the
// blocked list (the other side zero), sorted by currency.
func (c *Client) GetPositions(ctx context.Context, brokerAccountID string) ([]MoneyBalance, error) {
	var resp wireGetPositionsResponse
	if err := c.do(ctx, "OperationsService/GetPositions", accountIDRequest{AccountID: brokerAccountID}, &resp); err != nil {
		return nil, err
	}

	byCurrency := make(map[string]*MoneyBalance)
	get := func(currency string) *MoneyBalance {
		b, ok := byCurrency[currency]
		if !ok {
			b = &MoneyBalance{Currency: currency}
			byCurrency[currency] = b
		}
		return b
	}
	for _, w := range resp.Money {
		mv, err := w.parse()
		if err != nil {
			return nil, fmt.Errorf("tinvest: OperationsService/GetPositions: money: %w", err)
		}
		get(mv.Currency).Value = mv.Decimal()
	}
	for _, w := range resp.Blocked {
		mv, err := w.parse()
		if err != nil {
			return nil, fmt.Errorf("tinvest: OperationsService/GetPositions: blocked: %w", err)
		}
		get(mv.Currency).Blocked = mv.Decimal()
	}

	currencies := make([]string, 0, len(byCurrency))
	for currency := range byCurrency {
		currencies = append(currencies, currency)
	}
	sort.Strings(currencies)

	balances := make([]MoneyBalance, 0, len(currencies))
	for _, currency := range currencies {
		balances = append(balances, *byCurrency[currency])
	}
	return balances, nil
}

// InstrumentBrief is GetInstrumentBy's answer, trimmed. It has no nominal:
// GetInstrumentBy supplies none (see BondNominalByUID).
type InstrumentBrief struct {
	UID, FIGI, ISIN, Ticker, Name, Currency, InstrumentType string
}

// InstrumentByUID calls InstrumentsService/GetInstrumentBy with
// id_type=INSTRUMENT_ID_TYPE_UID.
func (c *Client) InstrumentByUID(ctx context.Context, uid string) (InstrumentBrief, error) {
	req := instrumentByRequest{IDType: instrumentIDTypeUID, ID: uid}
	var resp wireInstrumentResponse
	if err := c.do(ctx, "InstrumentsService/GetInstrumentBy", req, &resp); err != nil {
		return InstrumentBrief{}, err
	}

	return InstrumentBrief{
		UID:            resp.Instrument.UID,
		FIGI:           resp.Instrument.FIGI,
		ISIN:           resp.Instrument.ISIN,
		Ticker:         resp.Instrument.Ticker,
		Name:           resp.Instrument.Name,
		Currency:       strings.ToUpper(resp.Instrument.Currency),
		InstrumentType: resp.Instrument.InstrumentType,
	}, nil
}

// Listing is one of the broker's tradable lines for a security; one paper has
// several (Apple's ISIN: nine, under three tickers). It has no currency because
// the search result has none (live, 2026-08-10); ask the passport.
type Listing struct {
	UID, ISIN, Ticker, Name, ClassCode, Kind string
}

// FindInstruments searches the broker's catalog, here always by ISIN: a ticker
// is not unique ("T" is one issuer's bond and another's share). It returns every
// listing; choosing is the caller's.
func (c *Client) FindInstruments(ctx context.Context, query string) ([]Listing, error) {
	req := struct {
		Query string `json:"query"`
	}{Query: query}
	var resp wireFindInstrumentResponse
	if err := c.do(ctx, "InstrumentsService/FindInstrument", req, &resp); err != nil {
		return nil, err
	}
	out := make([]Listing, 0, len(resp.Instruments))
	for _, w := range resp.Instruments {
		out = append(out, Listing{
			UID: w.UID, ISIN: w.ISIN, Ticker: w.Ticker, Name: w.Name,
			ClassCode: w.ClassCode, Kind: w.InstrumentKind,
		})
	}
	return out, nil
}

// CurrencyNominalByUID calls CurrencyBy and returns which money one unit of a
// currency instrument is and how much: a trade row names only its payment
// currency, and a unit is not always one (Kyrgyz som: 100, Uzbek sum: 10 000;
// live, 2026-08-05). The call shape is by analogy with BondBy; if wrong, the trade
// becomes a visible unparsed row. A zero or currency-less nominal is returned for
// the caller to refuse (Resolver.ResolveCurrency).
func (c *Client) CurrencyNominalByUID(ctx context.Context, uid string) (MoneyValue, error) {
	req := instrumentByRequest{IDType: instrumentIDTypeUID, ID: uid}
	var resp wireCurrencyResponse
	if err := c.do(ctx, "InstrumentsService/CurrencyBy", req, &resp); err != nil {
		return MoneyValue{}, err
	}
	nominal, err := resp.Instrument.Nominal.parse()
	if err != nil {
		return MoneyValue{}, fmt.Errorf("tinvest: InstrumentsService/CurrencyBy: nominal: %w", err)
	}
	return nominal, nil
}

func (c *Client) bondBy(ctx context.Context, uid string) (wireBondResponse, error) {
	var resp wireBondResponse
	err := c.do(ctx, "InstrumentsService/BondBy", instrumentByRequest{IDType: instrumentIDTypeUID, ID: uid}, &resp)
	return resp, err
}

// BondTermsByUID is a bond's current nominal and the coupon interest accrued
// on it, as BondBy states them; a redeemed bond's nominal is 0.
func (c *Client) BondTermsByUID(ctx context.Context, uid string) (nominal, accrued MoneyValue, err error) {
	resp, err := c.bondBy(ctx, uid)
	if err != nil {
		return MoneyValue{}, MoneyValue{}, err
	}
	if nominal, err = resp.Instrument.Nominal.parse(); err != nil {
		return MoneyValue{}, MoneyValue{}, fmt.Errorf("tinvest: InstrumentsService/BondBy: nominal: %w", err)
	}
	if accrued, err = resp.Instrument.AciValue.parse(); err != nil {
		return MoneyValue{}, MoneyValue{}, fmt.Errorf("tinvest: InstrumentsService/BondBy: accrued interest: %w", err)
	}
	return nominal, accrued, nil
}

// BondNominalByUID calls BondBy and returns the bond's nominal, which
// GetInstrumentBy lacks (live shape: "nominal" under "instrument", in the
// sandbox).
func (c *Client) BondNominalByUID(ctx context.Context, uid string) (MoneyValue, error) {
	resp, err := c.bondBy(ctx, uid)
	if err != nil {
		return MoneyValue{}, err
	}

	nominal, err := resp.Instrument.Nominal.parse()
	if err != nil {
		return MoneyValue{}, fmt.Errorf("tinvest: InstrumentsService/BondBy: nominal: %w", err)
	}
	if !nominal.Decimal().IsZero() {
		return nominal, nil
	}

	// A redeemed bond reports nominal 0 and keeps initialNominal (live:
	// Быстроденьги Ю002Р-01 and ОФЗ 29014 answer 0 with 100 CNY and 1000 RUB;
	// Казахстан 11 answers 1000 for both). The current nominal comes first:
	// an amortizing live bond's is smaller than the initial. The fallback is
	// reached only at zero, where nothing is left to overstate and the
	// history still needs a face value.
	initial, err := resp.Instrument.InitialNominal.parse()
	if err != nil {
		return MoneyValue{}, fmt.Errorf("tinvest: InstrumentsService/BondBy: initial nominal: %w", err)
	}
	return initial, nil
}

// TradeSettlement is one report trade and its settlement day. TradeID is the
// exchange's trade number, also on an operation's tradesInfo.trades[].num (all 18
// trades of a month matched on the owner's account, 2026-10-05). SettledOn is UTC
// midnight.
type TradeSettlement struct {
	TradeID   string
	TradedAt  time.Time
	SettledOn time.Time
}

// brokerReportRPC both orders a report and returns it, by request body.
const brokerReportRPC = "OperationsService/GetBrokerReport"

// brokerReportPollInterval: the method allows a handful of calls a minute
// (the fifth poll three seconds apart was limited), shared by ordering and
// polling.
const brokerReportPollInterval = 12 * time.Second

// brokerReportMaxPolls: two minutes; past that, the month is asked again on
// a later run.
const brokerReportMaxPolls = 10

// TradeSettlements returns the trades of [from, to) and their settlement days.
// The report is built asynchronously: the first request orders it and returns a
// task id, later ones page the finished report and answer errReportNotReady until
// then. The wait is c.sleep. A report built before comes back at once as its
// first page, with the task id for the rest.
func (c *Client) TradeSettlements(ctx context.Context, brokerAccountID string, from, to time.Time) ([]TradeSettlement, error) {
	var ordered wireGenerateBrokerReportResponse
	if err := c.do(ctx, brokerReportRPC, generateBrokerReportRequest{Generate: generateBrokerReport{
		AccountID: brokerAccountID,
		From:      from.UTC().Format(time.RFC3339),
		To:        to.UTC().Format(time.RFC3339),
	}}, &ordered); err != nil {
		return nil, err
	}
	taskID := ordered.Generate.TaskID
	if ordered.Ready != nil {
		taskID = ordered.Ready.TaskID
	}
	if taskID == "" && (ordered.Ready == nil || ordered.Ready.PagesCount > 1) {
		return nil, fmt.Errorf("tinvest: %s: the broker ordered a report and named no task to fetch it by", brokerReportRPC)
	}

	var out []TradeSettlement
	for page := 0; ; page++ {
		report, err := c.reportPage(ctx, ordered.Ready, taskID, page)
		if err != nil {
			return nil, err
		}
		for _, w := range report.Rows {
			s, ok, err := w.parse()
			if err != nil {
				return nil, fmt.Errorf("tinvest: %s: page %d: %w", brokerReportRPC, page, err)
			}
			if ok {
				out = append(out, s)
			}
		}
		if page+1 >= report.PagesCount {
			return out, nil
		}
	}
}

// reportPage is page of the report: the one the order handed back at once,
// or one asked for by task.
func (c *Client) reportPage(ctx context.Context, ready *wireBrokerReport, taskID string, page int) (wireBrokerReport, error) {
	if page == 0 && ready != nil {
		return *ready, nil
	}
	return c.brokerReportPage(ctx, taskID, page)
}

// brokerReportPage is one page of an ordered report, waiting while it is
// not built.
func (c *Client) brokerReportPage(ctx context.Context, taskID string, page int) (wireBrokerReport, error) {
	for poll := 1; ; poll++ {
		var resp wireGetBrokerReportResponse
		err := c.do(ctx, brokerReportRPC, getBrokerReportRequest{Get: getBrokerReport{TaskID: taskID, Page: page}}, &resp)
		if !errors.Is(err, errReportNotReady) {
			return resp.Get, err
		}
		if poll >= brokerReportMaxPolls {
			return wireBrokerReport{}, fmt.Errorf("tinvest: %s: report task %s still not built after %d polls: %w",
				brokerReportRPC, taskID, poll, err)
		}
		if err := c.sleep(ctx, brokerReportPollInterval); err != nil {
			return wireBrokerReport{}, fmt.Errorf("tinvest: %s: waiting for report task %s: %w", brokerReportRPC, taskID, err)
		}
	}
}

// DeclaredDividend is one dividend from a paper's calendar: the declared amount
// per share before tax, the record day and the days around it (Р-14). Days are UTC
// midnight, nil when absent.
type DeclaredDividend struct {
	PerShare    MoneyValue
	RecordDate  time.Time
	PaymentDate *time.Time
	LastBuyDate *time.Time
}

// Dividends returns a paper's dividends with a record date in [from, to).
// instrumentID is a uid or figi. The calendar is the broker's, not the account's,
// so one connection serves every account's papers. A dividend declared as zero (a
// cancelled one) is left out.
func (c *Client) Dividends(ctx context.Context, instrumentID string, from, to time.Time) ([]DeclaredDividend, error) {
	var resp wireGetDividendsResponse
	if err := c.do(ctx, "InstrumentsService/GetDividends", getDividendsRequest{
		InstrumentID: instrumentID,
		From:         from.UTC().Format(time.RFC3339),
		To:           to.UTC().Format(time.RFC3339),
	}, &resp); err != nil {
		return nil, err
	}
	out := make([]DeclaredDividend, 0, len(resp.Dividends))
	for _, w := range resp.Dividends {
		d, ok, err := w.parse()
		if err != nil {
			return nil, fmt.Errorf("tinvest: InstrumentsService/GetDividends(%s): %w", instrumentID, err)
		}
		if ok {
			out = append(out, d)
		}
	}
	return out, nil
}

// rateLimitError is a 429, caught by do to wait and retry.
type rateLimitError struct {
	resetAfter time.Duration
}

func (e *rateLimitError) Error() string {
	return fmt.Sprintf("tinvest: rate limited, reset in %s", e.resetAfter)
}

// rateLimitResetHeader is the seconds until the per-method limit resets.
const rateLimitResetHeader = "x-ratelimit-reset"

// maxRateLimitWait caps one 429 wait: limits reset per minute, plus a margin
// for skew; a garbled header must not sleep a sync for a day.
const maxRateLimitWait = 65 * time.Second

// minRateLimitWait is the wait for an explicit "0" or negative header: an
// answer, not a missing one, but the single retry should not fire into the same
// window.
const minRateLimitWait = 1 * time.Second

// parseRateLimitReset reads the header: missing or garbled waits
// maxRateLimitWait, zero or negative minRateLimitWait, anything else as given up
// to the cap.
func parseRateLimitReset(raw string) time.Duration {
	secs, err := strconv.Atoi(raw)
	if err != nil {
		return maxRateLimitWait
	}
	if secs <= 0 {
		return minRateLimitWait
	}
	d := time.Duration(secs) * time.Second
	if d > maxRateLimitWait {
		return maxRateLimitWait
	}
	return d
}

// rpcPathPrefix: requests go to POST {base}/<prefix><Service>/<Method>.
const rpcPathPrefix = "/tinkoff.public.invest.api.contract.v1."

// do performs one call, retrying once after a 429 for the reset the gateway
// reported (capped); a second 429 is an error.
func (c *Client) do(ctx context.Context, rpc string, reqBody, respBody any) error {
	err := c.doOnce(ctx, rpc, reqBody, respBody)

	var rl *rateLimitError
	if !errors.As(err, &rl) {
		return err
	}

	c.log.Warn("tinvest: rate limited, waiting for reset", "rpc", rpc, "wait", rl.resetAfter)
	if serr := c.sleep(ctx, rl.resetAfter); serr != nil {
		return fmt.Errorf("tinvest: %s: waiting out rate limit: %w", rpc, serr)
	}

	err = c.doOnce(ctx, rpc, reqBody, respBody)
	if errors.As(err, &rl) {
		c.log.Warn("tinvest: still rate limited after one retry, giving up", "rpc", rpc)
		return fmt.Errorf("tinvest: %s: rate limited again after waiting once: %w", rpc, err)
	}
	return err
}

// doOnce sends one POST and decodes a 200 into respBody (if non-nil). A 429 is
// *rateLimitError; 401 or code 40003 is ErrTokenInvalid; 404 or code 50002 wraps
// ErrInstrumentNotFound around the full error, since that one is worth reading
// with its rpc and body; anything else is a generic error.
func (c *Client) doOnce(ctx context.Context, rpc string, reqBody, respBody any) error {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("tinvest: %s: encode request: %w", rpc, err)
	}

	url := c.baseURL + rpcPathPrefix + rpc
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("tinvest: %s: build request: %w", rpc, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("tinvest: %s: request: %w", rpc, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("tinvest: %s: read response: %w", rpc, err)
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return &rateLimitError{resetAfter: parseRateLimitReset(resp.Header.Get(rateLimitResetHeader))}
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrTokenInvalid
	case resp.StatusCode != http.StatusOK:
		generic := fmt.Errorf("tinvest: %s: status %d: %s", rpc, resp.StatusCode, strings.TrimSpace(string(body)))
		var wErr wireError
		if json.Unmarshal(body, &wErr) == nil {
			if n, convErr := wErr.Description.Int64(); convErr == nil {
				switch n {
				case tokenInvalidDescription:
					return ErrTokenInvalid
				case instrumentNotFoundDescription:
					return fmt.Errorf("%w: %w", generic, ErrInstrumentNotFound)
				case reportNotReadyDescription:
					return fmt.Errorf("%w: %w", generic, errReportNotReady)
				}
			}
		}
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: %w", generic, ErrInstrumentNotFound)
		}
		return generic
	}

	if respBody == nil {
		return nil
	}
	if err := json.Unmarshal(body, respBody); err != nil {
		return fmt.Errorf("tinvest: %s: decode response: %w", rpc, err)
	}
	return nil
}
