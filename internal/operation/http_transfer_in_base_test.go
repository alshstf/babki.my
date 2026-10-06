package operation_test

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitest"
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
	resp := apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+accountID+"/positions", "")
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("list positions = %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Positions []positionItem `json:"positions"`
	}
	apitest.Decode(t, resp, &out)
	return out.Positions
}

// A transfer's amount is the basis of shares bought on other days, so the
// journal converts it per purchase date and agrees with the position. The demo
// instance's numbers (cmd/babki/seed.go):
//
//	lot 1: 5 TSLA @ $180.00 on 2026-05-13 ->  90_000 USD, rate 60.00 -> 5_400_000
//	lot 2: 5 TSLA @ $200.00 on 2026-06-15 -> 100_000 USD, rate 64.00 -> 6_400_000
//	transferred whole on 2026-07-20 (rate 78.50)
//
//	per purchase date:    5_400_000 + 6_400_000 = 11_800_000
//	at the transfer day:  190_000 × 78.50       = 14_915_000 (wrong)
//
// Both figures below must be 11_800_000.
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

	resp := apitest.Do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-20"}`,
		from, to, tsla))
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	apitest.Decode(t, resp, &pair)

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
	// Struck at two rates, rate_on names the newer purchase, never the
	// transfer day.
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

// A transfer with no breakdown (a basis given by hand) has no purchase date,
// so neither leg publishes a base-currency figure, matching the position built
// from the undated lot (TestPositionInBaseNullWhenALotHasNoAcquisitionDate).
//
//	190_000 USD given by hand, moved on 2026-07-20 (rate 78.50)
//	190_000 × 78.50 = 14_915_000 would be invented
func TestTransferWithoutBreakdownHasNoRubleEquivalentEither(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	seedFxRate(t, mdStore, "2026-05-13", "60.00")
	seedFxRate(t, mdStore, "2026-07-20", "78.50")

	from := mkAccount(t, url, c, "Т-Банк", "USD")
	to := mkAccount(t, url, c, "Freedom KZ", "USD")
	tsla := mkInstrument(t, url, c,
		`{"type":"share","name":"Tesla","ticker":"TSLA","currency":"USD"}`)
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-05-13","quantity":"10","price":"180","amount_minor":-180000,"currency":"USD"}`, from, tsla))

	resp := apitest.Do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-20","cost_minor":190000}`,
		from, to, tsla))
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer with a manual basis = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	apitest.Decode(t, resp, &pair)

	outRow := findOperation(t, listJournal(t, url, c, from), pair.Out.ID)
	inRow := findOperation(t, listJournal(t, url, c, to), pair.In.ID)

	if inRow.InBase != nil {
		if inRow.InBase.AmountMinor == 14_915_000 {
			t.Fatalf("transfer_in in_base.amount_minor = 14915000 — the basis at the TRANSFER day's rate (190000 × 78.50); nobody recorded that day as a purchase date, so no rate honestly answers for this basis, exactly like the position built from the same lot")
		}
		t.Fatalf("transfer_in in_base = %+v, want null: a hand-typed basis has no purchase date behind it", *inRow.InBase)
	}
	if outRow.InBase != nil {
		if outRow.InBase.AmountMinor == 14_915_000 {
			t.Fatalf("transfer_out in_base.amount_minor = 14915000 — the same invented figure on the departing leg; both legs describe one undated parcel")
		}
		t.Fatalf("transfer_out in_base = %+v, want null: both legs describe the same undated basis", *outRow.InBase)
	}
}

// Both legs are converted from the breakdown. While only the arriving leg read
// it, the demo's source journal printed 14 915 000 for the departing leg and
// 11 800 000 for the arriving one: the same pair, two figures.
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

	resp := apitest.Do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-20"}`,
		from, to, tsla))
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	apitest.Decode(t, resp, &pair)

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
	// Same headline date on the departing leg.
	if outRow.InBase.RateOn != "2026-06-15" {
		t.Errorf("transfer_out in_base.rate_on = %q, want 2026-06-15", outRow.InBase.RateOn)
	}
	// Both say their figure was assembled (#67).
	if !outRow.AssembledFromLots || !inRow.AssembledFromLots {
		t.Errorf("assembled_from_lots: out = %v, in = %v, want true on both — rate_on here is one of several rates, not the rate",
			outRow.AssembledFromLots, inRow.AssembledFromLots)
	}
}

