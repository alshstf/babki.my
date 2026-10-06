package operation_test

import (
	"fmt"
	"io"
	"testing"

	"babki.my/babki/internal/platform/apitest"
)

// has_undated_lots says why in_base is null when the cause is an unrecorded
// purchase date, which never resolves, unlike a missing rate. The twin of
// TestPositionSaysWhenALotHasNoAcquisitionDate. The transfer day here has a rate,
// so "no rate for the operation date" would be false.
//
//	hand-typed transfer, both legs  has_undated_lots true,  in_base null
//	buy with a rate on its day      has_undated_lots false, in_base present
//	buy older than every rate       has_undated_lots false, in_base null
func TestJournalSaysWhenATransferHasNoPurchaseDates(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	// Deliberately no rate on 2026-01-05: that is the early buy's own day.
	seedFxRate(t, mdStore, "2026-05-13", "60.00")
	seedFxRate(t, mdStore, "2026-07-20", "78.50")

	from := mkAccount(t, url, c, "Т-Банк", "USD")
	to := mkAccount(t, url, c, "Freedom KZ", "USD")
	tsla := mkInstrument(t, url, c,
		`{"type":"share","name":"Tesla","ticker":"TSLA","currency":"USD"}`)
	acme := mkInstrument(t, url, c,
		`{"type":"share","name":"Acme","ticker":"ACME","currency":"USD"}`)

	dated := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-05-13","quantity":"10","price":"180","amount_minor":-180000,"currency":"USD"}`, from, tsla))
	tooEarly := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-01-05","quantity":"1","price":"50","amount_minor":-5000,"currency":"USD"}`, from, acme))

	// cost_minor given by hand: no source lots are released, so no acquisition
	// dates travel with the parcel and none exist for it anywhere.
	resp := apitest.Do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-20","cost_minor":190000}`,
		from, to, tsla))
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer with a manual basis = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	apitest.Decode(t, resp, &pair)

	source := listJournal(t, url, c, from)
	outRow := findOperation(t, source, pair.Out.ID)
	inRow := findOperation(t, listJournal(t, url, c, to), pair.In.ID)

	for _, leg := range []struct {
		name string
		row  journalItem
	}{{"transfer_out", outRow}, {"transfer_in", inRow}} {
		if !leg.row.HasUndatedLots {
			t.Errorf("%s.has_undated_lots = false, want true: this parcel's basis was typed in by hand, so no purchase date exists for it anywhere — and the rate for its own date, 78.50 on 2026-07-20, is seeded and must not be offered as the missing piece",
				leg.name)
		}
		if leg.row.InBase != nil {
			t.Errorf("%s.in_base = %+v, want null alongside has_undated_lots", leg.name, *leg.row.InBase)
		}
	}

	datedRow := findOperation(t, source, dated)
	if datedRow.HasUndatedLots {
		t.Errorf("the ordinary buy reports has_undated_lots = true; an operation's own amount belongs to the day it happened and needs no purchase date")
	}
	if datedRow.InBase == nil {
		t.Errorf("the ordinary buy has no in_base, but its own day's rate (60.00 on 2026-05-13) is seeded — the fixture assumption the row above rests on")
	}

	earlyRow := findOperation(t, source, tooEarly)
	if earlyRow.InBase != nil {
		t.Fatalf("the 2026-01-05 buy converted to %+v, but no rate that far back was seeded — the fixture is not testing what it claims", *earlyRow.InBase)
	}
	if earlyRow.HasUndatedLots {
		t.Errorf("the 2026-01-05 buy reports has_undated_lots = true merely because it could not be converted. That row's date is written down and its rate arrives with the next backfill; telling the reader a permanent, unrecoverable gap here is the exact confusion this field exists to remove")
	}
}

