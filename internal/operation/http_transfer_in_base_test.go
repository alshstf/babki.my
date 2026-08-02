package operation_test

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/marketdata"
)

// positionInBase mirrors the part of apitypes.PositionInBase these tests read.
type positionInBase struct {
	CostMinor int64  `json:"cost_minor"`
	Currency  string `json:"currency"`
	RateOn    string `json:"rate_on"`
}

// positionItem is the subset of apitypes.Position these tests care about.
type positionItem struct {
	Quantity  string          `json:"quantity"`
	CostMinor int64           `json:"cost_minor"`
	Currency  string          `json:"currency"`
	InBase    *positionInBase `json:"in_base"`
}

// listPositions fetches GET .../positions and decodes it.
func listPositions(t *testing.T, url string, c *http.Client, accountID string) []positionItem {
	t.Helper()
	resp := do(t, c, "GET", url+"/api/v1/accounts/"+accountID+"/positions", "")
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("list positions = %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Positions []positionItem `json:"positions"`
	}
	decodeJSON(t, resp, &out)
	return out.Positions
}

// TestTransferInBaseMatchesThePositionItProduces pins the one number the
// journal and the positions screen were saying differently for the same
// shares.
//
// A transfer's amount_minor is not money that moved on the transfer date: it
// is the cost basis of shares bought on other days, carried to another account
// of the same family. The journal used to convert every operation's amount at
// the rate of its own date, so this one was converted at the rate of the day
// the shares changed brokers — the one rate that has nothing to do with what
// they cost. The positions screen has converted each lot at the rate of its own
// purchase date since plan 6, so the two screens printed different rubles for
// the same shares, and the journal's was the wrong one.
//
// The demo instance's own numbers (see cmd/babki/seed.go), which is what makes
// this checkable by eye:
//
//	lot 1: 5 TSLA @ $180.00 on 2026-05-13 →  90_000 minor USD, rate that day 60.00 →  5_400_000 = 54 000,00 ₽
//	lot 2: 5 TSLA @ $200.00 on 2026-06-15 → 100_000 minor USD, rate that day 64.00 →  6_400_000 = 64 000,00 ₽
//	transferred whole on 2026-07-20 (rate that day 78.50)
//
//	basis, per purchase date: 5_400_000 + 6_400_000 = 11_800_000 = 118 000,00 ₽
//	basis at the transfer day: 190_000 × 78.50     = 14_915_000 = 149 150,00 ₽
//
// 149 150,00 ₽ is the number README.md and the seed call an invented one and
// TestPositionInBaseTransferredLotsKeepTheirPurchaseDates names as the wrong
// answer — and it was sitting in the journal, on the demo data, next to a
// position saying 118 000,00 ₽. Both figures below must be 11_800_000, and
// must be equal to each other: they describe the same purchases.
func TestTransferInBaseMatchesThePositionItProduces(t *testing.T) {
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

	resp := do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-20"}`,
		from, to, tsla))
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	decodeJSON(t, resp, &pair)

	const wantBase = int64(11_800_000)
	const collapsed = int64(14_915_000)

	row := findOperation(t, listJournal(t, url, c, to), pair.In.ID)
	if row.InBase == nil {
		t.Fatalf("transfer_in in_base = null, want a conversion from the purchase dates")
	}
	if row.AmountMinor != 190_000 {
		t.Errorf("transfer_in amount_minor = %d, want 190000 (the basis that moved, in USD)", row.AmountMinor)
	}
	if row.InBase.AmountMinor != wantBase {
		t.Errorf("journal row in_base.amount_minor = %d, want %d (118 000,00 ₽ — each piece at the rate of the day it was bought); %d is the same shares priced at the transfer day's rate",
			row.InBase.AmountMinor, wantBase, collapsed)
	}
	// The figure is struck at two rates, so rate_on names the newer of them —
	// the most recent purchase in the parcel — and never the transfer's own
	// date, which is the one rate deliberately not used.
	if row.InBase.RateOn != "2026-06-15" {
		t.Errorf("journal row in_base.rate_on = %q, want 2026-06-15 (the newest rate behind the figure, not the transfer's 2026-07-20)", row.InBase.RateOn)
	}

	positions := listPositions(t, url, c, to)
	if len(positions) != 1 {
		t.Fatalf("receiving account has %d positions, want 1", len(positions))
	}
	pos := positions[0]
	if pos.InBase == nil {
		t.Fatalf("position in_base = null, want a basis converted per lot")
	}
	if pos.CostMinor != row.AmountMinor {
		t.Errorf("position cost_minor = %d but the journal row says %d — same shares, same currency", pos.CostMinor, row.AmountMinor)
	}
	if pos.InBase.CostMinor != wantBase {
		t.Errorf("position in_base.cost_minor = %d, want %d", pos.InBase.CostMinor, wantBase)
	}
	if pos.InBase.CostMinor != row.InBase.AmountMinor {
		t.Errorf("the journal says these shares cost %d and the position says %d — one screen contradicting the other is the whole complaint",
			row.InBase.AmountMinor, pos.InBase.CostMinor)
	}
}

