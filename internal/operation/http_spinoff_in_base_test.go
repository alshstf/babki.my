package operation_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
)

// TestSpinoffLegsInTheJournalConvertAtThePurchaseDates: both legs of a spin-off
// carry a cost basis, not money, so the journal converts them piece by piece at
// the rates of the days the parcels were bought — exactly as it does a
// transfer's legs — and the listing answers at all. The departing leg has no
// quantity, which the transfer-shaped check used to dereference.
//
//	lot 1:  90 000 minor USD bought 2026-05-13 at 60.00
//	lot 2: 100 000 minor USD bought 2026-06-15 at 64.00
//	a quarter moves: 22 500 + 25 000 = 47 500 minor USD
//	in rubles: 22 500 x 60 + 25 000 x 64 = 2 950 000 kopecks
func TestSpinoffLegsInTheJournalConvertAtThePurchaseDates(t *testing.T) {
	pool, mdStore := newTestPool(t)
	url, c := newAPIOn(t, pool, marketdata.NewConverter(mdStore))
	seedFxRate(t, mdStore, "2026-05-13", "60.00")
	seedFxRate(t, mdStore, "2026-06-15", "64.00")
	seedFxRate(t, mdStore, "2026-07-20", "78.50")

	acc := mkAccount(t, url, c, "Брокер", "USD")
	fund := mkInstrument(t, url, c, `{"type":"etf","name":"Fund","ticker":"FUND","currency":"USD"}`)
	carved := mkInstrument(t, url, c, `{"type":"etf","name":"Fund blocked","ticker":"FUND2","currency":"USD"}`)
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-05-13","quantity":"5","price":"180","amount_minor":-90000,"currency":"USD"}`, acc, fund))
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-06-15","quantity":"5","price":"200","amount_minor":-100000,"currency":"USD"}`, acc, fund))

	var spaceID uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT id FROM spaces`).Scan(&spaceID); err != nil {
		t.Fatalf("space id: %v", err)
	}
	svc := operation.NewService(operation.NewStore(pool))
	out, in, err := svc.CreateSpinoff(t.Context(), spaceID, operation.SpinoffParams{
		AccountID:        uuid.MustParse(acc),
		FromInstrumentID: uuid.MustParse(fund),
		ToInstrumentID:   uuid.MustParse(carved),
		RatioFrom:        decimal.RequireFromString("1"),
		RatioTo:          decimal.RequireFromString("1"),
		BasisShare:       decimal.RequireFromString("0.25"),
		OccurredOn:       mustDate(t, "2026-07-20"),
		Source:           operation.SourceRegistry,
		Note:             "spin-off",
	})
	if err != nil {
		t.Fatalf("CreateSpinoff: %v", err)
	}

	journal := listJournal(t, url, c, acc)
	const wantBase = int64(2_950_000)
	for _, leg := range []struct {
		name string
		id   uuid.UUID
	}{{"spinoff_out", out.ID}, {"spinoff_in", in.ID}} {
		row := findOperation(t, journal, leg.id.String())
		if row.AmountMinor != 47_500 {
			t.Fatalf("%s carries %d minor USD, want 47500", leg.name, row.AmountMinor)
		}
		if row.InBase == nil {
			t.Fatalf("%s in_base is null — every piece has a date and every date a rate", leg.name)
		}
		if row.InBase.AmountMinor != wantBase {
			t.Errorf("%s in_base.amount_minor = %d, want %d: each piece at the rate of the day it was bought, not at the spin-off day's 78.50",
				leg.name, row.InBase.AmountMinor, wantBase)
		}
		if row.InBase.RateOn != "2026-06-15" {
			t.Errorf("%s in_base.rate_on = %q, want 2026-06-15, the newest purchase in the parcel", leg.name, row.InBase.RateOn)
		}
		if !row.AssembledFromLots {
			t.Errorf("%s assembled_from_lots = false, want true", leg.name)
		}
	}
}
