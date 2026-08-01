package operation_test

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/marketdata"
)

// TestJournalRefusesTransferWithCorruptedBreakdown closes the asymmetry
// issue #56 describes: portfolio.CheckTransferLots already refuses to fold a
// transfer_in's stored breakdown into a position when its pieces no longer
// sum to the operation carrying them (see portfolio.Compute's TypeTransferIn
// branch) — but that guard only ever ran on the arriving leg's POSITION.
// Nothing checked the breakdown on the JOURNAL path (amountTerms, which
// GET .../operations uses to build the row's ruble figure), on either leg,
// before this fix.
//
// A breakdown can only drift from its sum through damage done directly in
// the database: every write path (CreateTransfer/quantizeLots, and
// Store.CreatePair's own re-check of the rows as stored) keeps the pieces
// and the operation in lockstep, so the API itself cannot produce this
// state. That is the point of corrupting a stored lot with raw SQL below
// instead of trying to reach this through the API: healthy data can never
// trip the check, so a mismatch here can only mean the rows were changed
// outside the application, and the request must fail loudly for it instead
// of quietly emitting a ruble figure assembled from broken data.
//
// Without the fix in amountTerms, this test fails: both legs' GET
// .../operations come back 200 with an in_base.amount_minor computed from
// the corrupted pieces — the source account's journal doing so was exactly
// the "silent wrong rubles" issue #56 reports, and the destination leg's own
// journal (a different question from its POSITIONS screen, which was
// already guarded) turns out to have had the identical gap.
func TestJournalRefusesTransferWithCorruptedBreakdown(t *testing.T) {
	pool, mdStore := newTestPool(t)
	url, c := newAPIOn(t, pool, marketdata.NewConverter(mdStore))
	seedFxRate(t, mdStore, "2026-05-13", "60.00")
	seedFxRate(t, mdStore, "2026-07-20", "78.50")

	from := mkAccount(t, url, c, "Т-Банк", "USD")
	to := mkAccount(t, url, c, "Freedom KZ", "USD")
	tsla := mkInstrument(t, url, c,
		`{"type":"share","name":"Tesla","ticker":"TSLA","currency":"USD"}`)

	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-05-13","quantity":"10","price":"180","amount_minor":-180000,"currency":"USD"}`, from, tsla))

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

	// Sanity check: the healthy breakdown converts fine on both legs before
	// anything is corrupted, so a later failure can only be attributed to
	// the corruption below and not to some unrelated setup mistake.
	if row := findOperation(t, listJournal(t, url, c, from), pair.Out.ID); row.InBase == nil {
		t.Fatalf("healthy transfer_out in_base = null, want a conversion before corrupting anything")
	}
	if row := findOperation(t, listJournal(t, url, c, to), pair.In.ID); row.InBase == nil {
		t.Fatalf("healthy transfer_in in_base = null, want a conversion before corrupting anything")
	}

	// Corrupt the stored breakdown directly in the database: bump one
	// piece's cost by a single minor unit, so the pieces no longer sum to
	// the operation's amount_minor. The rows live next to the transfer_in
	// operation regardless of which leg later reads them (see
	// Store.attachTransferLots), so this single row corrupts what both legs
	// will read.
	ct, err := pool.Exec(t.Context(),
		`UPDATE operation_transfer_lots SET cost_minor = cost_minor + 1
		 WHERE operation_id = $1 AND seq = 0`, inID)
	if err != nil {
		t.Fatalf("corrupt transfer lot: %v", err)
	}
	if ct.RowsAffected() != 1 {
		t.Fatalf("corrupt transfer lot affected %d rows, want 1", ct.RowsAffected())
	}

	for _, tc := range []struct {
		name      string
		accountID string
	}{
		{"transfer_out (source account journal)", from},
		{"transfer_in (destination account journal)", to},
	} {
		resp := do(t, c, "GET", url+"/api/v1/accounts/"+tc.accountID+"/operations", "")
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s: GET operations with a corrupted breakdown = 200: %s — "+
				"a breakdown that no longer sums to the operation must fail the request, "+
				"not silently print a ruble figure assembled from it", tc.name, body)
		}
	}
}
