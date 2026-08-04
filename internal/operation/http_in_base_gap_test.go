package operation_test

import (
	"fmt"
	"testing"
)

// The journal used to publish a null in_base and leave the screen to guess at
// the reason, which it did with one sentence — «Нет курса на дату операции» —
// over every one of them. These tests pin the server's answer instead, one per
// branch where the conversion stops, and each asserts ITS OWN value rather than
// merely "some gap is set": a caption that names the wrong cause is worse than
// the vague one it replaced, because it reads as knowledge (#79).
//
// The three values, and what makes each of them the honest one:
//
//	no_rate_operation_date — the amount is money that moved on the row's own
//	        date and the fx table cannot reach that day. Closes on its own.
//	no_rate_lot_date       — the amount is a cost basis assembled from
//	        purchases on other days and one of THOSE days has no rate. The
//	        transfer's own day usually does, which is what makes the old
//	        sentence false rather than merely unhelpful. Closes on its own.
//	undated_lot            — nobody ever recorded when the parcel was bought.
//	        Never closes.

// TestJournalGapNamesTheOperationsOwnDate is the ordinary row: money that moved
// on the day the row is dated, and no rate for that day nor any earlier one.
// Here — and only here — the old blanket sentence happened to be true, so this
// case exists to keep the new field from over-correcting: the value must still
// be the one about the operation's own date.
func TestJournalGapNamesTheOperationsOwnDate(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	// Seeded AFTER the operation: nothing on or before 2026-01-05.
	seedFxRate(t, mdStore, "2026-05-13", "60.00")

	acc := mkAccount(t, url, c, "US брокер", "USD")
	wd := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2026-01-05","amount_minor":-10000,"currency":"USD"}`, acc))

	row := findOperation(t, listJournal(t, url, c, acc), wd)
	if row.InBase != nil {
		t.Fatalf("in_base = %+v, want null — no USD->RUB rate on or before 2026-01-05", *row.InBase)
	}
	if row.InBaseGap != "no_rate_operation_date" {
		t.Errorf("in_base_gap = %q, want no_rate_operation_date: this amount is money that moved on 2026-01-05 and that is the day whose rate is missing",
			row.InBaseGap)
	}
}

// TestJournalGapNamesThePurchaseDateNotTheTransferDate is the case issue #79 is
// named for, and the one the whole field exists to make sayable.
//
// The parcel was bought on 2026-01-05, a day the fx table cannot reach. It was
// moved between two accounts of the same family on 2026-07-20, a day whose rate
// IS seeded — the fixture proves it by converting an ordinary withdrawal on
// that very date in the same journal. So the row shows no ruble figure while
// the rate for its own date sits in the table unused, and the sentence «нет
// курса на дату операции» over it is not vague, it is false.
//
// Both legs are asserted. The breakdown is stored once, beside the arriving
// leg, and read onto the departing one (see Store.attachTransferLots): one
// parcel, one set of purchases, one answer — a pair that explained itself
// differently on the two accounts would be the same defect one screen away.
func TestJournalGapNamesThePurchaseDateNotTheTransferDate(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	// Nothing on or before the 2026-01-05 purchase; the transfer's own day has
	// a rate and so does the control row below.
	seedFxRate(t, mdStore, "2026-07-20", "78.50")

	from := mkAccount(t, url, c, "Т-Банк", "USD")
	to := mkAccount(t, url, c, "Freedom KZ", "USD")
	tsla := mkInstrument(t, url, c,
		`{"type":"share","name":"Tesla","ticker":"TSLA","currency":"USD"}`)
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-01-05","quantity":"10","price":"180","amount_minor":-180000,"currency":"USD"}`, from, tsla))
	// The control: an ordinary row on the transfer's own date. If this one fails
	// to convert, the fixture has stopped testing what it claims — the point is
	// that 2026-07-20's rate exists and is deliberately not the one that may
	// value shares bought in January.
	control := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2026-07-20","amount_minor":-10000,"currency":"USD"}`, from))

	// No cost_minor: the basis is released from the source's own lot, so the
	// purchase date travels with the parcel.
	pair := mkTransfer(t, url, c, fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-20"}`,
		from, to, tsla))

	source := listJournal(t, url, c, from)
	if row := findOperation(t, source, control); row.InBase == nil {
		t.Fatalf("the control withdrawal on 2026-07-20 did not convert, but that day's rate (78.50) is seeded — the fixture is not exercising the case it describes")
	}
	for _, leg := range []struct {
		name string
		row  journalItem
	}{
		{"transfer_out", findOperation(t, source, pair.Out.ID)},
		{"transfer_in", findOperation(t, listJournal(t, url, c, to), pair.In.ID)},
	} {
		if leg.row.InBase != nil {
			t.Fatalf("%s.in_base = %+v, want null — the 2026-01-05 purchase behind this basis has no rate", leg.name, *leg.row.InBase)
		}
		if leg.row.InBaseGap != "no_rate_lot_date" {
			t.Errorf("%s.in_base_gap = %q, want no_rate_lot_date. The missing rate is the PURCHASE day's (2026-01-05); the operation's own day (2026-07-20) has one and it is seeded, so no_rate_operation_date here would name a rate that exists (#79)",
				leg.name, leg.row.InBaseGap)
		}
		if leg.row.HasUndatedLots {
			t.Errorf("%s.has_undated_lots = true, but this parcel's purchase date is recorded — it is the RATE for it that is missing, and undated_lot would promise a figure that is never coming", leg.name)
		}
	}
}

