package operation_test

import (
	"fmt"
	"testing"
)

// Each branch where conversion stops publishes its own in_base_gap, and each
// test asserts that value: a caption naming the wrong cause reads as knowledge
// (#79).
//
// 	no_rate_operation_date  money moved on the row's date, which has no rate
// 	no_rate_lot_date        a purchase day behind a basis has no rate
// 	undated_lot             nobody recorded when the parcel was bought; never
// 	                        closes

// Money that moved on the row's date, no rate for it: the operation-date
// gap.
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

// The parcel was bought on 2026-01-05, unreachable; it moved on 2026-07-20,
// which has a rate (a control withdrawal that day converts). The gap is the
// purchase date, on both legs.
func TestJournalGapNamesThePurchaseDateNotTheTransferDate(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	// Nothing on or before 2026-01-05; the transfer day has a rate.
	seedFxRate(t, mdStore, "2026-07-20", "78.50")

	from := mkAccount(t, url, c, "Т-Банк", "USD")
	to := mkAccount(t, url, c, "Freedom KZ", "USD")
	tsla := mkInstrument(t, url, c,
		`{"type":"share","name":"Tesla","ticker":"TSLA","currency":"USD"}`)
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-01-05","quantity":"10","price":"180","amount_minor":-180000,"currency":"USD"}`, from, tsla))
	// The control: an ordinary row on the transfer day converts.
	control := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2026-07-20","amount_minor":-10000,"currency":"USD"}`, from))

	// No cost_minor: the basis and its purchase date come from the source's
	// lot.
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

// The headline (the newest piece, 2026-06-14, valued from Friday) resolves;
// the older 2026-01-05 piece does not. This is the only fixture reaching the
// per-term branch.
func TestJournalGapNamesThePurchaseDateWhenOnlyAnOlderPieceLacksARate(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	// Nothing on or before 2026-01-05; the newest purchase and the transfer
	// day are reachable.
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

// A basis typed by hand has no purchase date at all; every rate is seeded,
// so nothing is waiting on the fx table.
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

	// cost_minor by hand: no lots released, no dates carried.
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

// An undated piece and a missing rate together report the undated piece,
// which never closes; the missing rate would promise a figure that is not
// coming.
func TestJournalGapPrefersTheUndatedParcelOverAMissingRate(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	// A rate exists, but after every date here.
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

// A row with a figure publishes no gap.
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

// A row already in the base currency publishes no gap either.
func TestJournalPublishesNoGapWhenAlreadyInTheBaseCurrency(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	// A rate exists, so null comes from the base-currency case only.
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

// #80 on an ordinary row: dated Sunday 2019-03-17, rate from Friday
// 2019-03-15. rate_on says Friday, dated_on says Sunday.
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

// #80 on a transfer: newest purchase Sunday 2026-06-14, rate from Friday
// 2026-06-12, transfer on 2026-07-20. Neither rate_on nor occurred_on is the
// purchase day; dated_on is. A weekday holiday reaches the same case.
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
