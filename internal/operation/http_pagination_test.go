package operation_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
)

// Issue #86. The handler clamps `limit` to its ceiling without saying so and
// used to answer with a bare array, so the only thing a client could do was
// compare the page's length against what it asked for. That comparison is
// wrong in exactly one place and it is the place that matters: at the ceiling,
// a client asking for more than the ceiling gets the ceiling back, concludes
// there is nothing further, and hides the only control that could have reached
// the older entries. A truncated journal presented itself as the whole journal.
//
// These tests pin the flag at the boundary in BOTH directions — the page that
// is exactly the whole journal must say so, and the page one row short of it
// must say the opposite — and then at the ceiling itself, which is where the
// defect was actually reachable.

// journalPage is the listing response: the page and the answer to "is that
// all". Decoded here rather than reusing the generated type so a test would
// notice a field renamed in the contract.
type journalPage struct {
	Operations []journalItem `json:"operations"`
	HasMore    bool          `json:"has_more"`
}

// getJournalPage fetches one page with an explicit query string and decodes the
// envelope, failing the test on a non-200.
func getJournalPage(t *testing.T, url string, c *http.Client, accountID, query string) journalPage {
	t.Helper()
	resp := do(t, c, "GET", url+"/api/v1/accounts/"+accountID+"/operations?"+query, "")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET operations?%s = %d: %s", query, resp.StatusCode, b)
	}
	var page journalPage
	decodeJSON(t, resp, &page)
	return page
}

