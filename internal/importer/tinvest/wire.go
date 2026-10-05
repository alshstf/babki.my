package tinvest

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// parseWireInt64 parses a string-encoded int64 (protobuf int64 over JSON is a
// string). An absent field is "" and reads as 0, since protojson omits zero
// values.
func parseWireInt64(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

// parseWireTime parses an RFC 3339 timestamp; "" is the zero time. Callers
// that must tell absent from zero check for "" first.
func parseWireTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse time %q: %w", s, err)
	}
	return t, nil
}

// wireMoneyValue is the gateway's MoneyValue: units a string, nano a number,
// currency lower case (upper-cased by parse).
type wireMoneyValue struct {
	Currency string `json:"currency"`
	Units    string `json:"units"`
	Nano     int32  `json:"nano"`
}

func (w wireMoneyValue) parse() (MoneyValue, error) {
	units, err := parseWireInt64(w.Units)
	if err != nil {
		return MoneyValue{}, fmt.Errorf("tinvest: parse MoneyValue.units %q: %w", w.Units, err)
	}
	return MoneyValue{
		Currency: strings.ToUpper(w.Currency),
		Units:    units,
		Nano:     w.Nano,
	}, nil
}

// wireQuotation is the gateway's Quotation: units and nano, no currency.
type wireQuotation struct {
	Units string `json:"units"`
	Nano  int32  `json:"nano"`
}

func (w wireQuotation) parse() (Quotation, error) {
	units, err := parseWireInt64(w.Units)
	if err != nil {
		return Quotation{}, fmt.Errorf("tinvest: parse Quotation.units %q: %w", w.Units, err)
	}
	return Quotation{Units: units, Nano: w.Nano}, nil
}

// wireError is the gateway's error body: Code is the gRPC status, Description
// the broker's business code (40003: bad token, with HTTP 401). The spec says
// Description is an integer, but the live gateway sends it quoted
// ("description":"30079"), so it is a json.Number.
type wireError struct {
	Code        int         `json:"code"`
	Message     string      `json:"message"`
	Description json.Number `json:"description"`
}

// tokenInvalidDescription: an expired, revoked or invalid token.
const tokenInvalidDescription = 40003

// instrumentNotFoundDescription: an unknown instrument (live, 2026-08-05:
// HTTP 404 with "description":"50002", quoted).
const instrumentNotFoundDescription = 50002

// reportNotReadyDescription: a broker report not built yet
// (errReportNotReady).
const reportNotReadyDescription = 30058

// wireAccount is the gateway's Account; Type and Status stay wire strings.
type wireAccount struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	OpenedDate string `json:"openedDate"`
}

func (w wireAccount) parse() (Account, error) {
	acc := Account{ID: w.ID, Name: w.Name, Type: w.Type, Status: w.Status}
	if w.OpenedDate != "" {
		t, err := parseWireTime(w.OpenedDate)
		if err != nil {
			return Account{}, fmt.Errorf("tinvest: parse Account.openedDate: %w", err)
		}
		acc.OpenedOn = &t
	}
	return acc, nil
}

// wireGetAccountsResponse mirrors UsersService/GetAccounts's response body.
type wireGetAccountsResponse struct {
	Accounts []wireAccount `json:"accounts"`
}

// wireOperationItem is the gateway's OperationItem, only the fields
// OperationItem surfaces; the rest stays in OperationItem.Raw.
type wireOperationItem struct {
	ID                string         `json:"id"`
	ParentOperationID string         `json:"parentOperationId"`
	Type              string         `json:"type"`
	State             string         `json:"state"`
	Date              string         `json:"date"`
	InstrumentUID     string         `json:"instrumentUid"`
	FIGI              string         `json:"figi"`
	PositionUID       string         `json:"positionUid"`
	AssetUID          string         `json:"assetUid"`
	InstrumentType    string         `json:"instrumentType"`
	Ticker            string         `json:"ticker"`
	ClassCode         string         `json:"classCode"`
	Payment           wireMoneyValue `json:"payment"`
	Commission        wireMoneyValue `json:"commission"`
	AccruedInt        wireMoneyValue `json:"accruedInt"`
	Price             wireMoneyValue `json:"price"`
	Quantity          string         `json:"quantity"`
	QuantityDone      string         `json:"quantityDone"`
	Description       string         `json:"description"`
}