// TestJournalGapNamesThePurchaseDateWhenOnlyAnOlderPieceLacksARate reaches the
// same value down the other road.
//
// A transfer's figure is struck at one rate per piece, and the headline — the
// newest purchase, the one rate_on names — is resolved separately from the rest
// (it also values the fee). Here the headline SUCCEEDS: the 2026-06-14 piece is
// valued from the Friday before. It is the older 2026-01-05 piece, deep in the
// loop, that the fx table cannot reach. Without this fixture the per-term branch
// is never exercised: every other case in this file stops on the headline, so an
// implementation that named its cause from anywhere at all would pass.
func TestJournalGapNamesThePurchaseDateWhenOnlyAnOlderPieceLacksARate(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	// Nothing on or before 2026-01-05; the newest purchase and the transfer day
	// are both reachable.
	seedFxRate(t, mdStore, "2026-06-12", "64.00")
	seedFxRate(t, mdStore, "2026-07-20", "78.50")

	from := mkAccount(t, url, c, "Т-Банк", "USD")
	to := mkAccount(t, url, c, "Freedom KZ", "USD")
	tsla := mkInstrument(t, url, c,
		`{"type":"share","name":"Tesla","ticker":"TSLA","currency":"USD"}`)
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-01-05","quantity":"5","price":"180","amount_minor":-90000,"currency":"USD"}`, from, tsla))
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-06-14","quantity":"5","price":"200","amount_minor":-100000,"currency":"USD"}`, from, tsla))

	pair := mkTransfer(t, url, c, fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-20"}`,
		from, to, tsla))

	row := findOperation(t, listJournal(t, url, c, to), pair.In.ID)
	if row.InBase != nil {
		t.Fatalf("in_base = %+v, want null — the 2026-01-05 piece of this parcel has no rate, and a basis summed from only the pieces that converted is smaller than the truth", *row.InBase)
	}
	if row.InBaseGap != "no_rate_lot_date" {
		t.Errorf("in_base_gap = %q, want no_rate_lot_date. The rate that is missing belongs to a PURCHASE day reached inside the per-piece loop, not to the headline (2026-06-14 resolves from the Friday before) and not to the operation's own day (2026-07-20 is seeded)",
			row.InBaseGap)
	}
}

// TestJournalGapNamesTheUndatedParcel covers the one cause no backfill closes:
// the basis was typed in by hand, so no purchase date exists for it anywhere.
//
// Every rate this row could possibly want is seeded, which is what separates
// this value from the two no_rate_* ones: nothing here is waiting on the fx
// table, and a caption promising the figure later would be a promise nobody can
// keep.
func TestJournalGapNamesTheUndatedParcel(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	seedFxRate(t, mdStore, "2026-05-13", "60.00")
	seedFxRate(t, mdStore, "2026-07-20", "78.50")

	from := mkAccount(t, url, c, "Т-Банк", "USD")
	to := mkAccount(t, url, c, "Freedom KZ", "USD")
	tsla := mkInstrument(t, url, c,
		`{"type":"share","name":"Tesla","ticker":"TSLA","currency":"USD"}`)
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-05-13","quantity":"10","price":"180","amount_minor":-180000,"currency":"USD"}`, from, tsla))

	// cost_minor given by hand: no source lots are released, so no acquisition
	// dates travel with the parcel and none exist for it anywhere.
	pair := mkTransfer(t, url, c, fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-20","cost_minor":190000}`,
		from, to, tsla))

	for _, leg := range []struct {
		name string
		row  journalItem
	}{
		{"transfer_out", findOperation(t, listJournal(t, url, c, from), pair.Out.ID)},
		{"transfer_in", findOperation(t, listJournal(t, url, c, to), pair.In.ID)},
	} {
		if leg.row.InBase != nil {
			t.Fatalf("%s.in_base = %+v, want null — a hand-typed basis has no purchase date to value it at", leg.name, *leg.row.InBase)
		}
		if leg.row.InBaseGap != "undated_lot" {
			t.Errorf("%s.in_base_gap = %q, want undated_lot. Every rate this row could want is seeded (2026-05-13 and 2026-07-20), so any no_rate_* value here blames the fx table for a date nobody ever wrote down and promises a figure that will never arrive",
				leg.name, leg.row.InBaseGap)
		}
	}
}

