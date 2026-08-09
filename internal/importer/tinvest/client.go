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

// SandboxBaseURL is the T-Invest sandbox REST gateway: same methods and
// wire shapes as the live gateway, but orders execute against simulated
// money rather than a real brokerage account.
const SandboxBaseURL = "https://sandbox-invest-public-api.tinkoff.ru/rest"

// ErrTokenInvalid is returned when the broker rejects the configured token:
// HTTP 401, or a response carrying business error code 40003 ("token
// invalid or revoked" — see wireError). Both are the gateway's documented
// signal that the token needs replacing, not that this one request failed.
var ErrTokenInvalid = errors.New("tinvest: token invalid or revoked")

// ErrInstrumentNotFound is returned when the broker says it has no such
// instrument: HTTP 404, or a response carrying business error code 50002
// ("Instrument not found" — see wireError). Verified against the live
// gateway on 2026-08-05: asking InstrumentsService/GetInstrumentBy about an
// unknown uid answers 404 with the body
// {"code":5,"message":"Instrument not found","description":"50002"}, so
// "there is no such paper" is DISTINGUISHABLE from a network or gateway
// failure rather than being guessed at.
//
// That distinction is the whole point of the sentinel. A caller resolving an
// instrument can refuse the one operation that names it and go on — which is
// the only outcome that ever ends, for a delisted paper the broker will
// never answer about again — while every other failure stays fatal, because
// those are the ones the next run is likely to survive.
//
// The error this wraps still carries the rpc, the status and the body, so a
// log line about it says what the broker actually answered.
var ErrInstrumentNotFound = errors.New("tinvest: the broker has no such instrument")

// defaultHTTPTimeout is the request timeout NewClient uses when the caller
// passes a nil *http.Client. 30s: generous for a single unary REST call
// (accounts, one page of operations, a portfolio snapshot), while still
// bounding a stalled connection well short of an hourly sync job's budget.
// This project has already shipped an unbounded HTTP client once (see the
// moex client note in cmd/babki/root.go) — every client here gets an
// explicit, tested timeout so that gap does not repeat itself.
const defaultHTTPTimeout = 30 * time.Second

// russianTrustedRootCAPEM's SHA-256 fingerprint is
// D2:6D:2D:02:31:B7:C3:9F:92:CC:73:85:12:BA:54:10:35:19:E4:40:5D:68:B5:BD:70:3E:97:88:CA:8E:CF:31
// — verified with `openssl x509 -in russian_trusted_root_ca.pem -noout
// -fingerprint -sha256` on 2026-08-04, against the root that alone
// completed verification of a live TLS chain from
// invest-public-api.tinkoff.ru (whose leaf chains through an intermediate,
// "Russian Trusted Sub CA", issued by this root). Expires 2032-02-27.
// TestEmbeddedCert_FingerprintMatchesWhatWasVerifiedOutOfBand re-derives
// and checks this value against the embedded file on every test run, so a
// future re-embedding (rotation, or someone swapping the file) that changes
// it will fail loudly rather than silently trusting a different root.
//
//go:embed russian_trusted_root_ca.pem
var russianTrustedRootCAPEM []byte

// NewHTTPClient returns an *http.Client trusted to reach the T-Invest REST
// gateway, on this client's transport only — the process-wide system trust
// store is never modified. Normally that means the machine's system
// certificate pool plus the embedded Russian Trusted Root CA; if the
// platform reports no usable system pool (see the fallback in the body
// below), only the embedded root is trusted, since there is no system pool
// to add it to. The gateway's certificate chains through an intermediate
// ("Russian Trusted Sub CA") up to this root, which is not present in
// typical system trust stores, so without it every request would fail TLS
// verification.
//
// timeout bounds every request this client makes; see defaultHTTPTimeout
// for the reasoning NewClient uses when picking one.
func NewHTTPClient(timeout time.Duration) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		// x509.SystemCertPool's own doc: it can return a non-nil error on
		// platforms without a system pool concept. A caller-visible failure
		// here would turn "this platform has no notion of a system pool"
		// into "tinvest cannot start at all", which is worse than trusting
		// only the embedded root in that case.
		pool = x509.NewCertPool()
	}
	if ok := pool.AppendCertsFromPEM(russianTrustedRootCAPEM); !ok {
		return nil, errors.New("tinvest: embedded Russian Trusted Root CA did not parse")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool}

	return &http.Client{Timeout: timeout, Transport: transport}, nil
}

