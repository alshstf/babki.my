package background_test

import (
	"log/slog"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/background"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

// The screen's list of running jobs: the family's own import with its accounts,
// and the instance's rates, but not another family's import.
func TestTheBackgroundTasksAreTheSpacesAndTheInstances(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	background.NewTasksHandler(pool, auth, sm).Mount(srv)
	base, client := apitest.Serve(t, srv.Handler())

	var mine uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT id FROM spaces`).Scan(&mine); err != nil {
		t.Fatal(err)
	}
	account := uuid.New()
	for _, r := range []struct {
		id       int64
		kind     string
		space    *uuid.UUID
		accounts []uuid.UUID
	}{
		{1, "tinvest.sync", &mine, []uuid.UUID{account}},
		{2, "marketdata.backfill_fx", nil, []uuid.UUID{}},
		{3, "tinvest.sync", func() *uuid.UUID { u := uuid.New(); return &u }(), []uuid.UUID{}},
	} {
		if _, err := pool.Exec(t.Context(), `
			INSERT INTO job_progress (job_id, kind, space_id, account_ids, stage, done, total)
			VALUES ($1, $2, $3, $4, 'journal', 40, 100)`, r.id, r.kind, r.space, r.accounts); err != nil {
			t.Fatal(err)
		}
	}

	resp := apitest.Do(t, client, http.MethodGet, base+"/api/v1/background-tasks", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var tasks []apitypes.BackgroundTask
	apitest.Decode(t, resp, &tasks)
	if len(tasks) != 2 {
		t.Fatalf("tasks = %+v, want the family's import and the instance's rates", tasks)
	}
	sync, rates := tasks[0], tasks[1]
	if sync.Kind != "tinvest.sync" || sync.WholeInstance || len(sync.AccountIds) != 1 || sync.AccountIds[0] != account ||
		sync.Stage != "journal" || sync.Done != 40 || sync.Total != 100 {
		t.Errorf("the import = %+v, want journal 40/100 on the family's account", sync)
	}
	if rates.Kind != "marketdata.backfill_fx" || !rates.WholeInstance || len(rates.AccountIds) != 0 {
		t.Errorf("the rates = %+v, want a task of the whole instance with no accounts", rates)
	}
}