func (w wireOperationItem) parse(raw json.RawMessage) (OperationItem, error) {
	date, err := parseWireTime(w.Date)
	if err != nil {
		return OperationItem{}, fmt.Errorf("tinvest: parse OperationItem(%s).date: %w", w.ID, err)
	}
	quantity, err := parseWireInt64(w.Quantity)
	if err != nil {
		return OperationItem{}, fmt.Errorf("tinvest: parse OperationItem(%s).quantity %q: %w", w.ID, w.Quantity, err)
	}
	quantityDone, err := parseWireInt64(w.QuantityDone)
	if err != nil {
		return OperationItem{}, fmt.Errorf("tinvest: parse OperationItem(%s).quantityDone %q: %w", w.ID, w.QuantityDone, err)
	}
	payment, err := w.Payment.parse()
	if err != nil {
		return OperationItem{}, fmt.Errorf("tinvest: OperationItem(%s).payment: %w", w.ID, err)
	}
	commission, err := w.Commission.parse()
	if err != nil {
		return OperationItem{}, fmt.Errorf("tinvest: OperationItem(%s).commission: %w", w.ID, err)
	}
	accruedInt, err := w.AccruedInt.parse()
	if err != nil {
		return OperationItem{}, fmt.Errorf("tinvest: OperationItem(%s).accruedInt: %w", w.ID, err)
	}
	price, err := w.Price.parse()
	if err != nil {
		return OperationItem{}, fmt.Errorf("tinvest: OperationItem(%s).price: %w", w.ID, err)
	}

	return OperationItem{
		ID:                w.ID,
		ParentOperationID: w.ParentOperationID,
		Type:              w.Type,
		State:             w.State,
		Date:              date,
		InstrumentUID:     w.InstrumentUID,
		FIGI:              w.FIGI,
		PositionUID:       w.PositionUID,
		AssetUID:          w.AssetUID,
		InstrumentType:    w.InstrumentType,
		Ticker:            w.Ticker,
		ClassCode:         w.ClassCode,
		Payment:           payment,
		Commission:        commission,
		AccruedInt:        accruedInt,
		Price:             price,
		Quantity:          quantity,
		QuantityDone:      quantityDone,
		Description:       w.Description,
		Raw:               raw,
	}, nil
}

