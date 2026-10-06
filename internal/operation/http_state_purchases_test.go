package operation_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/portfolio"
)

// arrivalFixture is the full stack with an account holding an imported
// transfer_in with no sibling and no basis, the only shape purchases can be
// stated for; only an importer writes it.
type arrivalFixture struct {
	url       string
	c         *http.Client
	pool      *pgxpool.Pool
	spaceID   uuid.UUID
	accountID string
	sberID    string
	arrival   string
}

func newArrivalFixture(t *testing.T) arrivalFixture {
	t.Helper()
	pool, mdStore := newTestPool(t)
	url, c := newAPIOn(t, pool, marketdata.NewConverter(mdStore))
	f := arrivalFixture{url: url, c: c, pool: pool}

	f.accountID = createID(t, c, url+"/api/v1/accounts", `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	f.sberID = createID(t, c, url+"/api/v1/instruments", `{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	if err := pool.QueryRow(context.Background(), `SELECT id FROM spaces LIMIT 1`).Scan(&f.spaceID); err != nil {
		t.Fatalf("read the space: %v", err)
	}

	accountID, instrumentID := uuid.MustParse(f.accountID), uuid.MustParse(f.sberID)
	qty := decimal.RequireFromString("10")
	externalID := "row-1:0"
	applied, _, err := operation.NewService(operation.NewStore(pool)).ApplyImportDelta(context.Background(), f.spaceID,
		operation.ImportDelta{Add: []operation.Operation{{
			AccountID: accountID, InstrumentID: &instrumentID, Type: operation.TypeTransferIn,
			OccurredOn: date("2026-06-15"), Quantity: &qty, Currency: "RUB",
			Source: "tinvest", ExternalID: &externalID,
		}}})
	if err != nil || len(applied) != 1 {
		t.Fatalf("write the arrival: %v (%d rows)", err, len(applied))
	}
	f.arrival = applied[0].ID.String()
	return f
}

func createID(t *testing.T, c *http.Client, url, body string) string {
	t.Helper()
	resp := apitest.Do(t, c, "POST", url, body)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s = %d: %s", url, resp.StatusCode, b)
	}
	var out idResp
	apitest.Decode(t, resp, &out)
	return out.ID
}

func (f arrivalFixture) state(t *testing.T, body string) *http.Response {
	t.Helper()
	return apitest.Do(t, f.c, "PUT", f.url+"/api/v1/operations/"+f.arrival+"/purchases", body)
}

// held folds the account's journal as every later read does.
func (f arrivalFixture) held(t *testing.T) *portfolio.Position {
	t.Helper()
	journal, err := operation.NewStore(f.pool).ListForEngine(context.Background(), f.spaceID, uuid.MustParse(f.accountID))
	if err != nil {
		t.Fatalf("ListForEngine: %v", err)
	}
	positions, err := portfolio.Compute(journal)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	return positions[uuid.MustParse(f.sberID)]
}

// statedRows is what the importer will find on its next rebuild.
func (f arrivalFixture) statedRows(t *testing.T) []string {
	t.Helper()
	got, err := operation.NewStore(f.pool).StatedPurchases(context.Background(), f.spaceID,
		[]uuid.UUID{uuid.MustParse(f.accountID)}, "tinvest")
	if err != nil {
		t.Fatalf("StatedPurchases: %v", err)
	}
	var out []string
	for _, pieces := range got {
		for _, pc := range pieces {
			day := "-"
			if pc.AcquiredOn != nil {
				day = pc.AcquiredOn.Format("2006-01-02")
			}
			out = append(out, fmt.Sprintf("%s×%d@%s", pc.Quantity, pc.CostMinor, day))
		}
	}
	return out
}

// Two purchases behind ten shares that arrived with no price: one known by its
// price and day, one by its total and commission alone. The arrival's basis
// becomes their sum, the account holds them as two lots, and the statement is
// kept where the importer will look for it.
func TestStatingThePurchasesBehindAnArrivalGivesItsSharesTheirCost(t *testing.T) {
	f := newArrivalFixture(t)

	resp := f.state(t, `{"purchases":[
		{"quantity":"4","price":"250.50","acquired_on":"2021-03-02"},
		{"quantity":"6","cost_minor":180000,"fee_minor":150}
	]}`)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT purchases = %d: %s", resp.StatusCode, b)
	}
	var op opResp
	apitest.Decode(t, resp, &op)
	// 4 × 250,50 = 1 002,00 ₽ struck by the server; 1 800,00 + 1,50 fee.
	if op.AmountMinor != 100_200+180_150 {
		t.Errorf("amount_minor = %d, want 280350", op.AmountMinor)
	}
	if !op.HasUndatedLots {
		t.Error("has_undated_lots = false, but one purchase was stated without its day")
	}

	p := f.held(t)
	if p == nil || len(p.Lots) != 2 || p.CostMinor != 280_350 {
		t.Fatalf("position = %+v, want two lots costing 280350", p)
	}
	// In the order the queue releases them, which is the engine's to decide:
	// what is known of the two is the cost and the day of each.
	byCost := map[int64]*portfolio.Lot{}
	for i := range p.Lots {
		byCost[p.Lots[i].CostMinor] = &p.Lots[i]
	}
	if dated := byCost[100_200]; dated == nil || dated.AcquiredOn == nil || dated.AcquiredOn.Format("2006-01-02") != "2021-03-02" {
		t.Errorf("lots = %+v, want 4 shares costing 100200 bought on 2021-03-02", p.Lots)
	}
	if undated := byCost[180_150]; undated == nil || undated.AcquiredOn != nil {
		t.Errorf("lots = %+v, want 6 shares costing 180150 with no day", p.Lots)
	}
	if got := f.statedRows(t); len(got) != 2 {
		t.Errorf("stated for the importer: %v, want both purchases", got)
	}

	// Stated again: replaced, not added to.
	if resp := f.state(t, `{"purchases":[{"quantity":"10","price":"300","acquired_on":"2022-01-10"}]}`); resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("second PUT = %d: %s", resp.StatusCode, b)
	}
	if p := f.held(t); len(p.Lots) != 1 || p.CostMinor != 300_000 {
		t.Errorf("after restating: %+v, want one lot of 300000", p)
	}
	if got := f.statedRows(t); len(got) != 1 || got[0] != "10×300000@2022-01-10" {
		t.Errorf("stated for the importer after restating: %v", got)
	}
}

