package operation_test

import (
	"fmt"
	"io"
	"testing"
)

// TestJournalSaysWhenATransferHasNoPurchaseDates is what lets the journal
// explain a missing ruble figure instead of guessing at it — the twin of
// TestPositionSaysWhenALotHasNoAcquisitionDate in package portfolio_test, and
// here for the identical reason.
//
// A row whose in_base is null already tells a reader that no base-currency
// figure could be published; it does not tell them WHY, and the two reasons are
// not interchangeable. "No fx rate for that day" is a gap the backfill job
// closes on its own — the number appears later. "Nobody recorded when these
// shares were bought" never resolves.
//
// On a journal row the difference is sharper than on a position, which is what
// makes the wrong sentence not merely unhelpful but false: a transfer's own
// date usually HAS a rate — this fixture seeds one for 2026-07-20, the day the
// shares moved — and it is precisely the rate that must not value a basis
// assembled on other days. A screen saying "нет курса на дату операции" over
// that row names a rate that exists, blames a cause that is not the cause, and
// promises a figure that will never come.
//
// FOUR rows in TWO journals pin it, and each kills a different wrong
// implementation:
//
//	transfer_out / transfer_in of a hand-typed basis — has_undated_lots true,
//	        in_base null, on BOTH legs: one parcel, one answer.
//	an ordinary buy with a rate on its own day — has_undated_lots false,
//	        in_base present. (Kills "always true".)
//	an ordinary buy dated EARLIER than any seeded rate — has_undated_lots
//	        false, in_base null. (Kills "has_undated_lots is in_base == null
//	        renamed" — the row that most needs the two told apart is this one.)
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
	resp := do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-20","cost_minor":190000}`,
		from, to, tsla))
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer with a manual basis = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	decodeJSON(t, resp, &pair)

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

// TestTransferPairAnswersUndatedTheSameOnBothLegs pins that the pair agrees
// with itself in the response that creates it, not only in the journal read
// back afterwards.
//
// The breakdown is stored next to the arriving leg alone, but it describes THE
// PARCEL, and every later read hands it to both legs for exactly that reason
// (see Store.attachTransferLots). The create path used to return the departing
// leg without it, which cost nothing while nothing was published from it — and
// the moment has_undated_lots was, that leg answered "these shares do not know
// when they were bought" about a parcel whose purchase dates had just been
// written in the same transaction. One transfer would then have contradicted
// itself inside a single 201 response.
//
// Both legs are checked in both places: the response and the journal listing.
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
	resp := do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-20"}`,
		from, to, tsla))
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	decodeJSON(t, resp, &pair)

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
