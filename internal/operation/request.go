package operation

import (
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/platform/apitypes"
)

// BadFieldError is a request field this package could not read at all — a
// date that is not a date, a decimal that is not a number. It is a 400 with
// the message as it stands, and it is a TYPE rather than a sentence handed to
// a response writer because two doors now build an operation out of the same
// request shape: the journal's own create endpoint and the importer's
// "explain this broker row by hand" (see OperationFromCreateRequest). A
// message written at the point of failure is the same message at both.
type BadFieldError struct{ Message string }

func (e BadFieldError) Error() string { return e.Message }

// OperationFromCreateRequest turns the contract's CreateOperationRequest into
// the journal operation it describes, or names the first field it could not
// read.
//
// IT VALIDATES NOTHING BEYOND READABILITY. Whether a buy may carry a positive
// amount, whether a split may come from this source, whether the account
// exists — all of that is Service.Create's, and this deliberately does not
// anticipate any of it: a second opinion here would be a second rule to keep
// in step, and the two would disagree the first time one moved.
//
// The Source is left empty, which Service.Create reads as "manual". A caller
// that means something else sets it on the returned operation.
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
		AmountMinor:  req.AmountMinor,
		Currency:     req.Currency,
		FeeMinor:     feeMinor,
		Note:         note,
		SplitRatio:   splitRatio,
	}, nil
}
