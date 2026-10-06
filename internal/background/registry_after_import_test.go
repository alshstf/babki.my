package background_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/background"
	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/importer/tinvest"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/jobs"
	"babki.my/babki/internal/platform/testdb"
)

// brokerFixture is one of the importer's own recorded broker documents.
func brokerFixture(t *testing.T, name ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{"..", "importer", "tinvest", "testdata"}, name...)...)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the broker fixture %s: %v", path, err)
	}
	return string(raw)
}

// The workers as NewWorkers wires them apply the corporate-actions registry
// after an import: a purchase of 100 before a recorded 1:10 split gets the
// split's row from the first sync.
func TestAnImportInTheQueueGetsTheRegistrysRows(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	fam := family.NewStore(pool)
	user, err := fam.CreateUser(ctx, "alex", "Александр", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	space, err := fam.CreateSpaceWithOwner(ctx, "Семья", user.ID)
	if err != nil {
		t.Fatalf("CreateSpaceWithOwner: %v", err)
	}
	acc, err := account.NewStore(pool).Create(ctx, space.ID, nil,
		"Т-Инвестиции", account.TypeBrokerage, "RUB", "Т-Банк")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	buy, passport := brokerFixture(t, "ops", "buy.json"), brokerFixture(t, "instrument.json")
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/GetOperationsByCursor"):
			_, _ = w.Write([]byte(`{"hasNext":false,"nextCursor":"","items":[` + buy + `]}`))
		case strings.HasSuffix(r.URL.Path, "/GetInstrumentBy"):
			_, _ = w.Write([]byte(passport))
		case strings.HasSuffix(r.URL.Path, "/GetPortfolio"):
			_, _ = w.Write([]byte(`{"positions":[]}`))
		default:
			_, _ = w.Write([]byte(`{"money":[],"blocked":[]}`))
		}
	}))
	t.Cleanup(broker.Close)

	deps := stubTinvestDeps(t, pool)
	deps.NewClient = func(token string) (*tinvest.Client, error) {
		return tinvest.NewClient(broker.Client(), broker.URL, token, slog.Default()), nil
	}
	conn, err := deps.Store.CreateConnection(ctx, space.ID,
		deps.Box.Seal([]byte("t.a-read-only-token")), "oken", tinvest.StatusActive)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	if _, err := deps.Store.CreateLink(ctx, tinvest.AccountLink{
		ConnectionID: conn.ID, SpaceID: space.ID, AccountID: acc.ID,
		BrokerAccountID: "2000000001", BrokerAccountName: "Брокерский счёт",
		BrokerAccountType: "ACCOUNT_TYPE_TINKOFF",
	}); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	caStore, caMaterializer := stubCorporateActions(pool)
	if _, err := caStore.Create(ctx, corporateaction.Event{
		Kind: corporateaction.KindSplit, ISIN: "RU0009029540",
		EffectiveOn: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), RatioFrom: 1, RatioTo: 10,
		Source: corporateaction.SourceManual, SourceRef: "test",
	}); err != nil {
		t.Fatalf("record the split: %v", err)
	}

	enqueuer := jobs.NewEnqueuer()
	opStore := operation.NewStore(pool)
	workers := background.NewWorkers(slog.Default(), pool, marketdata.NewStore(pool), instrument.NewStore(pool),
		opStore, account.NewStore(pool), fam,
		stubFxProvider{}, stubQuoteProvider{}, background.ReferenceSources{}, deps, caStore, caMaterializer, enqueuer)
	client, err := background.NewClient(pool, workers, enqueuer, slog.Default())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	}()

	// Wait for the sync's run log. The sweep on start could write the same row,
	// so an unlucky schedule may pass without the step, never fail with it.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var finished int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM tinvest_sync_runs WHERE connection_id = $1 AND finished_at IS NOT NULL`,
			conn.ID).Scan(&finished); err != nil {
			t.Fatalf("read the run log: %v", err)
		}
		if finished > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no sync run finished within 30s")
		}
		time.Sleep(100 * time.Millisecond)
	}

	imported, err := opStore.ListBySource(ctx, space.ID, acc.ID, tinvest.Source)
	if err != nil {
		t.Fatalf("ListBySource: %v", err)
	}
	if len(imported) == 0 {
		t.Fatal("the sync finished and imported nothing: the test's broker is not being read")
	}
	rows, err := opStore.ListBySource(ctx, space.ID, acc.ID, operation.SourceRegistry)
	if err != nil {
		t.Fatalf("ListBySource: %v", err)
	}
	if len(rows) != 1 || rows[0].Type != operation.TypeSplit {
		t.Fatalf("the journal holds %d registry rows after the first sync, want the one split: %+v",
			len(rows), rows)
	}
}
