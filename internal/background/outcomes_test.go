package background_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/background"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/jobs"
	"babki.my/babki/internal/platform/testdb"
)

func sourceOf(t *testing.T, list []jobs.Source, kind string) jobs.Source {
	t.Helper()
	for _, s := range list {
		if s.Kind == kind {
			return s
		}
	}
	t.Fatalf("no source %q in %+v", kind, list)
	return jobs.Source{}
}

// The sources are the fourteen outside feeds, in the order a reader looks for
// them: the exchange's quotes first.
func TestTheSourcesAreTheOutsideFeedsInTheirOrder(t *testing.T) {
	list, err := background.Sources(context.Background(), testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 14 || list[0].Kind != (marketdata.RefreshQuotesArgs{}).Kind() {
		t.Errorf("sources = %d starting with %q, want the fourteen in their fixed order", len(list), list[0].Kind)
	}
}

// The queue the application starts records its jobs' outcomes: the
// exchange's quotes job, run when the queue starts, leaves its success behind.
func TestTheRunningQueueRecordsItsJobs(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	enqueuer := jobs.NewEnqueuer()
	caStore, caMaterializer := stubCorporateActions(pool)
	workers := background.NewWorkers(slog.Default(), pool, marketdata.NewStore(pool), instrument.NewStore(pool),
		operation.NewStore(pool), account.NewStore(pool), family.NewStore(pool),
		stubFxProvider{}, stubQuoteProvider{}, background.ReferenceSources{}, stubTinvestDeps(t, pool), caStore, caMaterializer, enqueuer)
	client, err := background.NewClient(pool, workers, enqueuer, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	}()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		list, err := background.Sources(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		if s := sourceOf(t, list, marketdata.RefreshQuotesArgs{}.Kind()); s.LastSuccessAt != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the quotes job's success was not recorded within 15s")
}

// Every member may read the sources; a visitor may not.
func TestTheSourcesAreServedToMembers(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	background.NewStatusHandler(pool, auth, sm).Mount(srv)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	anon, err := http.Get(ts.URL + "/api/v1/data-sources")
	if err != nil {
		t.Fatal(err)
	}
	if anon.StatusCode != http.StatusUnauthorized {
		t.Errorf("a visitor = %d, want 401", anon.StatusCode)
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, err := c.Post(ts.URL+"/api/v1/setup", "application/json",
		strings.NewReader(`{"space_name":"S","username":"alex","display_name":"A","password":"secret123"}`))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("setup: %v %v", err, resp)
	}
	got, err := c.Get(ts.URL + "/api/v1/data-sources")
	if err != nil {
		t.Fatal(err)
	}
	if got.StatusCode != http.StatusOK {
		t.Errorf("a member = %d, want 200", got.StatusCode)
	}
}