// A missing rate for one purchase date nulls both legs together; a basis from
// only the pieces that converted would be smaller than the truth.
func TestBothTransferLegsGoNullTogetherWhenAPurchaseDateHasNoRate(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	// No rate on or before 2026-05-13; the second purchase and the transfer
	// day have one.
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

	resp := apitest.Do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-20"}`,
		from, to, tsla))
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	apitest.Decode(t, resp, &pair)

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

// A piece with no purchase date is legitimate and must not fail the request,
// but it has no day to be valued at, so both legs publish nothing.
//
//	lot 1: 5 @ $180.00 on 2026-05-13 ->  90_000 USD, date erased below
//	lot 2: 5 @ $200.00 on 2026-06-15 -> 100_000 USD, rate 64.00
//	transfer of all 10 on 2026-07-20 (rate 78.50)
//
// Wrong answers: 14_915_000 (all at the transfer day) and 6_400_000 (the dated
// piece alone).
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

	resp := apitest.Do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-20"}`,
		from, to, tsla))
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	apitest.Decode(t, resp, &pair)
	inID, err := uuid.Parse(pair.In.ID)
	if err != nil {
		t.Fatalf("parse transfer_in id: %v", err)
	}

	// Both legs convert before the date is removed.
	if row := findOperation(t, listJournal(t, url, c, from), pair.Out.ID); row.InBase == nil {
		t.Fatalf("fully dated transfer_out in_base = null, want a conversion before erasing anything")
	}

	// Erase the first piece's purchase date.
	ct, err := pool.Exec(t.Context(),
		`UPDATE operation_transfer_lots SET acquired_on = NULL
		 WHERE operation_id = $1 AND seq = 0`, inID)
	if err != nil {
		t.Fatalf("erase the piece's acquisition date: %v", err)
	}
	if ct.RowsAffected() != 1 {
		t.Fatalf("erasing the date affected %d rows, want 1", ct.RowsAffected())
	}

	// Still 200: an undated piece is legitimate, unlike a breakdown that no
	// longer sums (TestJournalRefusesTransferWithCorruptedBreakdown).
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