// The pair agrees with itself in the 201 that creates it as well as in the
// journal: the departing leg returns the breakdown it shares.
func TestTransferPairAnswersUndatedTheSameOnBothLegs(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	seedFxRate(t, mdStore, "2026-05-13", "60.00")
	seedFxRate(t, mdStore, "2026-06-15", "64.00")
	seedFxRate(t, mdStore, "2026-07-20", "78.50")

	from := mkAccount(t, url, c, "Т-Банк", "USD")
	to := mkAccount(t, url, c, "Freedom KZ", "USD")
	tsla := mkInstrument(t, url, c,
		`{"type":"share","name":"Tesla","ticker":"TSLA","currency":"USD"}`)
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-05-13","quantity":"5","price":"180","amount_minor":-90000,"currency":"USD"}`, from, tsla))
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-06-15","quantity":"5","price":"200","amount_minor":-100000,"currency":"USD"}`, from, tsla))

	// No cost_minor: the basis is released from the source's own lots, so both
	// purchase dates travel with the parcel.
	resp := apitest.Do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-20"}`,
		from, to, tsla))
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	apitest.Decode(t, resp, &pair)

	if pair.Out.HasUndatedLots {
		t.Errorf("the transfer response's departing leg says has_undated_lots = true about a parcel released from two dated lots (2026-05-13 and 2026-06-15) — the arriving leg of the same 201 says false, so one transfer contradicts itself inside one response")
	}
	if pair.In.HasUndatedLots {
		t.Errorf("the transfer response's arriving leg says has_undated_lots = true about a parcel released from two dated lots")
	}

	outRow := findOperation(t, listJournal(t, url, c, from), pair.Out.ID)
	inRow := findOperation(t, listJournal(t, url, c, to), pair.In.ID)
	if outRow.HasUndatedLots || inRow.HasUndatedLots {
		t.Errorf("journal has_undated_lots = out %v / in %v, want false on both: every piece of this parcel carries the day it was bought",
			outRow.HasUndatedLots, inRow.HasUndatedLots)
	}
	// The dated case must still publish its figure — a flag that reads false
	// while the conversion silently vanished would prove nothing.
	if outRow.InBase == nil || inRow.InBase == nil {
		t.Fatalf("in_base = out %+v / in %+v, want both published: every purchase date behind this parcel has a rate", outRow.InBase, inRow.InBase)
	}
}

// Shares that arrived with no price count as bought for nothing, and zero
// needs no date (#226): the journal shows 0 in roubles, as the position does.
// Moved together with bought shares, the parcel is valued by the bought ones.
func TestATransferBoughtForNothingIsNoughtInTheBaseCurrency(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	seedFxRate(t, mdStore, "2026-07-20", "78.50")
	seedFxRate(t, mdStore, "2026-08-03", "80.00")

	to := mkAccount(t, url, c, "Freedom KZ", "USD")
	onward := mkAccount(t, url, c, "Т-Банк", "USD")
	tsla := mkInstrument(t, url, c, `{"type":"share","name":"Tesla","ticker":"TSLA","currency":"USD"}`)

	// Shares from another broker, their purchases not stated.
	resp := apitest.Do(t, c, "POST", url+"/api/v1/operations/arrivals", fmt.Sprintf(
		`{"account_id":%q,"instrument_id":%q,"occurred_on":"2026-07-20","quantity":"10","currency":"USD"}`, to, tsla))
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("arrival = %d: %s", resp.StatusCode, b)
	}
	var arrived journalItem
	apitest.Decode(t, resp, &arrived)
	row := findOperation(t, listJournal(t, url, c, to), arrived.ID)
	if row.HasUndatedLots || row.InBase == nil || row.InBase.AmountMinor != 0 ||
		row.InBase.RateOn != "2026-07-20" || row.InBase.DatedOn != "2026-07-20" {
		t.Errorf("arrival %+v, want 0 in rubles at its own date and no missing purchase date", row)
	}

	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-08-03","quantity":"5","price":"200","amount_minor":-100000,"currency":"USD"}`, to, tsla))
	resp = apitest.Do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"15","occurred_on":"2026-08-10"}`,
		to, onward, tsla))
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("moving the parcel on = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	apitest.Decode(t, resp, &pair)
	// 1 000 $ bought on 2026-08-03 at 80 and 10 shares for nothing.
	row = findOperation(t, listJournal(t, url, c, onward), pair.In.ID)
	if row.HasUndatedLots || row.InBase == nil || row.InBase.AmountMinor != 8_000_000 || row.InBase.DatedOn != "2026-08-03" {
		t.Errorf("onward leg %+v, want 80 000 ₽ from the bought shares alone", row)
	}
}
