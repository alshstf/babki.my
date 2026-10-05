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
	"babki.my/babki/internal/platform/dates"
	"babki.my/babki/internal/platform/httpjson"
)

// PaperReturnBasis reckons one paper's period, from (exclusive) to to
// (inclusive), across every account of the space it has been on: its worth at
// either end — all the units held, at that day's closing price — and the money
// that went into it or came out of it in between, in the currency it is valued
// in. A buy puts money in and a sale, a payout or a redemption takes it out,
// each with its commission; a tax withheld takes back part of a payout. A move
// between two of the family's accounts is no flow at all: the paper stays in
// the family. Shares that arrive from outside — from another broker, or as the
// result of a conversion — come in at that day's closing price, as on an
// account (see ReturnBasis), and shares converted away leave at it. A spin-off
// carries part of the paper's worth onto another paper, which no closing price
// says; a period with one is not complete.
func (h *Handler) PaperReturnBasis(ctx context.Context, spaceID, instrumentID uuid.UUID, from, to time.Time) (ReturnBasis, error) {
	history, ok := h.quotes.(quoteHistory)
	if !ok {
		return ReturnBasis{}, ErrNoQuoteHistory
	}
	finder, ok := h.ops.(holdingAccounts)
	if !ok {
		return ReturnBasis{}, errors.New("portfolio: the journal store cannot say which accounts hold a paper")
	}
	papers, err := h.instruments.ByIDs(ctx, []uuid.UUID{instrumentID})
	if err != nil {
		return ReturnBasis{}, err
	}
	paper, ok := papers[instrumentID]
	if !ok {
		return ReturnBasis{}, errInstrumentNotInCatalog
	}
	accounts, err := finder.AccountsWithInstrument(ctx, spaceID, instrumentID)
	if err != nil {
		return ReturnBasis{}, err
	}

	held := [2]decimal.Decimal{}
	var journals [][]Operation
	for _, accountID := range accounts {
		ops, err := h.ops.ListForEngine(ctx, spaceID, accountID)
		if err != nil {
			return ReturnBasis{}, err
		}
		journals = append(journals, ops)
		for i, day := range []time.Time{from, to} {
			positions, err := Compute(journalTo(ops, day))
			if err != nil {
				return ReturnBasis{}, journalDoesNotCompute{err}
			}
			if p, ok := positions[instrumentID]; ok && p.Quantity.IsPositive() {
				held[i] = held[i].Add(p.Quantity)
			}
		}
	}

	out := ReturnBasis{Currency: paper.Currency, Complete: true}
	for i, day := range []time.Time{from, to} {
		v := JournalValue{Currency: paper.Currency}
		if held[i].IsPositive() {
			quotes, err := history.QuotesOn(ctx, []uuid.UUID{instrumentID}, day)
			if err != nil {
				return ReturnBasis{}, err
			}
			q, quoted := quotes[instrumentID]
			if quoted && q.On.Before(day.AddDate(0, 0, -staleQuoteDays)) {
				quoted = false
			}
			minor, currency, gap, err := marketValue(paper.Type, paper.FaceValueMinor, paper.FaceCurrency, held[i], q, quoted)
			if err != nil {
				return ReturnBasis{}, err
			}
			if gap == valuationStruck {
				v.Minor, v.Currency = minor, currency
				out.Currency = currency
			} else {
				v.Unpriced = 1
				out.Complete = false
			}
		}
		if i == 0 {
			out.Start = v
		} else {
			out.End = v
		}
	}

	rates := marketdata.NewRateMemo(h.conv)
	for _, ops := range journals {
		for _, o := range ops {
			if o.InstrumentID == nil || *o.InstrumentID != instrumentID || !o.OccurredOn.After(from) || o.OccurredOn.After(to) {
				continue
			}
			flow, known, err := h.paperFlow(ctx, history, o)
			if err != nil {
				return ReturnBasis{}, err
			}
			if !known {
				out.Complete = false
				continue
			}
			if flow.minor == 0 {
				continue
			}
			converted, ok, err := h.sumInBase(ctx, []datedMinor{flow}, out.Currency, rates)
			if err != nil {
				return ReturnBasis{}, err
			}
			if !ok {
				out.Complete = false
				continue
			}
			out.Flows = append(out.Flows, ReturnFlow{Day: o.OccurredOn, Minor: converted})
		}
	}
	return out, nil
}

// journalTo is the part of a journal folded by the end of day.
func journalTo(ops []Operation, day time.Time) []Operation {
	var out []Operation
	for _, o := range ops {
		if !o.OccurredOn.After(day) {
			out = append(out, o)
		}
	}
	return out
}

// paperFlow is the money one row of the paper's journal moved into or out of
// it, signed from the investor's side (in negative); known is false when it
// cannot be had — a moved parcel with no price on its day, or a spin-off.
func (h *Handler) paperFlow(ctx context.Context, history quoteHistory, o Operation) (datedMinor, bool, error) {
	flow := datedMinor{from: o.Currency, on: o.OccurredOn}
	switch o.Type {
	case TypeTransferIn, TypeTransferOut:
		if o.TransferGroupID != nil {
			return flow, true, nil
		}
	case TypeExchangeIn, TypeExchangeOut, TypeSpinoffIn:
	case TypeSpinoffOut:
		return flow, false, nil
	default:
		flow.minor = o.AmountMinor - o.FeeMinor
		return flow, true, nil
	}
	worth, currency, known, err := h.parcelWorth(ctx, history, o)
	if err != nil || !known {
		return flow, false, err
	}
	flow.minor, flow.from = worth, currency
	if o.Type == TypeTransferIn || o.Type == TypeExchangeIn || o.Type == TypeSpinoffIn {
		flow.minor = -worth
	}
	return flow, true, nil
}

func (h *Handler) handlePaperReturn(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathInstrumentID(w, r)
	if !ok {
		return
	}
	from, errFrom := time.Parse(time.DateOnly, r.URL.Query().Get("from"))
	to, errTo := time.Parse(time.DateOnly, r.URL.Query().Get("to"))
	if errFrom != nil || errTo != nil || !from.Before(to) || to.After(dates.LatestRecordable()) {
		httpjson.Error(w, http.StatusBadRequest, "from and to must be YYYY-MM-DD, from before to, to at most today")
		return
	}
	basis, err := h.PaperReturnBasis(r.Context(), p.SpaceID, id, from, to)
	var notComputed journalDoesNotCompute
	switch {
	case errors.As(err, &notComputed):
		httpjson.Error(w, http.StatusUnprocessableEntity, notComputed.Error())
		return
	case errors.Is(err, errInstrumentNotInCatalog):
		httpjson.Error(w, http.StatusNotFound, "not found")
		return
	case err != nil:
		family.WriteError(w, err)
		return
	}
	out, err := periodReturnToAPI(basis, from, to)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, out)
}
