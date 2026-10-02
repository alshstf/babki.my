package portfolio

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/dates"
	"babki.my/babki/internal/platform/httpjson"
)

// ManualPriceSource is what the quotes table calls a price a person stated.
const ManualPriceSource = "manual"

// ErrNoQuoteWriter is a quote store this handler cannot write to.
var ErrNoQuoteWriter = errors.New("portfolio: the quote store takes no prices")

type quoteWriter interface {
	UpsertQuotes(ctx context.Context, quotes []marketdata.Quote) error
}

func (h *Handler) handleStatePrice(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("instrumentId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid instrumentId")
		return
	}
	var req apitypes.StateInstrumentPriceJSONRequestBody
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	on, err := time.Parse(time.DateOnly, req.On)
	if err != nil || on.After(dates.LatestRecordable()) || on.Before(dates.EarliestRecordable()) {
		httpjson.Error(w, http.StatusBadRequest, "on must be a YYYY-MM-DD date, not in the future")
		return
	}
	price, err := decimal.NewFromString(req.Price)
	if err != nil || !price.IsPositive() {
		httpjson.Error(w, http.StatusBadRequest, "price must be a positive decimal")
		return
	}
	if err := h.StatePrice(r.Context(), id, on, price); err != nil {
		if errors.Is(err, errInstrumentNotInCatalog) {
			httpjson.Error(w, http.StatusNotFound, "not found")
			return
		}
		family.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// StatePrice records a price a person stated for a paper on a day, in the
// paper's currency, as a quote of its own source.
func (h *Handler) StatePrice(ctx context.Context, instrumentID uuid.UUID, on time.Time, price decimal.Decimal) error {
	writer, ok := h.quotes.(quoteWriter)
	if !ok {
		return ErrNoQuoteWriter
	}
	papers, err := h.instruments.ByIDs(ctx, []uuid.UUID{instrumentID})
	if err != nil {
		return err
	}
	paper, ok := papers[instrumentID]
	if !ok {
		return errInstrumentNotInCatalog
	}
	return writer.UpsertQuotes(ctx, []marketdata.Quote{{
		InstrumentID: instrumentID, On: on, Price: price, Currency: paper.Currency, Source: ManualPriceSource,
	}})
}