// TestTransferWithoutBreakdownStillConvertsOnItsOwnDate pins the case that
// must NOT change: a transfer whose basis was typed in by hand has no
// purchase dates behind it — no lots were released to produce that number —
// so the transfer's own date remains the only date it has, and the journal
// keeps converting it there. Inventing dates for it would be worse than a
// rough answer.
//
//	basis 190_000 minor USD given by hand, moved on 2026-07-20 (rate 78.50)
//	  → 190_000 × 78.50 = 14_915_000
//
// It is the same 14_915_000 the case above rejects, and that is the point:
// the number is wrong only when better dates exist. Here they do not.
func TestTransferWithoutBreakdownStillConvertsOnItsOwnDate(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	seedFxRate(t, mdStore, "2026-05-13", "60.00")
	seedFxRate(t, mdStore, "2026-07-20", "78.50")

	from := mkAccount(t, url, c, "Т-Банк", "USD")
	to := mkAccount(t, url, c, "Freedom KZ", "USD")
	tsla := mkInstrument(t, url, c,
		`{"type":"share","name":"Tesla","ticker":"TSLA","currency":"USD"}`)
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-05-13","quantity":"10","price":"180","amount_minor":-180000,"currency":"USD"}`, from, tsla))

	resp := do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-20","cost_minor":190000}`,
		from, to, tsla))
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer with a manual basis = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	decodeJSON(t, resp, &pair)

	row := findOperation(t, listJournal(t, url, c, to), pair.In.ID)
	if row.InBase == nil {
		t.Fatalf("transfer_in in_base = null, want a conversion on the transfer's own date")
	}
	if row.InBase.AmountMinor != 14_915_000 {
		t.Errorf("in_base.amount_minor = %d, want 14915000 (190000 × 78.50, the transfer's own date — there are no purchase dates behind a hand-typed basis)",
			row.InBase.AmountMinor)
	}
	if row.InBase.RateOn != "2026-07-20" {
		t.Errorf("in_base.rate_on = %q, want 2026-07-20", row.InBase.RateOn)
	}
}