// TestJournalGapPrefersTheUndatedParcelOverAMissingRate pins the ORDER, which
// is load-bearing rather than incidental: the parcel's dates are settled before
// any rate is looked up, so a row that has both an undated piece and an
// unreachable fx table reports the piece.
//
// The two are not equally true to a reader. A missing rate is a gap the
// backfill closes on its own and the figure appears later; a purchase date
// nobody recorded never resolves. Reporting the closeable one on a row that
// also has the permanent one promises a number that is not coming.
func TestJournalGapPrefersTheUndatedParcelOverAMissingRate(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	// A rate exists, but far in the future of everything in this fixture: no
	// date any row here could ask about is reachable.
	seedFxRate(t, mdStore, "2030-01-09", "100")

	from := mkAccount(t, url, c, "Т-Банк", "USD")
	to := mkAccount(t, url, c, "Freedom KZ", "USD")
	tsla := mkInstrument(t, url, c,
		`{"type":"share","name":"Tesla","ticker":"TSLA","currency":"USD"}`)
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-05-13","quantity":"10","price":"180","amount_minor":-180000,"currency":"USD"}`, from, tsla))

	pair := mkTransfer(t, url, c, fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-20","cost_minor":190000}`,
		from, to, tsla))

	row := findOperation(t, listJournal(t, url, c, to), pair.In.ID)
	if row.InBaseGap != "undated_lot" {
		t.Errorf("in_base_gap = %q, want undated_lot. This row has no rate for any date it could name AND no purchase date at all; the permanent cause is the one to report, because the other one implies the figure arrives with the next backfill and it does not",
			row.InBaseGap)
	}
}

// TestJournalPublishesNoGapWhenThereIsAFigure and its base-currency twin below
// keep the field from degenerating into "in_base is null", which is the shape a
// reader can already see and which would caption two entirely different rows
// with the same sentence.
func TestJournalPublishesNoGapWhenThereIsAFigure(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	seedFxRate(t, mdStore, "2026-05-13", "60.00")

	acc := mkAccount(t, url, c, "US брокер", "USD")
	wd := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2026-05-13","amount_minor":-10000,"currency":"USD"}`, acc))

	row := findOperation(t, listJournal(t, url, c, acc), wd)
	if row.InBase == nil {
		t.Fatalf("in_base = null, but 2026-05-13's rate is seeded")
	}
	if row.InBaseGap != "" {
		t.Errorf("in_base_gap = %q, want null: nothing stopped this conversion", row.InBaseGap)
	}
}

// TestJournalPublishesNoGapWhenAlreadyInTheBaseCurrency covers the row where
// there was never anything to convert. in_base is null exactly as it is over a
// genuine gap, and the difference matters: «не пересчитано» over an amount that
// IS the base-currency amount is a caption about nothing.
func TestJournalPublishesNoGapWhenAlreadyInTheBaseCurrency(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	// A rate exists, so a null here can only come from the base-currency
	// short-circuit and never from a failed lookup.
	seedFxRate(t, mdStore, "2026-05-13", "60.00")

	acc := mkAccount(t, url, c, "Рублёвый брокер", "RUB")
	wd := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2026-05-13","amount_minor":-10000,"currency":"RUB"}`, acc))

	row := findOperation(t, listJournal(t, url, c, acc), wd)
	if row.InBase != nil {
		t.Fatalf("in_base = %+v, want null (already the base currency)", *row.InBase)
	}
	if row.InBaseGap != "" {
		t.Errorf("in_base_gap = %q, want null: this row's own amounts ARE the base-currency ones, so nothing was withheld and no cause exists to name",
			row.InBaseGap)
	}
}