// sleepFunc pauses for d, respecting ctx cancellation, and reports which one
// won. It exists as a field (rather than a bare time.Sleep call) so tests
// can control the 429 backoff without a real wait — see (*Client).sleep.
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
// parses responses into this package's types, and retries exactly once on
// a rate limit. It holds no account state and does no caching.
type Client struct {
	http    *http.Client
	baseURL string
	token   string
	log     *slog.Logger
	// sleep implements the 429 backoff wait. Unexported deliberately: tests
	// within this package substitute a fake to control timing (see item 4
	// of the task brief — "сделай поле-функцию сна на клиенте, невыставленное
	// наружу"); nothing outside the package should ever need to touch it.
	sleep sleepFunc
}

// NewClient builds a Client. hc may be nil, in which case
// NewHTTPClient(defaultHTTPTimeout) is used; baseURL may be empty, in which
// case DefaultBaseURL is used (baseURL is parameterized rather than
// hardcoded so tests can point it at an httptest.Server, following this
// codebase's cbr.New convention); log may be nil, in which case
// slog.Default is used.
func NewClient(hc *http.Client, baseURL, token string, log *slog.Logger) *Client {
	if hc == nil {
		var err error
		hc, err = NewHTTPClient(defaultHTTPTimeout)
		if err != nil {
			// NewHTTPClient can only fail if the embedded certificate does
			// not parse, and TestEmbeddedCert_ParsesAndIsNotExpired /
			// TestEmbeddedCert_FingerprintMatchesWhatWasVerifiedOutOfBand
			// already prove it does on every build of this package. A
			// caller passing nil is trusting that default to work exactly
			// as those tests already showed it does; silently falling back
			// to a client with no pinned root (which would then fail every
			// TLS handshake anyway, just later and less diagnosably) is the
			// kind of silent failure this project's own history warns
			// against. Panicking here keeps the failure loud and at
			// startup, which is unreachable in practice.
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

// MoneyValue is a monetary amount with a currency, mirroring the broker's
// wire MoneyValue (units + nano). Currency is always upper case.
type MoneyValue struct {
	Currency string
	Units    int64
	Nano     int32
}

// Decimal returns m as a decimal.Decimal: Units + Nano*1e-9, computed by
// adding the two quantities rather than by formatting "<Units>.<Nano>" as
// text. The broker's wire format allows Units and Nano to carry the sign
// independently — a "negative zero" split such as {Units: 0, Nano:
// -200000000}, meaning -0.2 — and whenever both are nonzero their signs
// agree, so summing decimal.NewFromInt(Units) and decimal.New(Nano, -9) is
// correct for both the negative-zero case and the ordinary case; string
// concatenation would get the former wrong (there is no "-0" to concatenate
// a fractional part onto).
func (m MoneyValue) Decimal() decimal.Decimal {
	return decimal.NewFromInt(m.Units).Add(decimal.New(int64(m.Nano), -9))
}

// Quotation is a bare decimal quantity with no currency, mirroring the
// broker's wire Quotation.
type Quotation struct {
	Units int64
	Nano  int32
}

// Decimal returns q as a decimal.Decimal; see MoneyValue.Decimal for why
// Units and Nano are summed rather than concatenated as text.
func (q Quotation) Decimal() decimal.Decimal {
	return decimal.NewFromInt(q.Units).Add(decimal.New(int64(q.Nano), -9))
}

// Account is one of the caller's T-Invest accounts (UsersService/
// GetAccounts). Type and Status are the enum's wire strings verbatim (e.g.
// "ACCOUNT_TYPE_TINKOFF_IIS", "ACCOUNT_STATUS_OPEN") — this package hands
// them on rather than parsing them further, leaving classification to the
// caller. OpenedOn is nil when the gateway sent no openedDate.
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

// OperationItem is one entry from OperationsService/GetOperationsByCursor.
// Type and State are the enum's wire strings verbatim (e.g.
// "OPERATION_TYPE_BUY", "OPERATION_STATE_EXECUTED"); Raw is the element
// exactly as the gateway sent it, for callers (the mirror store) that need
// to keep the broker's own bytes rather than only this package's reading of
// them.
type OperationItem struct {
	ID, ParentOperationID, Type, State                         string
	Date                                                       time.Time
	InstrumentUID, FIGI, PositionUID, AssetUID, InstrumentType string
	Payment, Commission, AccruedInt, Price                     MoneyValue
	// Quantity is the size of the ORDER this operation belongs to, and
	// QuantityDone how much of that order was executed. They differ on every
	// partial fill, and the difference is not small: the broker has sent an
	// order of 11100 units against a fill of 6644.
	//
	// ONLY AN ORDER CAN BE PARTIALLY EXECUTED, which is what decides where
	// each of the two belongs. A trade takes its units from QuantityDone —
	// the money the broker reports for the operation always divides by that
	// one and never by Quantity (#131). A securities transfer is not an order
	// and has no fill: the broker sends the same number in both fields, and
	// the projection reads Quantity there, because a transfer between one's
	// own accounts takes its DIRECTION from that number's sign and nothing
	// documents QuantityDone as signed.
	//
	// QuantityDone is zero when the broker omits the field. That is not a
	// size of zero and must not be read as one; a trade without it is refused
	// rather than measured by its order (see projectTrade).
	Quantity, QuantityDone int64
	Description            string
	Raw                    json.RawMessage
}

// operationsPageLimit is the page size OperationsAll requests. 1000 is the
// documented maximum (see docs_limits/the task brief); the brief also notes
// that limit<=2 is documented to duplicate rows and break cursor movement,
// so this is chosen to be as far from that broken range as the API allows,
// not merely "a limit above 2".
const operationsPageLimit = 1000

// OperationsAll walks OperationsService/GetOperationsByCursor to
// completion, starting at from (the zero time.Time omits the "from" filter
// entirely, requesting the account's full history), and returns every
// operation across every page — executed, canceled, and in-progress alike,
// since state is never filtered (see getOperationsByCursorRequest). It does
// not deduplicate: this client always requests operationsPageLimit (1000),
// well clear of the small-page-size range (limit<=2) the broker's own docs
// warn duplicates rows in, but the mirror this feeds is append-only by
// design and needs its own dedup pass regardless of page size — so that
// pass belongs to the caller, not here.
func (c *Client) OperationsAll(ctx context.Context, brokerAccountID string, from time.Time) ([]OperationItem, error) {
	var all []OperationItem
	cursor := ""
	// seenCursors guards against the gateway cycling cursors (A -> B -> A ->
	// B -> ...), not just repeating the immediately preceding one: a check
	// against only the previous cursor would spin through a longer cycle
	// like that until ctx's own deadline cut it off, accumulating items in
	// memory the whole time. Every cursor this loop has already requested
	// goes in here before it is requested again, so any repeat — one step
	// back or many steps around a cycle — is caught on the request that
	// would repeat it. The set grows with the number of distinct pages
	// actually walked, the same order as `all` itself, so it adds no memory
	// growth risk beyond what a long legitimate history already costs.
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
			// Guards against an infinite loop if the gateway ever answers
			// hasNext=true with a cursor this loop has already requested —
			// whether that's the immediately preceding one or an earlier
			// page in a longer cycle: a loud, diagnosable error beats a
			// sync job stuck forever re-walking the same pages.
			return nil, fmt.Errorf(
				"tinvest: OperationsService/GetOperationsByCursor: hasNext=true but cursor %q repeats an earlier page",
				resp.NextCursor)
		}
		seenCursors[resp.NextCursor] = true
		cursor = resp.NextCursor
	}
}

// PortfolioPosition is one holding from OperationsService/GetPortfolio.
//
// Quantity and Blocked answer two different questions, confirmed 2026-08-05
// against the published proto comments (RussianInvestments/investAPI,
// operations.proto, message PortfolioPosition):
//
//   - Quantity ("Количество инструмента в портфеле в штуках") is the whole
//     position: everything this account holds, full stop. The wire format
//     documents no "free" vs "reserved" split on this field, so it already
//     includes any units currently reserved by an open sell order — that
//     reservation is a separate field, blocked_lots (a quantity), which
//     this client does not decode. A reconciliation against the journal
//     must compare Quantity as-is; there is no separate blocked quantity on
//     this type to add to it.
//   - Blocked ("Заблокировано на бирже") is not a quantity at all — it is a
//     bool flagging that the position is halted at the depository/exchange
//     level (the broker's own FAQ: "отражает, заблокирован ли инструмент
//     депозитарием"). It is unrelated to blocked_lots and must not be
//     combined with Quantity in any arithmetic.
//
// InstrumentType is the broker's own word for what kind of asset this is, the
// same vocabulary InstrumentBrief.InstrumentType carries ("share", "bond",
// "etf" are the three this program accounts for; see brokerInstrumentTypes).
// IT IS NOT ALWAYS A SECURITY: a live sandbox account that was only ever
// topped up with rubles, holding nothing, came back with exactly one
// position, of type "currency" (checked 2026-08-05; the response is
// testdata/portfolio_cash_only.json). Whoever compares these positions
// against a portfolio of securities has to read this field first — see
// compareInstruments.
//
// Ticker is what a person calls the instrument, and is empty when the gateway
// sent none.
type PortfolioPosition struct {
	InstrumentUID, FIGI, InstrumentType, Ticker string
	Quantity                                    Quotation
	Blocked                                     bool
}

// GetPortfolio calls OperationsService/GetPortfolio and returns the
// account's current positions.
func (c *Client) GetPortfolio(ctx context.Context, brokerAccountID string) ([]PortfolioPosition, error) {
	var resp wireGetPortfolioResponse
	if err := c.do(ctx, "OperationsService/GetPortfolio", accountIDRequest{AccountID: brokerAccountID}, &resp); err != nil {
		return nil, err
	}
	positions := make([]PortfolioPosition, 0, len(resp.Positions))
	for _, w := range resp.Positions {
		p, err := w.parse()
		if err != nil {
			return nil, fmt.Errorf("tinvest: OperationsService/GetPortfolio: %w", err)
		}
		positions = append(positions, p)
	}
	return positions, nil
}

// MoneyBalance is one currency's cash balance from OperationsService/
// GetPositions: Value is the free balance, Blocked the amount held by open
// orders. Both are decimal.Decimal, not MoneyValue — a balance is already
// pinned to Currency, so there is no separate currency to carry per field.
type MoneyBalance struct {
	Currency       string
	Value, Blocked decimal.Decimal
}

// GetPositions calls OperationsService/GetPositions and returns one
// MoneyBalance per currency that appears in either the response's money or
// blocked list. The two lists are independent (a currency can appear in
// one and not the other), so a currency missing from one side is reported
// with zero on that side rather than treated as an error. Currencies are
// sorted ascending so the result is deterministic across calls.
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

// InstrumentBrief is InstrumentsService/GetInstrumentBy's answer, trimmed to
// the fields this package's callers need.
//
// It has no Nominal or Blocked field: GetInstrumentBy does not supply a
// nominal (see wireInstrument's doc comment for what that claim is checked
// against), and there is nothing to populate it with here, honestly zero or
// not. A bond's nominal lives on a different call — BondNominalByUID.
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

// Listing is one of the broker's tradable lines for a security, as
// FindInstrument reports it. ONE PAPER HAS SEVERAL — a search for Apple's ISIN
// answers with nine, across venues and under three different tickers.
//
// THERE IS NO CURRENCY HERE, AND THAT IS THE SEARCH'S OWN SHAPE rather than an
// omission of this type: FindInstrument returns InstrumentShort, which carries
// no currency field at all (checked live, 2026-08-10 — every one of the nine
// Apple listings came back without one). What a listing is denominated in has
// to be asked of the passport, InstrumentByUID.
//
// Declaring it here anyway was a defect worth remembering: a filter compared it
// against the catalog's currency, every listing carried the empty string, and
// the whole search quietly matched nothing. The unit tests passed because their
// fixtures supplied a currency the real API never sends.
type Listing struct {
	UID, ISIN, Ticker, Name, ClassCode, Kind string
}

// FindInstruments searches the broker's catalog by a free-text query, which for
// this program's purposes is always an ISIN: a ticker is not unique across
// venues or even across issuers (the broker answers "T" with a bond of one
// issuer and a share of another), and matching on one is how a holding gets
// priced with a stranger's price.
//
// The result is EVERY listing the broker returns, unfiltered. Choosing among
// them is the caller's, and is deliberately not hidden in here: the rule for
// picking is about what the catalog row says and what the prices show, neither
// of which a search knows.
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

// BondNominalByUID calls InstrumentsService/BondBy with
// id_type=INSTRUMENT_ID_TYPE_UID and returns the bond's nominal — the one
// value InstrumentByUID's GetInstrumentBy call cannot supply (see
// InstrumentBrief's doc comment). This task's review checked the shape live
// against the sandbox gateway for bond uid
// 2dd3b003-aca2-4920-89ce-8d827c637372: its BondBy response carried
// "nominal": {"currency": "rub", "units": "1000", "nano": 0} under
// "instrument" (that call is not re-run by this fix; the fixture-based
// tests below are what run on every build). The name is deliberately
// narrow: this method returns exactly the bond's nominal and nothing else
// about the instrument, so a caller cannot mistake it for a general
// instrument lookup.
func (c *Client) BondNominalByUID(ctx context.Context, uid string) (MoneyValue, error) {
	req := instrumentByRequest{IDType: instrumentIDTypeUID, ID: uid}
	var resp wireBondResponse
	if err := c.do(ctx, "InstrumentsService/BondBy", req, &resp); err != nil {
		return MoneyValue{}, err
	}

	nominal, err := resp.Instrument.Nominal.parse()
	if err != nil {
		return MoneyValue{}, fmt.Errorf("tinvest: InstrumentsService/BondBy: nominal: %w", err)
	}
	if !nominal.Decimal().IsZero() {
		return nominal, nil
	}

	// A redeemed bond is quoted with a nominal of zero — the face value has
	// been paid back, so none of it is outstanding — while initialNominal
	// keeps what the bond was issued at. Checked live against the gateway on
	// three bonds: МФК Быстроденьги Ю002Р-01 (matured 2026-06-03) and ОФЗ
	// 29014 (matured 2026-03-25) both answer nominal 0 with initialNominal
	// 100 CNY and 1000 RUB; Республика Казахстан 11 (matures 2030) answers
	// 1000 for both.
	//
	// The order matters and is not interchangeable. An AMORTIZING bond that
	// is still alive has repaid part of its face and carries a nominal
	// smaller than the initial one; taking the initial there would overstate
	// every valuation of it, since a bond is quoted as a percent of nominal.
	// The fallback is reachable only at zero, which is the state where there
	// is no live position left to overstate — and where the history still
	// needs a face value, because refusing the instrument outright drops
	// every purchase, coupon and redemption the bond ever had.
	initial, err := resp.Instrument.InitialNominal.parse()
	if err != nil {
		return MoneyValue{}, fmt.Errorf("tinvest: InstrumentsService/BondBy: initial nominal: %w", err)
	}
	return initial, nil
}

// rateLimitError signals a 429 response; do() catches it with errors.As to
// decide whether to wait and retry, rather than treating it like any other
// non-200 status.
type rateLimitError struct {
	resetAfter time.Duration
}

func (e *rateLimitError) Error() string {
	return fmt.Sprintf("tinvest: rate limited, reset in %s", e.resetAfter)
}

// rateLimitResetHeader is the header the gateway uses to say how many
// seconds remain until its per-method rate limit resets (see docs_grpc.md /
// the task brief's "Факты T-Invest API").
const rateLimitResetHeader = "x-ratelimit-reset"

// maxRateLimitWait bounds how long do() will ever sleep for a single 429.
// T-Invest's limits reset on a per-minute window (the task brief: "Лимиты
// (в минуту, ...)"), so a truthful x-ratelimit-reset value should never
// exceed ~60s; 65s adds a five-second margin for clock skew between the
// gateway's limiter and this process, while still refusing to trust a
// garbled or absurd header value (the brief's own example: "100000") enough
// to sleep a sync job for a day.
const maxRateLimitWait = 65 * time.Second

// minRateLimitWait is what parseRateLimitReset returns for an explicit
// non-positive header value (e.g. "0"). The gateway saying "0" is a
// concrete answer — "already clear" — not the absence of one, so it must
// not be treated the same as a missing/garbled header: sleeping
// maxRateLimitWait (65s) on an explicit "0" would idle an hourly sync job
// for a reason the gateway never gave. A small positive floor rather than 0
// outright still guarantees do()'s single retry is not fired back-to-back
// into the same window.
const minRateLimitWait = 1 * time.Second

// parseRateLimitReset reads rateLimitResetHeader's value.
//   - Missing or unparsable: treated as "wait the full cap"
//     (maxRateLimitWait) — the conservative reading, since the alternative
//     (wait 0 and immediately retry into the same limit) is the one
//     failure mode a rate limiter's whole purpose is to prevent, and there
//     is no concrete number here to trust instead.
//   - An explicit zero or negative value: treated as minRateLimitWait, not
//     the cap — see minRateLimitWait's own comment.
//   - Any other positive value: used as given, capped at maxRateLimitWait.
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

// rpcPathPrefix is prepended to every rpc argument do()/doOnce() receive to
// build the request path, per the gateway's own convention: POST
// {base}/tinkoff.public.invest.api.contract.v1.<Service>/<Method>.
const rpcPathPrefix = "/tinkoff.public.invest.api.contract.v1."

// do performs one gateway call, retrying exactly once if the first attempt
// is rate limited: it waits out the reset the gateway itself reported (via
// c.sleep, capped by maxRateLimitWait) and tries again. A second 429 is
// surfaced as an error rather than retried again — see the task brief:
// "Повтор после ожидания — ОДИН раз, не бесконечный цикл".
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

// doOnce sends one POST request to rpc and decodes a 200 response into
// respBody (skipped when respBody is nil). It returns *rateLimitError for a
// 429 (letting do() decide whether to wait and retry), ErrTokenInvalid for
// a 401 or a body whose Description is tokenInvalidDescription, an error
// wrapping ErrInstrumentNotFound for a 404 or a body whose Description is
// instrumentNotFoundDescription, and a generic "tinvest: <rpc>: status
// <code>: <body>" error for anything else that isn't 200.
//
// The two sentinels are returned differently on purpose: a token refusal is
// the same statement whichever call met it, while "no such instrument" is
// worth reading with the rpc, the status and the broker's own body attached,
// so that one wraps the generic error rather than replacing it.
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
