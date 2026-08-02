package operation_test

import (
	"fmt"
	"io"
	"testing"
)

// TestAssembledFromLotsSurvivesWhenTheOperationIsAlreadyInTheBaseCurrency is
// the exact gap #67 tracked: a family whose base currency is RUB (the setup
// default — see newAPIOn) moving RUB-denominated shares between two of its
// own RUB accounts. The transfer's parcel has a complete, dated breakdown —
// one purchase, one date, nothing missing — so has_undated_lots is false on
// both legs. in_base is nonetheless null on both, for the most ordinary of
// its three reasons: `currency` already equals the space's base currency, so
// there is nothing to convert at all, and no fx rate is even asked for.
//
// Before this fix, assembled_from_lots lived only inside in_base
// (OperationInBase.AssembledFromLots) and vanished together with it whenever
// in_base did — including for this exact reason. A client reading
// has_undated_lots (false, correctly) and in_base.assembled_from_lots
// (absent, because in_base itself is absent) had no way left to learn that
// this row's amount is a cost basis at all, and the only remaining way to
// find out was a client-side list of operation types — the copy of a server
// rule this whole branch removes. This is not a synthetic corner: a RUB
// space with RUB brokerage accounts, moving RUB-denominated shares between
// them, is the ordinary case for a Russian-resident owner, not the unusual
// one.
func TestAssembledFromLotsSurvivesWhenTheOperationIsAlreadyInTheBaseCurrency(t *testing.T) {
	url, c := newAPI(t) // RUB base currency, no fx converter needed at all

	from := mkAccount(t, url, c, "Т-Банк", "RUB")
	to := mkAccount(t, url, c, "Тинькофф ИИС", "RUB")
	sber := mkInstrument(t, url, c,
		`{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)

	buy := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-05-13","quantity":"10","price":"250","amount_minor":-250000,"currency":"RUB"}`, from, sber))

	resp := do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-20"}`,
		from, to, sber))
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	decodeJSON(t, resp, &pair)

	// The create/transfer response itself: assembled_from_lots is a property
	// of the operation, not of an in_base block this response never carries
	// (see the API contract), so it must already be true here — before the
	// journal is even read back.
	if !pair.Out.AssembledFromLots || !pair.In.AssembledFromLots {
		t.Errorf("transfer response assembled_from_lots: out = %v, in = %v, want true on both",
			pair.Out.AssembledFromLots, pair.In.AssembledFromLots)
	}

	outRow := findOperation(t, listJournal(t, url, c, from), pair.Out.ID)
	inRow := findOperation(t, listJournal(t, url, c, to), pair.In.ID)

	for _, leg := range []struct {
		name string
		row  journalItem
	}{{"transfer_out", outRow}, {"transfer_in", inRow}} {
		if leg.row.InBase != nil {
			t.Fatalf("%s.in_base = %+v, want null: currency already equals the base currency, so there is nothing to convert — the fixture is not testing the case it claims",
				leg.name, *leg.row.InBase)
		}
		if leg.row.HasUndatedLots {
			t.Errorf("%s.has_undated_lots = true, want false: the only purchase behind this parcel is dated 2026-05-13", leg.name)
		}
		if !leg.row.AssembledFromLots {
			t.Errorf("%s.assembled_from_lots = false, want true: this parcel has a full, dated breakdown, and the currency match must not hide that — this is the exact case #67 closes", leg.name)
		}
	}

	// The ordinary RUB buy on the same journal must not be swept up by the
	// fix: it carries no breakdown at all and is not a cost basis of anything.
	buyRow := findOperation(t, listJournal(t, url, c, from), buy)
	if buyRow.AssembledFromLots {
		t.Errorf("the ordinary RUB buy reports assembled_from_lots = true; its amount is money that moved on its own day, not a basis assembled from purchases")
	}
	if buyRow.InBase != nil {
		t.Errorf("the ordinary RUB buy has a non-null in_base %+v; it is already in the base currency and should convert to nothing at all", *buyRow.InBase)
	}
}