// TestJournalDatedOnIsTheDateAskedForNotTheRatesOwn is issue #80 on an ordinary
// row. The operation is dated Sunday 2019-03-17; the CBR published nothing that
// weekend, so the rate comes from Friday 2019-03-15. rate_on has always said
// Friday, correctly — it is the rate actually used — and a caption reading it as
// the date the money moved names a day nothing happened on. dated_on says the
// Sunday.
func TestJournalDatedOnIsTheDateAskedForNotTheRatesOwn(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	seedFxRate(t, mdStore, "2019-03-15", "65")

	acc := mkAccount(t, url, c, "US брокер", "USD")
	wd := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2019-03-17","amount_minor":-10000,"currency":"USD"}`, acc))

	row := findOperation(t, listJournal(t, url, c, acc), wd)
	if row.InBase == nil {
		t.Fatalf("in_base = null, want a conversion at the nearest earlier (2019-03-15) rate")
	}
	if row.InBase.DatedOn != "2019-03-17" {
		t.Errorf("in_base.dated_on = %q, want 2019-03-17 — the day this figure belongs to, which is the operation's own date for an ordinary row",
			row.InBase.DatedOn)
	}
	if row.InBase.RateOn != "2019-03-15" {
		t.Errorf("in_base.rate_on = %q, want 2019-03-15 — the rate ACTUALLY used, from the Friday before", row.InBase.RateOn)
	}
	if row.InBase.DatedOn == row.InBase.RateOn {
		t.Errorf("dated_on and rate_on are both %q; on this fixture they must differ, or the field cannot be what tells an exact hit from a weekend fallback",
			row.InBase.DatedOn)
	}
}

// TestJournalDatedOnIsThePurchaseDateOnATransfer is issue #80 where it actually
// bites, and where no client can work around it.
//
// The parcel's newest purchase is Sunday 2026-06-14; its rate comes from Friday
// 2026-06-12. The transfer itself is dated 2026-07-20. All three dates are
// different, and the screen's sentence — "converted at the rates of the purchase
// dates, the newest of them being X" — is only true of the first. Before
// dated_on the client had rate_on and occurred_on to choose between, and BOTH
// are wrong here: one is a day nothing was bought on, the other is the day the
// paperwork moved, which is the one rate deliberately not used.
//
// A weekend purchase date is one way to reach this; a public holiday — a
// weekday with no CBR rate — is another, and it is why the field is not a
// weekend special case.
func TestJournalDatedOnIsThePurchaseDateOnATransfer(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	seedFxRate(t, mdStore, "2026-05-13", "60.00")
	// Friday's rate, for a parcel whose newest piece was bought on the Sunday.
	seedFxRate(t, mdStore, "2026-06-12", "64.00")
	seedFxRate(t, mdStore, "2026-07-20", "78.50")

	from := mkAccount(t, url, c, "Т-Банк", "USD")
	to := mkAccount(t, url, c, "Freedom KZ", "USD")
	tsla := mkInstrument(t, url, c,
		`{"type":"share","name":"Tesla","ticker":"TSLA","currency":"USD"}`)
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-05-13","quantity":"5","price":"180","amount_minor":-90000,"currency":"USD"}`, from, tsla))
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-06-14","quantity":"5","price":"200","amount_minor":-100000,"currency":"USD"}`, from, tsla))

	pair := mkTransfer(t, url, c, fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-20"}`,
		from, to, tsla))

	for _, leg := range []struct {
		name string
		row  journalItem
	}{
		{"transfer_out", findOperation(t, listJournal(t, url, c, from), pair.Out.ID)},
		{"transfer_in", findOperation(t, listJournal(t, url, c, to), pair.In.ID)},
	} {
		if leg.row.InBase == nil {
			t.Fatalf("%s.in_base = null, want a figure: every purchase date here reaches a rate", leg.name)
		}
		if !leg.row.AssembledFromLots {
			t.Fatalf("%s.assembled_from_lots = false; the fixture is not exercising a piece-by-piece conversion", leg.name)
		}
		if leg.row.InBase.DatedOn != "2026-06-14" {
			t.Errorf("%s.in_base.dated_on = %q, want 2026-06-14 — the newest PURCHASE in the parcel, which is what the figure's headline rate was asked for. 2026-06-12 is the rate's own date and nothing was bought on it; 2026-07-20 is the day the shares changed brokers, the one rate deliberately not used (#80)",
				leg.name, leg.row.InBase.DatedOn)
		}
		if leg.row.InBase.RateOn != "2026-06-12" {
			t.Errorf("%s.in_base.rate_on = %q, want 2026-06-12 — the rate ACTUALLY used for the newest purchase", leg.name, leg.row.InBase.RateOn)
		}
	}
}