// The same rule with the mixed breakdown built only through the API:
//
//	A: buy 5 TSLA on 2026-01-01
//	B: buy 5 TSLA @ $100.00 on 2026-03-01 (dated lot, 50_000)
//	A -> B: 5 on 2026-04-01 with cost_minor 70_000 (undated lot)
//	B -> C: all 10 on 2026-07-20 -> pieces [undated 70_000, dated 50_000]
//
// One piece has no date, so both legs of B -> C go null.
func TestMixedBreakdownReachedThroughTheAPIGoesNullOnBothLegs(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	seedFxRate(t, mdStore, "2026-03-01", "60.00")
	seedFxRate(t, mdStore, "2026-07-20", "90.00")

	a := mkAccount(t, url, c, "A", "USD")
	b := mkAccount(t, url, c, "B", "USD")
	dest := mkAccount(t, url, c, "C", "USD")
	tsla := mkInstrument(t, url, c,
		`{"type":"share","name":"Tesla","ticker":"TSLA","currency":"USD"}`)

	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-01-01","quantity":"5","price":"100","amount_minor":-50000,"currency":"USD"}`, a, tsla))
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-03-01","quantity":"5","price":"100","amount_minor":-50000,"currency":"USD"}`, b, tsla))

	resp := apitest.Do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"5","occurred_on":"2026-04-01","cost_minor":70000}`,
		a, b, tsla))
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer A->B with a manual basis = %d: %s", resp.StatusCode, body)
	}

	resp2 := apitest.Do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-20"}`,
		b, dest, tsla))
	if resp2.StatusCode != 201 {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("transfer B->C of the mixed parcel = %d: %s", resp2.StatusCode, body)
	}
	var pair transferResp
	apitest.Decode(t, resp2, &pair)

	if pair.Out.AmountMinor != 120_000 {
		t.Fatalf("transfer_out amount_minor = %d, want 120000 (50000 dated + 70000 undated) — the fixture assumption the rest of this test rests on",
			pair.Out.AmountMinor)
	}

	outRow := findOperation(t, listJournal(t, url, c, b), pair.Out.ID)
	inRow := findOperation(t, listJournal(t, url, c, dest), pair.In.ID)

	for _, tc := range []struct {
		name string
		row  journalItem
	}{
		{"transfer_out", outRow},
		{"transfer_in", inRow},
	} {
		if tc.row.InBase == nil {
			continue
		}
		switch tc.row.InBase.AmountMinor {
		case 10_800_000:
			t.Errorf("%s in_base.amount_minor = 10800000 — the whole basis at the TRANSFER day's rate (120000 × 90.00), the invented figure the breakdown exists to replace", tc.name)
		case 3_000_000:
			t.Errorf("%s in_base.amount_minor = 3000000 — only the piece that has a date (50000 × 60.00); a basis missing one of its pieces reads exactly like a smaller real one", tc.name)
		default:
			t.Errorf("%s in_base = %+v, want null: this parcel mixes a dated piece with an undated one, reached through ordinary transfer calls and no database manipulation", tc.name, *tc.row.InBase)
		}
	}
}

// Terms of a multi-term amount are summed as decimals and rounded once, not
// rounded one by one. Every other fixture multiplies exactly, so this one forces
// an uneven split:
//
//	lot 1: 14 @ $92.56  on 2019-03-01 -> 129584
//	lot 2:  8 @ $119.89 on 2019-03-02 ->  95912
//	both resolve to the one seeded rate, 78.4913
//
//	129584 × 78.4913 = 10171216.6192 -> alone 10171217
//	 95912 × 78.4913 =  7528257.5656 -> alone  7528258
//	rounded then summed:       17699475
//	summed then rounded once:  17699474 (wanted)
func TestTransferInBaseRoundsOnceForTheWholeAmount(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	seedFxRate(t, mdStore, "2019-01-01", "78.4913")

	from := mkAccount(t, url, c, "Т-Банк", "USD")
	to := mkAccount(t, url, c, "Freedom KZ", "USD")
	tsla := mkInstrument(t, url, c,
		`{"type":"share","name":"Tesla","ticker":"TSLA","currency":"USD"}`)

	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2019-03-01","quantity":"14","price":"92.56","amount_minor":-129584,"currency":"USD"}`, from, tsla))
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2019-03-02","quantity":"8","price":"119.89","amount_minor":-95912,"currency":"USD"}`, from, tsla))

	resp := apitest.Do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"22","occurred_on":"2019-03-10"}`,
		from, to, tsla))
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	apitest.Decode(t, resp, &pair)

	const wantSummedThenRounded = int64(17699474)
	const roundedEachTermFirst = int64(17699475)

	row := findOperation(t, listJournal(t, url, c, to), pair.In.ID)
	if row.InBase == nil {
		t.Fatalf("transfer_in in_base = null, want a conversion from both purchase dates")
	}
	if row.InBase.AmountMinor == roundedEachTermFirst {
		t.Fatalf("transfer_in in_base.amount_minor = %d — each lot's ruble figure was rounded on its own before being summed; summed unrounded and rounded once for the whole amount it is %d",
			roundedEachTermFirst, wantSummedThenRounded)
	}
	if row.InBase.AmountMinor != wantSummedThenRounded {
		t.Errorf("transfer_in in_base.amount_minor = %d, want %d (both lots' rubles summed as decimals, rounded once for the whole figure)",
			row.InBase.AmountMinor, wantSummedThenRounded)
	}
}
