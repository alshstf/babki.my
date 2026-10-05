package operation_test

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/marketdata"
)

// A stored breakdown that no longer sums to its operation fails the journal
// request on both legs instead of producing a rouble figure from broken data
// (#56). No write path can produce this, so the test corrupts a row with SQL.
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

	// Both legs convert before the corruption.
	if row := findOperation(t, listJournal(t, url, c, from), pair.Out.ID); row.InBase == nil {
		t.Fatalf("healthy transfer_out in_base = null, want a conversion before corrupting anything")
	}
	if row := findOperation(t, listJournal(t, url, c, to), pair.In.ID); row.InBase == nil {
		t.Fatalf("healthy transfer_in in_base = null, want a conversion before corrupting anything")
	}

	// Bump one piece's cost by one minor unit. The rows sit next to the
	// transfer_in and both legs read them.
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