// getOperationsByCursorRequest has no state field at all: the mirror is
// append-only, so canceled and in-progress operations must arrive like executed
// ones.
type getOperationsByCursorRequest struct {
	AccountID string `json:"accountId"`
	From      string `json:"from,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
	Limit     int    `json:"limit"`
}

// wireGetOperationsByCursorResponse keeps items raw so OperationItem.Raw is
// each element byte for byte.
type wireGetOperationsByCursorResponse struct {
	HasNext    bool              `json:"hasNext"`
	NextCursor string            `json:"nextCursor"`
	Items      []json.RawMessage `json:"items"`
}

// accountIDRequest is the {accountId} body of GetPortfolio and
// GetPositions.
type accountIDRequest struct {
	AccountID string `json:"accountId"`
}

// wirePortfolioPosition is the gateway's PortfolioPosition, only the fields
// PortfolioPosition surfaces. Ticker names a position that resolves to nothing of
// ours, since a uid and figi mean nothing to a person (present in the live
// sandbox, 2026-08-05).
type wirePortfolioPosition struct {
	FIGI           string        `json:"figi"`
	InstrumentType string        `json:"instrumentType"`
	Quantity       wireQuotation `json:"quantity"`
	InstrumentUID  string        `json:"instrumentUid"`
	Ticker         string        `json:"ticker"`
	Blocked        bool          `json:"blocked"`
}

func (w wirePortfolioPosition) parse() (PortfolioPosition, error) {
	qty, err := w.Quantity.parse()
	if err != nil {
		return PortfolioPosition{}, fmt.Errorf("tinvest: PortfolioPosition(%s).quantity: %w", w.InstrumentUID, err)
	}
	return PortfolioPosition{
		InstrumentUID:  w.InstrumentUID,
		FIGI:           w.FIGI,
		InstrumentType: w.InstrumentType,
		Ticker:         w.Ticker,
		Quantity:       qty,
		Blocked:        w.Blocked,
	}, nil
}

// wireGetPortfolioResponse mirrors PortfolioResponse.
type wireGetPortfolioResponse struct {
	Positions []wirePortfolioPosition `json:"positions"`
	// TotalAmountPortfolio is the account's worth in the requested currency;
	// absent from some answers (an empty sandbox).
	TotalAmountPortfolio *wireMoneyValue `json:"totalAmountPortfolio"`
}

// portfolioRequest asks for positions with the total in roubles.
type portfolioRequest struct {
	AccountID string `json:"accountId"`
	Currency  string `json:"currency"`
}

// wireGetPositionsResponse holds the money and blocked lists; a currency in
// one and not the other is zero on the missing side.
type wireGetPositionsResponse struct {
	Money   []wireMoneyValue `json:"money"`
	Blocked []wireMoneyValue `json:"blocked"`
}

// instrumentByRequest looks up by uid; classCode is needed only for tickers.
type instrumentByRequest struct {
	IDType string `json:"idType"`
	ID     string `json:"id"`
}

// instrumentIDTypeUID is the enum member by name, as REST expects.
const instrumentIDTypeUID = "INSTRUMENT_ID_TYPE_UID"

// wireInstrument is v1Instrument, GetInstrumentBy's answer. It has no nominal
// and no sanctions flag (spec re-checked 2026-08-05; blockedTcaFlag is a contract
// lock, not a freeze; a live bond lookup had no "nominal"). A bond's nominal comes
// from BondNominalByUID.
type wireInstrument struct {
	UID            string `json:"uid"`
	FIGI           string `json:"figi"`
	ISIN           string `json:"isin"`
	Ticker         string `json:"ticker"`
	Name           string `json:"name"`
	Currency       string `json:"currency"`
	InstrumentType string `json:"instrumentType"`
}

// wireInstrumentResponse mirrors InstrumentResponse.
type wireInstrumentResponse struct {
	Instrument wireInstrument `json:"instrument"`
}

// wireBondResponse is BondBy's answer, trimmed to the nominal, nested under
// "instrument".
type wireBondResponse struct {
	Instrument struct {
		Nominal        wireMoneyValue `json:"nominal"`
		InitialNominal wireMoneyValue `json:"initialNominal"`
	} `json:"instrument"`
}

// wireCurrencyResponse is CurrencyBy's answer, trimmed to the nominal, nested
// under "instrument" like every By-method.
type wireCurrencyResponse struct {
	Instrument struct {
		Nominal wireMoneyValue `json:"nominal"`
	} `json:"instrument"`
}

// wireFindInstrumentResponse is FindInstrument's answer: InstrumentShort
// elements with the four fields the search needs.
type wireFindInstrumentResponse struct {
	Instruments []struct {
		UID            string `json:"uid"`
		ISIN           string `json:"isin"`
		Ticker         string `json:"ticker"`
		Name           string `json:"name"`
		ClassCode      string `json:"classCode"`
		InstrumentKind string `json:"instrumentKind"`
	} `json:"instruments"`
}

// wireGetLastPricesResponse is GetLastPrices' answer, which has no currency
// (see LastPrice and migration 0017).
type wireGetLastPricesResponse struct {
	LastPrices []wireLastPrice `json:"lastPrices"`
}

type wireLastPrice struct {
	InstrumentUID string         `json:"instrumentUid"`
	Price         *wireQuotation `json:"price"`
	Time          string         `json:"time"`
	LastPriceType string         `json:"lastPriceType"`
}

// parse reports ok=false for an entry with a uid and no price, which the
// broker does send; it is not a price of zero.
func (w wireLastPrice) parse() (LastPrice, bool, error) {
	if w.Price == nil {
		return LastPrice{}, false, nil
	}
	q, err := w.Price.parse()
	if err != nil {
		return LastPrice{}, false, fmt.Errorf("tinvest: LastPrice(%s).price: %w", w.InstrumentUID, err)
	}
	price := q.Decimal()
	at, err := parseWireTime(w.Time)
	if err != nil {
		return LastPrice{}, false, fmt.Errorf("tinvest: parse LastPrice(%s).time: %w", w.InstrumentUID, err)
	}
	return LastPrice{
		InstrumentUID: w.InstrumentUID,
		Price:         price,
		At:            at,
		Dealer:        w.LastPriceType == "LAST_PRICE_DEALER",
	}, true, nil
}

// generateBrokerReportRequest is the request oneof's "generate" arm.
type generateBrokerReportRequest struct {
	Generate generateBrokerReport `json:"generateBrokerReportRequest"`
}

type generateBrokerReport struct {
	AccountID string `json:"accountId"`
	From      string `json:"from"`
	To        string `json:"to"`
}

// getBrokerReportRequest is the oneof's other arm: one page of an ordered
// report.
type getBrokerReportRequest struct {
	Get getBrokerReport `json:"getBrokerReportRequest"`
}

type getBrokerReport struct {
	TaskID string `json:"taskId"`
	Page   int    `json:"page"`
}

type wireGenerateBrokerReportResponse struct {
	Generate struct {
		TaskID string `json:"taskId"`
	} `json:"generateBrokerReportResponse"`
}

type wireGetBrokerReportResponse struct {
	Get wireBrokerReport `json:"getBrokerReportResponse"`
}

// wireBrokerReport is one report page, only the fields TradeSettlement
// needs.
type wireBrokerReport struct {
	Rows       []wireBrokerReportRow `json:"brokerReport"`
	PagesCount int                   `json:"pagesCount"`
}

type wireBrokerReportRow struct {
	TradeID        string `json:"tradeId"`
	TradeDatetime  string `json:"tradeDatetime"`
	ClearValueDate string `json:"clearValueDate"`
}

// parse reads one trade. The settlement day arrives as UTC midnight
// ("2024-02-27T00:00:00Z" for a trade of the 26th); anything else is refused
// rather than rounded. A row without one is ok=false and its operations keep
// their trade day.
func (w wireBrokerReportRow) parse() (s TradeSettlement, ok bool, err error) {
	if w.ClearValueDate == "" {
		return TradeSettlement{}, false, nil
	}
	if w.TradeID == "" {
		return TradeSettlement{}, false, fmt.Errorf("a trade with no tradeId")
	}
	traded, err := parseWireTime(w.TradeDatetime)
	if err != nil {
		return TradeSettlement{}, false, fmt.Errorf("trade %s: tradeDatetime: %w", w.TradeID, err)
	}
	settled, err := parseWireTime(w.ClearValueDate)
	if err != nil {
		return TradeSettlement{}, false, fmt.Errorf("trade %s: clearValueDate: %w", w.TradeID, err)
	}
	settled = settled.UTC()
	if !settled.Equal(settled.Truncate(24 * time.Hour)) {
		return TradeSettlement{}, false, fmt.Errorf("trade %s: clearValueDate %s is not a calendar day", w.TradeID, w.ClearValueDate)
	}
	return TradeSettlement{TradeID: w.TradeID, TradedAt: traded, SettledOn: settled}, true, nil
}

// getDividendsRequest mirrors GetDividendsRequest.
type getDividendsRequest struct {
	InstrumentID string `json:"instrumentId"`
	From         string `json:"from"`
	To           string `json:"to"`
}

type wireGetDividendsResponse struct {
	Dividends []wireDividend `json:"dividends"`
}

// wireDividend is GetDividends' Dividend, trimmed. dividendNet is, despite
// the name, per share before tax: NVIDIA's 0,04 $ for September 2021, of which
// 0,028 $ reached the account.
type wireDividend struct {
	DividendNet wireMoneyValue `json:"dividendNet"`
	RecordDate  string         `json:"recordDate"`
	PaymentDate string         `json:"paymentDate"`
	LastBuyDate string         `json:"lastBuyDate"`
}

// parse reads one dividend; ok=false for one declared as zero. No record
// date is an error: the estimate hangs on it.
func (w wireDividend) parse() (d DeclaredDividend, ok bool, err error) {
	per, err := w.DividendNet.parse()
	if err != nil {
		return DeclaredDividend{}, false, fmt.Errorf("dividendNet: %w", err)
	}
	if !per.Decimal().IsPositive() {
		return DeclaredDividend{}, false, nil
	}
	record, err := parseWireTime(w.RecordDate)
	if err != nil {
		return DeclaredDividend{}, false, fmt.Errorf("recordDate: %w", err)
	}
	if record.IsZero() {
		return DeclaredDividend{}, false, fmt.Errorf("a dividend of %s %s with no recordDate", per.Decimal(), per.Currency)
	}
	d = DeclaredDividend{PerShare: per, RecordDate: mskDay(record)}
	if d.PaymentDate, err = optionalDay(w.PaymentDate); err != nil {
		return DeclaredDividend{}, false, fmt.Errorf("paymentDate: %w", err)
	}
	if d.LastBuyDate, err = optionalDay(w.LastBuyDate); err != nil {
		return DeclaredDividend{}, false, fmt.Errorf("lastBuyDate: %w", err)
	}
	return d, true, nil
}

// optionalDay is a day the gateway may omit (nil). The calendar sends
// Moscow or UTC midnights depending on the paper; both are the same Moscow
// day.
func optionalDay(s string) (*time.Time, error) {
	t, err := parseWireTime(s)
	if err != nil || t.IsZero() {
		return nil, err
	}
	day := mskDay(t)
	return &day, nil
}
