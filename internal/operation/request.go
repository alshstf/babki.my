package operation

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/platform/apitypes"
)

// BadFieldError is a request field that could not be read at all (a date that
// is not a date, a decimal that is not a number): a 400 with the message as is.
// A type, because the journal's create endpoint and the importer's "explain this
// row" both build operations from the same request.
type BadFieldError struct{ Message string }

func (e BadFieldError) Error() string { return e.Message }

// OperationFromCreateRequest turns a CreateOperationRequest into the operation
// it describes, or names the first unreadable field. It checks readability only;
// every rule is Service.Create's. Source is left empty, which Create reads as
// manual.
func OperationFromCreateRequest(req apitypes.CreateOperationRequest) (Operation, error) {
	occurredOn, err := parseDate(req.OccurredOn)
	if err != nil {
		return Operation{}, BadFieldError{Message: "occurred_on " + err.Error()}
	}

	var settledOn *time.Time
	if req.SettledOn.IsSpecified() && !req.SettledOn.IsNull() {
		t, err := parseDate(req.SettledOn.MustGet())
		if err != nil {
			return Operation{}, BadFieldError{Message: "settled_on " + err.Error()}
		}
		settledOn = &t
	}

	quantity, err := nullableDecimal(req.Quantity, "quantity")
	if err != nil {
		return Operation{}, err
	}
	price, err := nullableDecimal(req.Price, "price")
	if err != nil {
		return Operation{}, err
	}
	splitRatio, err := nullableDecimal(req.SplitRatio, "split_ratio")
	if err != nil {
		return Operation{}, err
	}

	var instrumentID *uuid.UUID
	if req.InstrumentId.IsSpecified() && !req.InstrumentId.IsNull() {
		v := req.InstrumentId.MustGet()
		instrumentID = &v
	}

	amountMinor, err := requestAmount(Type(req.Type), req.AmountMinor, quantity, price)
	if err != nil {
		return Operation{}, err
	}

	feeMinor := int64(0)
	if req.FeeMinor != nil {
		feeMinor = *req.FeeMinor
	}
	note := ""
	if req.Note != nil {
		note = *req.Note
	}

	return Operation{
		AccountID:    req.AccountId,
		InstrumentID: instrumentID,
		Type:         Type(req.Type),
		OccurredOn:   occurredOn,
		SettledOn:    settledOn,
		Quantity:     quantity,
		Price:        price,
		AmountMinor:  amountMinor,
		Currency:     req.Currency,
		FeeMinor:     feeMinor,
		Note:         note,
		SplitRatio:   splitRatio,
	}, nil
}

// requestAmount is the amount a create request carries: the one it states, or —
// on a buy or a sell that gives both a quantity and a price and no amount — the
// one the server works out (see TradeAmountMinor). Anything else without an
// amount has none to work out and is refused by name.
func requestAmount(typ Type, given *int64, quantity, price *decimal.Decimal) (int64, error) {
	if given != nil {
		return *given, nil
	}
	if (typ != TypeBuy && typ != TypeSell) || quantity == nil || price == nil {
		return 0, BadFieldError{Message: "amount_minor is required; only a buy or a sell that gives both quantity and price may leave it to the server"}
	}
	amount, err := TradeAmountMinor(typ, *quantity, *price)
	if err != nil {
		return 0, BadFieldError{Message: "quantity × price does not fit in an amount"}
	}
	return amount, nil
}
