package jobs_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river/rivertype"

	"babki.my/babki/internal/account"
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

// Every attempt is recorded under its kind: a success as its time, a failure
// as its time and its text, cut short; the heartbeat, which says nothing about
// outside data, is not recorded; and the job's own error passes through
// unchanged whatever happens to the record.
func TestEachAttemptOfAJobIsRecorded(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	hook := jobs.NewOutcomeHook(pool, slog.Default())
	quotes := &rivertype.JobRow{Kind: marketdata.RefreshQuotesArgs{}.Kind()}

	if err := hook.WorkEnd(ctx, quotes, nil); err != nil {
		t.Fatalf("success: %v", err)
	}
	failure := errors.New("moex: unexpected status 503 " + strings.Repeat("x", 400))
	if err := hook.WorkEnd(ctx, quotes, failure); !errors.Is(err, failure) {
		t.Fatalf("failure passed on as %v, want the job's own error", err)
	}
	if err := hook.WorkEnd(ctx, &rivertype.JobRow{Kind: jobs.HeartbeatArgs{}.Kind()}, nil); err != nil {
		t.Fatal(err)
	}

	list, err := jobs.Sources(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	s := sourceOf(t, list, quotes.Kind)
	if s.LastSuccessAt == nil || s.LastFailureAt == nil {
		t.Fatalf("source = %+v, want both a success and a failure", s)
	}
	if !strings.HasPrefix(s.LastError, "moex: unexpected status 503") || len([]rune(s.LastError)) != 301 {
		t.Errorf("last error = %q (%d runes), want the text cut to 300 and an ellipsis", s.LastError, len([]rune(s.LastError)))
	}
	var heartbeats int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_outcomes WHERE kind = 'heartbeat'`).Scan(&heartbeats); err != nil {
		t.Fatal(err)
	}
	if heartbeats != 0 {
		t.Error("the heartbeat was recorded as a source")
	}
	if len(list) != 9 || list[0].Kind != (marketdata.RefreshQuotesArgs{}).Kind() {
		t.Errorf("sources = %d starting with %q, want the nine in their fixed order", len(list), list[0].Kind)
	}
}

// A source is stale once it has not succeeded for three of its intervals, or
// when it has only ever failed; one that has not run yet is not.
func TestASourceIsStaleAfterThreeMissedIntervals(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	for name, c := range map[string]struct {
		s    jobs.Source
		want bool
	}{
		"fresh":         {jobs.Source{Every: time.Hour, LastSuccessAt: at(3*time.Hour - time.Minute)}, false},
		"three missed":  {jobs.Source{Every: time.Hour, LastSuccessAt: at(3*time.Hour + time.Minute)}, true},
		"only failed":   {jobs.Source{Every: time.Hour, LastFailureAt: at(time.Minute)}, true},
		"not run yet":   {jobs.Source{Every: time.Hour}, false},
		"failed lately": {jobs.Source{Every: time.Hour, LastSuccessAt: at(time.Hour), LastFailureAt: at(time.Minute)}, false},
	} {
		if got := c.s.Stale(now); got != c.want {
			t.Errorf("%s: stale = %v, want %v", name, got, c.want)
		}
	}
}

// The queue the application starts records its jobs' outcomes: the
// exchange's quotes job, run when the queue starts, leaves its success behind.
func TestTheRunningQueueRecordsItsJobs(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	enqueuer := jobs.NewEnqueuer()
	caStore, caMaterializer := stubCorporateActions(pool)
	workers := jobs.NewWorkers(slog.Default(), pool, marketdata.NewStore(pool), instrument.NewStore(pool),
		operation.NewStore(pool), account.NewStore(pool), family.NewStore(pool),
		stubFxProvider{}, stubQuoteProvider{}, stubTinvestDeps(t, pool), caStore, caMaterializer, enqueuer)
	client, err := jobs.NewClient(pool, workers, enqueuer, slog.Default())
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
		list, err := jobs.Sources(ctx, pool)
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
	jobs.NewStatusHandler(pool, auth, sm).Mount(srv)
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
