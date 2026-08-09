package tinvest

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// parseWireInt64 parses one of the REST gateway's string-encoded int64
// fields (protobuf's int64 goes over JSON as a decimal string, since a JSON
// number cannot hold the full int64 range without losing precision in some
// decoders — the published OpenAPI spec marks every such field
// `format: int64, type: string`). An absent field unmarshals to "", which is
// treated as 0 rather than a parse error, on the assumption that the
// gateway omits zero-valued fields entirely (protojson's default). That
// assumption is checked against the spec's documented examples, not against
// a live response: this session's live sandbox calls never exercised a
// populated int64-string field (GetAccounts has none, and the one
// OperationsAll run that reached the gateway saw an empty operations list —
// see the task report for why).
func parseWireInt64(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

// parseWireTime parses one of the gateway's date-time fields (RFC 3339,
// e.g. "2026-01-10T10:00:00Z" — protobuf's google.protobuf.Timestamp over
// JSON). An empty string returns the zero time and no error; callers that
// need to tell "absent" apart from "parses to the zero time" (Account's
// OpenedOn) check for "" themselves before calling this.
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

// wireMoneyValue mirrors the REST gateway's MoneyValue: units arrives as a
// JSON string (see parseWireInt64), nano as a plain JSON number (int32
// always fits exactly in a float64/json.Number, so no precision is at
// risk). Currency arrives lower-case ("rub"); parse normalizes it to upper
// case, since every currency code stored or compared elsewhere in this
// codebase is upper case.
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

// wireQuotation mirrors the REST gateway's Quotation: the same units+nano
// shape as wireMoneyValue, minus the currency (a Quotation is a bare
// number, e.g. an instrument quantity).
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

// wireError mirrors the REST gateway's error body (components/schemas/
// ErrorResponse in the published OpenAPI spec, re-checked 2026-08-05
// against RussianInvestments/investAPI's src/docs/swagger-ui/openapi.yaml):
// Code is the gRPC status code, Message is human text, and Description is
// the broker's own numeric business error code — e.g. 40003,
// "authentication token is missing or invalid", which the spec's own
// documented example pairs with HTTP 401 and gRPC code 16 (Unauthenticated).
//
// The spec declares Description's JSON type as a bare integer — every one
// of its ~400 example error bodies shows it unquoted (e.g. `description:
// 30011`) — but the live gateway does not honor that: a real 400 response
// captured against invest-public-api.tinkoff.ru during this package's own
// sandbox testing (see task-3-report.md) carried `"description":"30079"`,
// a quoted string, and json.Unmarshal into an int field fails outright on
// that shape. json.Number accepts either wire form as-is (it stores
// whatever text arrived and parses it on demand via Int64()), so this field
// uses that type instead of plain int.
type wireError struct {
	Code        int         `json:"code"`
	Message     string      `json:"message"`
	Description json.Number `json:"description"`
}

// tokenInvalidDescription is the broker's business error code for an
// unusable token (expired, revoked, or never valid) — see wireError.
const tokenInvalidDescription = 40003

// instrumentNotFoundDescription is the broker's business error code for an
// instrument it does not know — see wireError. Captured from the live
// gateway on 2026-08-05, asking InstrumentsService/GetInstrumentBy about an
// unknown uid: HTTP 404 with
// {"code":5,"message":"Instrument not found","description":"50002"}. Note
// the QUOTED form the live gateway used here too, which is why
// wireError.Description is a json.Number.
const instrumentNotFoundDescription = 50002

// wireAccount mirrors the REST gateway's Account (UsersService/GetAccounts
// and GetSandboxAccounts): Type and Status are left as the enum's wire
// strings (e.g. "ACCOUNT_TYPE_TINKOFF") rather than parsed further, exactly
// as OperationItem.Type/State are — this package hands them on verbatim and
// leaves interpreting them to the caller.
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

// wireOperationItem mirrors the REST gateway's OperationItem
// (OperationsService/GetOperationsByCursor). Only the fields OperationItem
// (client.go) surfaces are declared; the rest of the wire shape (name,
// class_code, trades_info, child_operations, cancel_date_time/reason,
// yield/yield_relative, quantity_rest, ...) is preserved verbatim in
// OperationItem.Raw instead of being modeled here, since no caller of this
// package needs it decoded yet.
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

// getOperationsByCursorRequest mirrors GetOperationsByCursorRequest. State
// is deliberately not a field here at all (not merely omitted via
// omitempty): the mirror is a wire, not a copy of an empty pointer, so
// "not present in the struct" reads directly as the request's guarantee
// that it never filters by state — the mirror is append-only ("зеркало и
// проекция"), so canceled and in-progress operations must reach it exactly
// as executed ones do.
type getOperationsByCursorRequest struct {
	AccountID string `json:"accountId"`
	From      string `json:"from,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
	Limit     int    `json:"limit"`
}

// wireGetOperationsByCursorResponse mirrors
// GetOperationsByCursorResponse. Items is decoded as raw JSON per element
// rather than straight into []wireOperationItem so that OperationItem.Raw
// can carry each element exactly as the gateway sent it, byte for byte.
type wireGetOperationsByCursorResponse struct {
	HasNext    bool              `json:"hasNext"`
	NextCursor string            `json:"nextCursor"`
	Items      []json.RawMessage `json:"items"`
}

// accountIDRequest is the shared {accountId} request body for
// OperationsService/GetPortfolio and OperationsService/GetPositions — both
// take exactly one field.
type accountIDRequest struct {
	AccountID string `json:"accountId"`
}

// wirePortfolioPosition mirrors PortfolioPosition (OperationsService/
// GetPortfolio's response). Only the fields PortfolioPosition (client.go)
// surfaces are modeled; the response carries many more (average price,
// expected yield, ...) that no caller of this package needs yet.
//
// Ticker is decoded because the reconciliation has to NAME a position that
// resolves to no instrument of ours, and the two identifiers that always
// arrive are a UUID and a twelve-character figi — neither of which a person
// reads. The gateway sends it on a portfolio position (seen on a live sandbox
// response, 2026-08-05, on a position this program does not account for at
// all), and an absent ticker is simply an empty string here.
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
}

// wireGetPositionsResponse mirrors PositionsResponse's two currency-balance
// arrays. Money and Blocked are independent lists of MoneyValue, one entry
// per currency that has a nonzero amount in that list — a currency present
// in one and absent from the other is not documented as an error case, so
// GetPositions (client.go) treats "absent" as zero rather than rejecting
// the response.
type wireGetPositionsResponse struct {
	Money   []wireMoneyValue `json:"money"`
	Blocked []wireMoneyValue `json:"blocked"`
}

// instrumentByRequest mirrors InstrumentRequest for the one lookup this
// package performs: by instrument_uid. classCode is omitted — it is
// required only when idType is ticker, which InstrumentByUID never sends.
type instrumentByRequest struct {
	IDType string `json:"idType"`
	ID     string `json:"id"`
}

// instrumentIDTypeUID is InstrumentIdType's wire value for a lookup by
// instrument_uid (INSTRUMENT_ID_TYPE_UID = 3 in the proto enum; the REST
// gateway sends and expects enum members by name, not by number).
const instrumentIDTypeUID = "INSTRUMENT_ID_TYPE_UID"

// wireInstrument mirrors the REST gateway's v1Instrument — the base
// Instrument message InstrumentsService/GetInstrumentBy actually returns.
//
// It carries no nominal and no blocked field. Checked two ways: the
// published OpenAPI spec (RussianInvestments/investAPI's
// src/docs/swagger-ui/openapi.yaml, schema v1Instrument, re-checked
// 2026-08-05 — no "nominal" or "blocked" property; the only blocked-shaped
// field on this message is "blockedTcaFlag", a service-contract lock, not
// the sanctions freeze this codebase's own "frozen" flag means), and a live
// sandbox GetInstrumentBy call for a bond, whose response carried no
// "nominal" key at all (this task's own review, not re-run in this fix). A
// bond's nominal lives on the separate Bond message — see
// BondNominalByUID (client.go).
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

// wireBondResponse mirrors BondResponse (InstrumentsService/BondBy), trimmed
// to the one field BondNominalByUID (client.go) needs: the wire schema
// (v1BondResponse -> v1Bond, per the published OpenAPI spec) nests it under
// "instrument", same as wireInstrumentResponse does for GetInstrumentBy.
type wireBondResponse struct {
	Instrument struct {
		Nominal        wireMoneyValue `json:"nominal"`
		InitialNominal wireMoneyValue `json:"initialNominal"`
	} `json:"instrument"`
}

// wireFindInstrumentResponse mirrors InstrumentsService/FindInstrument. Its
// element is a shape of its own (InstrumentShort) rather than the full
// instrument GetInstrumentBy returns, and it carries exactly the four fields
// the search is for.
type wireFindInstrumentResponse struct {
	Instruments []struct {
		UID            string `json:"uid"`
		ISIN           string `json:"isin"`
		Ticker         string `json:"ticker"`
		Name           string `json:"name"`
		ClassCode      string `json:"classCode"`
		Currency       string `json:"currency"`
		InstrumentKind string `json:"instrumentKind"`
	} `json:"instruments"`
}

// wireGetLastPricesResponse mirrors MarketDataService/GetLastPrices. The
// response carries NO CURRENCY — see LastPrice and migration 0017 for where the
// currency of a price comes from instead, and why it cannot be the catalog
// row's.
type wireGetLastPricesResponse struct {
	LastPrices []wireLastPrice `json:"lastPrices"`
}

type wireLastPrice struct {
	InstrumentUID string         `json:"instrumentUid"`
	Price         *wireQuotation `json:"price"`
	Time          string         `json:"time"`
	LastPriceType string         `json:"lastPriceType"`
}

// parse reports ok=false for an entry the broker sent no price for, which it
// really does send: the whole entry arrives with a uid and nothing else. A zero
// would be a price of nothing and is not what that means.
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