// seedDeposits writes n RUB deposits straight through the store, each on its
// own day, and returns nothing but the count it wrote.
//
// It bypasses the service deliberately. Service.Create replays the whole
// account journal through the engine on every insert to prove the result still
// holds together, which is right for a write a user makes and quadratic for a
// fixture of two hundred rows that only ever gets read back. Store.Create with
// a nil verify is the plain insert underneath it (see its doc), so the rows are
// built from the same column list the real path uses, and the endpoint under
// test only reads them.
//
// RUB in an RUB-based space, so no row asks the fx layer for anything: this
// fixture is about counting a page, not about converting one.
func seedDeposits(t *testing.T, pool *pgxpool.Pool, accountID uuid.UUID, n int) {
	t.Helper()
	ctx := context.Background()
	var spaceID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT space_id FROM accounts WHERE id = $1`, accountID).
		Scan(&spaceID); err != nil {
		t.Fatalf("read the account's space: %v", err)
	}
	store := operation.NewStore(pool)
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range n {
		_, err := store.Create(ctx, spaceID, operation.Operation{
			AccountID:   accountID,
			Type:        operation.TypeDeposit,
			OccurredOn:  day.AddDate(0, 0, i),
			AmountMinor: 100_00,
			Currency:    "RUB",
		}, nil)
		if err != nil {
			t.Fatalf("seed deposit %d of %d: %v", i+1, n, err)
		}
	}
}

// mustAccountID parses the id mkAccount handed back.
func mustAccountID(t *testing.T, id string) uuid.UUID {
	t.Helper()
	parsed, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("account id %q: %v", id, err)
	}
	return parsed
}

// TestJournalSaysWhetherThereIsMore walks the boundary one row at a time. Three
// entries, and a window that stops one short of them, lands exactly on them,
// and steps past them with an offset.
//
// The middle row of the table is the one that fails a `len(page) < limit`
// client: a full page and the end of the journal look identical from the
// outside, which is why the answer has to come from the server.
func TestJournalSaysWhetherThereIsMore(t *testing.T) {
	pool, mdStore := newTestPool(t)
	url, c := newAPIOn(t, pool, marketdata.NewConverter(mdStore))
	acc := mkAccount(t, url, c, "Рублёвый брокер", "RUB")
	seedDeposits(t, pool, mustAccountID(t, acc), 3)

	for _, tc := range []struct {
		name      string
		query     string
		wantRows  int
		wantMore  bool
		whyItFits string
	}{
		{
			name: "one row short of the journal", query: "limit=2&offset=0",
			wantRows: 2, wantMore: true,
			whyItFits: "the third entry is still behind this page",
		},
		{
			name: "exactly the journal", query: "limit=3&offset=0",
			wantRows: 3, wantMore: false,
			whyItFits: "a full page and the end of the journal are the same shape; only the server can tell them apart",
		},
		{
			name: "a window wider than the journal", query: "limit=10&offset=0",
			wantRows: 3, wantMore: false,
			whyItFits: "nothing was withheld",
		},
		{
			name: "the last page reached by offset", query: "limit=2&offset=2",
			wantRows: 1, wantMore: false,
			whyItFits: "the flag answers for the page it travels with, not for the journal as a whole",
		},
		{
			name: "past the end", query: "limit=2&offset=3",
			wantRows: 0, wantMore: false,
			whyItFits: "an empty page is the end of the journal, never an invitation to ask again",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := getJournalPage(t, url, c, acc, tc.query)
			if len(page.Operations) != tc.wantRows {
				t.Fatalf("page holds %d operations, want %d", len(page.Operations), tc.wantRows)
			}
			if page.HasMore != tc.wantMore {
				t.Errorf("has_more = %v, want %v — %s", page.HasMore, tc.wantMore, tc.whyItFits)
			}
		})
	}
}

// TestJournalAtTheCeilingSaysThereIsMore is #86 at the boundary the clamp used
// to make unreachable.
//
// It used to ask for 250 against 201 entries and get 200 back, because the
// server clamped: the client read "fewer than I asked for" as "that is
// everything", hid its «показать ещё», and left the two-hundred-and-first entry
// unreachable through the interface. That request is a 400 now (#118), so the
// case is asked the way it survives: exactly at the ceiling, where the page is
// FULL and its length says nothing at all. That is the case the flag is really
// for, and the one no clamp can be blamed for.
func TestJournalAtTheCeilingSaysThereIsMore(t *testing.T) {
	pool, mdStore := newTestPool(t)
	url, c := newAPIOn(t, pool, marketdata.NewConverter(mdStore))
	acc := mkAccount(t, url, c, "Рублёвый брокер", "RUB")
	seedDeposits(t, pool, mustAccountID(t, acc), 201)

	page := getJournalPage(t, url, c, acc, "limit=200&offset=0")
	if len(page.Operations) != 200 {
		t.Fatalf("page holds %d operations, want 200 — the endpoint's ceiling, which this test is not raising", len(page.Operations))
	}
	if !page.HasMore {
		t.Errorf("has_more = false on a page of 200 out of 201 entries. This is the whole of #86: the page is full, its length cannot say whether the journal continues, and the answer looked exactly like the end of it")
	}
}

// TestJournalAtTheCeilingWithNothingBehindIt is the same request against a
// journal that really does end there, and it is the assertion that keeps the
// fix from becoming "always say there is more once the ceiling is hit".
//
// 200 entries, a request for 200: the page is exactly as long as what was asked
// for, indistinguishable by length from the test above, and yet there is
// genuinely nothing behind it. A «показать ещё» here would load the same two
// hundred rows again, forever.
func TestJournalAtTheCeilingWithNothingBehindIt(t *testing.T) {
	pool, mdStore := newTestPool(t)
	url, c := newAPIOn(t, pool, marketdata.NewConverter(mdStore))
	acc := mkAccount(t, url, c, "Рублёвый брокер", "RUB")
	seedDeposits(t, pool, mustAccountID(t, acc), 200)

	page := getJournalPage(t, url, c, acc, "limit=200&offset=0")
	if len(page.Operations) != 200 {
		t.Fatalf("page holds %d operations, want all 200", len(page.Operations))
	}
	if page.HasMore {
		t.Errorf("has_more = true on a page holding the entire 200-entry journal — the ceiling was reached and the journal ended at the same row, and the flag must answer about the journal, not about the ceiling")
	}
}

// TestJournalRefusesAPageItCannotHonour is #118 on the endpoint where #118 was
// found. A ceiling the contract states and the server does not apply is not a
// rule: the clamp this replaces answered a request for 250 as though it had
// asked for 200, and everything else it could not read — a zero, a negative, a
// word — fell through to the default and came back 200, a page of fifty rows
// answering a question nobody asked.
//
// The bounds are written as literals here — 200, 1, 0 — rather than read from
// the handler's constants, because a test that takes both sides of a comparison
// from the same declaration moves with it and proves nothing.
func TestJournalRefusesAPageItCannotHonour(t *testing.T) {
	url, c, _ := newAPIWithConverter(t)
	acc := mkAccount(t, url, c, "Рублёвый брокер", "RUB")

	for _, bad := range []string{
		"limit=201", // one past the ceiling the contract states
		"limit=999999",
		"limit=0",
		"limit=-1",
		"limit=fifty",
		"offset=-1",
		"offset=half",
	} {
		resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc+"/operations?"+bad, "")
		if resp.StatusCode != 400 {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("GET operations?%s = %d, want 400: %s", bad, resp.StatusCode, b)
			continue
		}
		var refusal struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&refusal); err != nil || refusal.Error == "" {
			t.Errorf("GET operations?%s answered 400 with no reason in the body (%v)", bad, err)
		}
	}

	// The bounds are refused PAST, not AT — and an absent parameter still
	// defaults rather than being refused for not being there.
	for _, good := range []string{"limit=200&offset=0", "limit=1", "offset=0", ""} {
		page := getJournalPage(t, url, c, acc, good)
		if len(page.Operations) != 0 || page.HasMore {
			t.Errorf("GET operations?%s on an empty journal = %d rows, has_more %v; want an empty page saying there is nothing more",
				good, len(page.Operations), page.HasMore)
		}
	}
}

// TestJournalEnvelopeCarriesTheSameRowsAsBefore keeps the shape change from
// quietly changing the page. The envelope is a breaking contract change made
// for one client; the rows inside it are the ones the bare array carried, in
// the same order, with every field they had.
func TestJournalEnvelopeCarriesTheSameRowsAsBefore(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	seedFxRate(t, mdStore, "2026-05-13", "60.00")

	acc := mkAccount(t, url, c, "US брокер", "USD")
	older := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"deposit",
		"occurred_on":"2026-05-13","amount_minor":10000,"currency":"USD"}`, acc))
	newer := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2026-05-14","amount_minor":-10000,"currency":"USD"}`, acc))

	page := getJournalPage(t, url, c, acc, "limit=50&offset=0")
	if len(page.Operations) != 2 {
		t.Fatalf("page holds %d operations, want 2", len(page.Operations))
	}
	if page.Operations[0].ID != newer || page.Operations[1].ID != older {
		t.Errorf("page order = %s, %s; want newest first (%s, %s)",
			page.Operations[0].ID, page.Operations[1].ID, newer, older)
	}
	if page.Operations[0].InBase == nil {
		t.Errorf("the newest row lost its in_base in the move to an envelope")
	}
}

// TestJournalResponseIsNoLongerABareArray states the breaking change as a fact
// about the wire, so that a later "simplification" back to an array has to
// delete a test that says why it must not be.
func TestJournalResponseIsNoLongerABareArray(t *testing.T) {
	url, c := newAPI(t)
	acc := mkAccount(t, url, c, "Рублёвый брокер", "RUB")

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc+"/operations", "")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var asArray []json.RawMessage
	if json.Unmarshal(body, &asArray) == nil {
		t.Fatalf("the journal still answers with a bare array: %s — an array has nowhere to say whether it is the whole journal, which is the defect (#86)", body)
	}
	var page journalPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode envelope from %s: %v", body, err)
	}
	if page.Operations == nil {
		t.Errorf("an empty journal answered with a null `operations` rather than an empty array: %s — a client mapping over it should not have to guard", body)
	}
}
