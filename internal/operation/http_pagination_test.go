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
	"babki.my/babki/internal/platform/apitest"
)

// hasMore is the server's answer to "is that all". A client comparing the
// page's length with the limit is wrong exactly at a full page, where a
// truncated journal looks whole (#86). These tests pin the flag on both sides of
// the boundary and at the ceiling.

// journalPage is the listing response, decoded locally so a renamed field
// is noticed.
type journalPage struct {
	Operations []journalItem `json:"operations"`
	HasMore    bool          `json:"has_more"`
}

// getJournalPage fetches one page and decodes the envelope.
func getJournalPage(t *testing.T, url string, c *http.Client, accountID, query string) journalPage {
	t.Helper()
	resp := apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+accountID+"/operations?"+query, "")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET operations?%s = %d: %s", query, resp.StatusCode, b)
	}
	var page journalPage
	apitest.Decode(t, resp, &page)
	return page
}

// seedDeposits writes n RUB deposits through the store, one per day.
// Service.Create would replay the journal on every insert, quadratic for a
// fixture only read back; Store.Create with nil verify is the plain insert. RUB
// in a RUB space, so nothing asks the fx layer.
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

// Three entries with a window short of them, exactly on them, and past them.
// The middle case is the one a len(page) < limit client gets wrong.
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

// A full page at the ceiling says nothing by its length: has_more answers
// about the journal (#86), true with a row behind the page and false when the
// journal ends exactly there.
func TestJournalAtTheCeilingAnswersAboutTheJournal(t *testing.T) {
	pool, mdStore := newTestPool(t)
	url, c := newAPIOn(t, pool, marketdata.NewConverter(mdStore))
	for _, tc := range []struct {
		entries int
		more    bool
	}{{201, true}, {200, false}} {
		acc := mkAccount(t, url, c, fmt.Sprintf("Брокер на %d", tc.entries), "RUB")
		seedDeposits(t, pool, mustAccountID(t, acc), tc.entries)

		page := getJournalPage(t, url, c, acc, "limit=200&offset=0")
		if len(page.Operations) != 200 {
			t.Fatalf("%d entries: page holds %d operations, want 200, the endpoint's ceiling", tc.entries, len(page.Operations))
		}
		if page.HasMore != tc.more {
			t.Errorf("%d entries: has_more = %v on a full page, want %v", tc.entries, page.HasMore, tc.more)
		}
	}
}

// A limit or offset outside the stated bounds, or unreadable, is a 400 (#118).
// Bounds are literals so the test does not move with the constants.
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
		resp := apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+acc+"/operations?"+bad, "")
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

	// Refused past the bounds, not at them; an absent parameter defaults.
	for _, good := range []string{"limit=200&offset=0", "limit=1", "offset=0", ""} {
		page := getJournalPage(t, url, c, acc, good)
		if len(page.Operations) != 0 || page.HasMore {
			t.Errorf("GET operations?%s on an empty journal = %d rows, has_more %v; want an empty page saying there is nothing more",
				good, len(page.Operations), page.HasMore)
		}
	}
}

// The page lists newest first, each row with its in_base.
func TestJournalPageListsNewestFirst(t *testing.T) {
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
		t.Errorf("the newest row has no in_base")
	}
}

// An empty journal answers an empty list, not null.
func TestEmptyJournalAnswersAnEmptyList(t *testing.T) {
	url, c := newAPI(t)
	acc := mkAccount(t, url, c, "Рублёвый брокер", "RUB")

	resp := apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+acc+"/operations", "")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var page journalPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode envelope from %s: %v", body, err)
	}
	if page.Operations == nil {
		t.Errorf("an empty journal answered with a null `operations` rather than an empty array: %s — a client mapping over it should not have to guard", body)
	}
}
