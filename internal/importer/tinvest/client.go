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
// gateway: the machine's system certificate pool plus the embedded Russian
// Trusted Root CA, on this client's transport only — the process-wide
// system trust store is never modified. The gateway's certificate chains
// through an intermediate ("Russian Trusted Sub CA") up to this root, which
// is not present in typical system trust stores, so without it every
// request would fail TLS verification.
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
			// not parse, and TestEmbeddedCertParses/TestEmbeddedCertFingerprint
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
	Quantity                                                   int64
	Description                                                string
	Raw                                                        json.RawMessage
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
		if resp.NextCursor == cursor {
			// Guards against an infinite loop if the gateway ever answers
			// hasNext=true without the cursor actually advancing: a loud,
			// diagnosable error beats a sync job stuck forever on a
			// "current" cursor page.
			return nil, fmt.Errorf(
				"tinvest: OperationsService/GetOperationsByCursor: hasNext=true but cursor did not advance past %q",
				cursor)
		}
		cursor = resp.NextCursor
	}
}

// PortfolioPosition is one holding from OperationsService/GetPortfolio.
type PortfolioPosition struct {
	InstrumentUID, FIGI, InstrumentType string
	Quantity                            Quotation
	Blocked                             bool
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
// Nominal and Blocked are always zero-value: see wireInstrument's doc
// comment for what has actually been verified about GetInstrumentBy's
// response shape. They are kept on this type (rather than dropped) because
// a bond's nominal and a currency pair's nominal are real, needed facts —
// just not ones this endpoint supplies; a future task adding GetBondBy/
// GetCurrencyBy can populate them without changing this type's shape.
type InstrumentBrief struct {
	UID, FIGI, ISIN, Ticker, Name, Currency, InstrumentType string
	Nominal                                                 MoneyValue
	Blocked                                                 bool
}

// InstrumentByUID calls InstrumentsService/GetInstrumentBy with
// id_type=INSTRUMENT_ID_TYPE_UID.
func (c *Client) InstrumentByUID(ctx context.Context, uid string) (InstrumentBrief, error) {
	req := instrumentByRequest{IDType: instrumentIDTypeUID, ID: uid}
	var resp wireInstrumentResponse
	if err := c.do(ctx, "InstrumentsService/GetInstrumentBy", req, &resp); err != nil {
		return InstrumentBrief{}, err
	}

	nominal, err := resp.Instrument.Nominal.parse()
	if err != nil {
		return InstrumentBrief{}, fmt.Errorf("tinvest: InstrumentsService/GetInstrumentBy: nominal: %w", err)
	}

	return InstrumentBrief{
		UID:            resp.Instrument.UID,
		FIGI:           resp.Instrument.FIGI,
		ISIN:           resp.Instrument.ISIN,
		Ticker:         resp.Instrument.Ticker,
		Name:           resp.Instrument.Name,
		Currency:       strings.ToUpper(resp.Instrument.Currency),
		InstrumentType: resp.Instrument.InstrumentType,
		Nominal:        nominal,
		Blocked:        resp.Instrument.Blocked,
	}, nil
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

// parseRateLimitReset reads rateLimitResetHeader's value. A missing or
// unparsable header, or a non-positive one, is treated as "wait the full
// cap" — the conservative reading, since the alternative (wait 0 and
// immediately retry into the same limit) is the one failure mode a rate
// limiter's whole purpose is to prevent.
func parseRateLimitReset(raw string) time.Duration {
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		return maxRateLimitWait
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
// a 401 or a body whose Description is tokenInvalidDescription, and a
// generic "tinvest: <rpc>: status <code>: <body>" error for anything else
// that isn't 200.
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
		var wErr wireError
		if json.Unmarshal(body, &wErr) == nil && wErr.Description == tokenInvalidDescription {
			return ErrTokenInvalid
		}
		return fmt.Errorf("tinvest: %s: status %d: %s", rpc, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	if respBody == nil {
		return nil
	}
	if err := json.Unmarshal(body, respBody); err != nil {
		return fmt.Errorf("tinvest: %s: decode response: %w", rpc, err)
	}
	return nil
}