// TestBothTransferLegsConvertAtThePurchaseDates is the other half of the same
// complaint, on the leg the first fix did not reach.
//
// The breakdown is stored next to the ARRIVING leg, so while only that leg
// read it, the departing one went on being converted the old way — at the rate
// of the day the shares changed brokers. On the demo data the source account's
// journal printed
//
//	transfer_out (Т-Банк):     190 000 × 78.50 = 14 915 000 = 149 150,00 ₽
//	transfer_in  (Freedom KZ): 5 400 000 + 6 400 000 = 11 800 000 = 118 000,00 ₽
//
// for one pair: same instrument, same quantity, same amount_minor, two
// different ruble figures, and the larger one is the very number README.md and
// cmd/babki/seed.go call invented. The pieces describe the parcel, not its
// arrival, so both legs are now converted from them.
func TestBothTransferLegsConvertAtThePurchaseDates(t *testing.T) {
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

	resp := do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-20"}`,
		from, to, tsla))
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	decodeJSON(t, resp, &pair)

	const wantBase = int64(11_800_000)
	const collapsed = int64(14_915_000)

	outRow := findOperation(t, listJournal(t, url, c, from), pair.Out.ID)
	inRow := findOperation(t, listJournal(t, url, c, to), pair.In.ID)
	if outRow.InBase == nil || inRow.InBase == nil {
		t.Fatalf("in_base: out = %+v, in = %+v — both legs describe the same purchases, so both convert or neither does",
			outRow.InBase, inRow.InBase)
	}
	if outRow.AmountMinor != inRow.AmountMinor {
		t.Fatalf("the legs carry %d and %d in USD — the rest of this test assumes one parcel",
			outRow.AmountMinor, inRow.AmountMinor)
	}
	if outRow.InBase.AmountMinor != wantBase {
		t.Errorf("transfer_out in_base.amount_minor = %d, want %d (118 000,00 ₽, each piece at the rate of the day it was bought); %d is the same shares priced on the day they changed brokers",
			outRow.InBase.AmountMinor, wantBase, collapsed)
	}
	if outRow.InBase.AmountMinor != inRow.InBase.AmountMinor {
		t.Errorf("the source's journal says %d ₽ and the destination's says %d ₽ about one transfer of the same ten shares",
			outRow.InBase.AmountMinor, inRow.InBase.AmountMinor)
	}
	// Same reasoning, same headline date: the newest purchase in the parcel,
	// never the transfer's own 2026-07-20.
	if outRow.InBase.RateOn != "2026-06-15" {
		t.Errorf("transfer_out in_base.rate_on = %q, want 2026-06-15", outRow.InBase.RateOn)
	}
	// And both say out loud that their figure was assembled, so the screen that
	// reads rate_on aloud cannot mistake it for an ordinary conversion date.
	if !outRow.InBase.AssembledFromLots || !inRow.InBase.AssembledFromLots {
		t.Errorf("assembled_from_lots: out = %v, in = %v, want true on both — rate_on here is one of several rates, not the rate",
			outRow.InBase.AssembledFromLots, inRow.InBase.AssembledFromLots)
	}
}

// TestBothTransferLegsGoNullTogetherWhenAPurchaseDateHasNoRate closes the
// asymmetry the review flagged as its own finding: with only the arriving leg
// converted from the pieces, a missing rate for one purchase date nulled that
// leg's in_base entirely while the departing leg carried on happily converting
// at the transfer day's rate — the screen showing an honest "not converted"
// marker next to another screen showing a confident, wrong number.
//
// Now that both legs are the same sum of the same terms, they answer the same
// way: publishing a basis built from only the purchases that happened to
// convert would be smaller than the truth and indistinguishable from it.
func TestBothTransferLegsGoNullTogetherWhenAPurchaseDateHasNoRate(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	// No rate on or before 2026-05-13, the first purchase's date; the second
	// purchase and the transfer day both have one.
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

	resp := do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-20"}`,
		from, to, tsla))
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	decodeJSON(t, resp, &pair)

	outRow := findOperation(t, listJournal(t, url, c, from), pair.Out.ID)
	inRow := findOperation(t, listJournal(t, url, c, to), pair.In.ID)
	if inRow.InBase != nil {
		t.Errorf("transfer_in in_base = %+v, want null: one of the purchases behind it has no rate", inRow.InBase)
	}
	if outRow.InBase != nil {
		t.Errorf("transfer_out in_base = %+v, want null for the same reason as its own pair — 14 915 000 here would be a confident wrong number sitting next to the destination's honest \"not converted\"",
			outRow.InBase)
	}
}