// What cannot be a statement about these shares is refused before anything is
// written, and the arrival keeps the basis it had.
func TestStatedPurchasesThatCannotBeTheseSharesAreRefused(t *testing.T) {
	f := newArrivalFixture(t)
	for name, body := range map[string]string{
		"too few shares":           `{"purchases":[{"quantity":"9","price":"100"}]}`,
		"too many shares":          `{"purchases":[{"quantity":"6","price":"100"},{"quantity":"5","price":"100"}]}`,
		"bought after they came":   `{"purchases":[{"quantity":"10","price":"100","acquired_on":"2026-06-16"}]}`,
		"a price and a total":      `{"purchases":[{"quantity":"10","price":"100","cost_minor":100000}]}`,
		"neither price nor total":  `{"purchases":[{"quantity":"10"}]}`,
		"no purchases at all":      `{"purchases":[]}`,
		"a negative price":         `{"purchases":[{"quantity":"10","price":"-1"}]}`,
		"finer than the journal":   `{"purchases":[{"quantity":"9.99999999999","price":"1"},{"quantity":"0.00000000001","price":"1"}]}`,
		"a quantity that is words": `{"purchases":[{"quantity":"ten","price":"1"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if resp := f.state(t, body); resp.StatusCode != http.StatusBadRequest {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want 400: %s", resp.StatusCode, b)
			}
		})
	}
	if p := f.held(t); p.CostMinor != 0 || len(p.Lots) != 1 {
		t.Errorf("position = %+v, want the arrival untouched: one lot bought for nothing", p)
	}
}

// Shares moved between two of the owner's accounts carry the purchases of the
// account they left; the place to state them is wherever they first arrived.
func TestPurchasesCannotBeStatedForATransferBetweenOwnAccounts(t *testing.T) {
	f := newArrivalFixture(t)
	other := createID(t, f.c, f.url+"/api/v1/accounts", `{"name":"Другой","type":"brokerage","currency":"RUB"}`)
	resp := apitest.Do(t, f.c, "POST", f.url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-01"}`,
		f.accountID, other, f.sberID))
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer = %d: %s", resp.StatusCode, b)
	}
	var tr transferResp
	apitest.Decode(t, resp, &tr)

	resp = apitest.Do(t, f.c, "PUT", f.url+"/api/v1/operations/"+tr.In.ID+"/purchases",
		`{"purchases":[{"quantity":"10","price":"100"}]}`)
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400: %s", resp.StatusCode, b)
	}
}

// Shares moved on after arriving carried a breakdown bought for nothing;
// stating the purchases releases the move again (#227), so the moved shares
// carry the stated price and day.
func TestStatingPurchasesCarriesThemToSharesMovedOnSince(t *testing.T) {
	f := newArrivalFixture(t)
	other := createID(t, f.c, f.url+"/api/v1/accounts", `{"name":"Другой","type":"brokerage","currency":"RUB"}`)
	resp := apitest.Do(t, f.c, "POST", f.url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"4","occurred_on":"2026-07-01"}`,
		f.accountID, other, f.sberID))
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer = %d: %s", resp.StatusCode, b)
	}

	resp = f.state(t, `{"purchases":[{"quantity":"10","price":"100","acquired_on":"2021-03-02"}]}`)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, b)
	}
	if p := f.held(t); p.CostMinor != 60_000 {
		t.Errorf("cost of the six that stayed = %d, want 60000", p.CostMinor)
	}
	moved := arrivalFixture{pool: f.pool, spaceID: f.spaceID, accountID: other, sberID: f.sberID}.held(t)
	if moved == nil || moved.CostMinor != 40_000 {
		t.Fatalf("the four that moved on: %+v, want a cost of 40000", moved)
	}
	for _, lot := range moved.Lots {
		if lot.AcquiredOn == nil || lot.AcquiredOn.Format("2006-01-02") != "2021-03-02" {
			t.Errorf("a lot that moved on is dated %v, want the stated 2021-03-02", lot.AcquiredOn)
		}
	}
}