// TestBothTransferLegsGoNullTogetherWhenAPieceHasNoAcquisitionDate is the
// undated twin of the test above, and the journal's share of the change that
// lets a lot not know when it was acquired.
//
// A piece with no date is not damage and must not fail the request: it is what
// a parcel looks like once it contains shares that arrived by an earlier
// transfer carrying no dates, and are then moved on again. But there is no day
// to strike a rate on for that piece, so the amount cannot be assembled — and
// the alternatives are exactly the two figures this whole mechanism removed:
// converting the undatable piece at the transfer day's rate, or summing only
// the pieces that do have dates and publishing a basis smaller than the truth.
// Both legs therefore publish nothing, together, like any other term that
// cannot be valued.
//
//	lot 1: 5 @ $180.00 on 2026-05-13 →  90_000 minor USD — date erased below
//	lot 2: 5 @ $200.00 on 2026-06-15 → 100_000 minor USD, rate 64.00
//	transfer of all 10 on 2026-07-20 (rate 78.50)
//
// The two wrong answers are named in the failure messages: 14_915_000 (the
// whole 190_000 at the transfer day's rate) and 6_400_000 (the datable piece
// alone).
func TestBothTransferLegsGoNullTogetherWhenAPieceHasNoAcquisitionDate(t *testing.T) {
	pool, mdStore := newTestPool(t)
	url, c := newAPIOn(t, pool, marketdata.NewConverter(mdStore))
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

	resp := do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-20"}`,
		from, to, tsla))
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	decodeJSON(t, resp, &pair)
	inID, err := uuid.Parse(pair.In.ID)
	if err != nil {
		t.Fatalf("parse transfer_in id: %v", err)
	}

	// Both legs convert before the date is removed, so a null below can only
	// come from the missing date and not from some unrelated setup mistake.
	if row := findOperation(t, listJournal(t, url, c, from), pair.Out.ID); row.InBase == nil {
		t.Fatalf("fully dated transfer_out in_base = null, want a conversion before erasing anything")
	}

	// Take the first piece's acquisition date away — the state a piece is in
	// when the shares behind it arrived without one.
	ct, err := pool.Exec(t.Context(),
		`UPDATE operation_transfer_lots SET acquired_on = NULL
		 WHERE operation_id = $1 AND seq = 0`, inID)
	if err != nil {
		t.Fatalf("erase the piece's acquisition date: %v", err)
	}
	if ct.RowsAffected() != 1 {
		t.Fatalf("erasing the date affected %d rows, want 1", ct.RowsAffected())
	}

	// The request still succeeds: an undated piece is legitimate journal data,
	// unlike a breakdown that no longer sums (see
	// TestJournalRefusesTransferWithCorruptedBreakdown, which must 5xx).
	outRow := findOperation(t, listJournal(t, url, c, from), pair.Out.ID)
	inRow := findOperation(t, listJournal(t, url, c, to), pair.In.ID)

	for _, tc := range []struct {
		name string
		row  journalItem
	}{
		{"transfer_in", inRow},
		{"transfer_out", outRow},
	} {
		if tc.row.InBase == nil {
			continue
		}
		switch tc.row.InBase.AmountMinor {
		case 14_915_000:
			t.Errorf("%s in_base.amount_minor = 14915000 — the whole basis at the TRANSFER day's rate (190000 × 78.50), the invented figure the breakdown exists to replace", tc.name)
		case 6_400_000:
			t.Errorf("%s in_base.amount_minor = 6400000 — only the piece that still has a date (100000 × 64.00); a basis missing one of its pieces reads exactly like a smaller real one", tc.name)
		default:
			t.Errorf("%s in_base = %+v, want null: one piece of this parcel does not know when it was bought, so no set of rates answers for the amount", tc.name, *tc.row.InBase)
		}
	}
}
