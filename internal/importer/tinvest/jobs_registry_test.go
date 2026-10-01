package tinvest

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/operation"
	"github.com/google/uuid"
)

// sberSplit records a 1:10 split of the paper buy.json buys, effective after
// that purchase.
func sberSplit(t *testing.T, f *workerFixture) {
	t.Helper()
	if _, err := corporateaction.NewStore(f.pool).Create(f.ctx, corporateaction.Event{
		Kind:        corporateaction.KindSplit,
		ISIN:        "RU0009029540",
		EffectiveOn: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		RatioFrom:   1,
		RatioTo:     10,
		Source:      corporateaction.SourceManual,
		SourceRef:   "test",
	}); err != nil {
		t.Fatalf("record the split: %v", err)
	}
}

// brokerHoldsSberAfterTheSplit answers the run's three questions the way a
// broker does after the split: the old purchase in its original 100, and the
// position in today's 1 000.
func brokerHoldsSberAfterTheSplit(t *testing.T, f *workerFixture) {
	t.Helper()
	f.broker.answer(rpcOperations, http.StatusOK, operationsPage(opFixture(t, "buy.json")))
	f.broker.answer(rpcInstrumentB, http.StatusOK, string(readFixture(t, "instrument.json")))
	f.broker.answer(rpcPortfolio, http.StatusOK,
		`{"positions":[{"instrumentUid":"e6123145-9665-43e0-8413-cd61b8aa9b13","instrumentType":"share",`+
			`"quantity":{"units":"1000","nano":0},"blocked":false}]}`)
}

func (f *workerFixture) registryRows(t *testing.T) []operation.Operation {
	t.Helper()
	ops, err := f.ops.ListBySource(f.ctx, f.spaceID, f.accountID, operation.SourceRegistry)
	if err != nil {
		t.Fatalf("ListBySource: %v", err)
	}
	return ops
}

// paperDifferences is what the latest run recorded about papers.
func (f *workerFixture) paperDifferences(t *testing.T) []ReconcileMismatch {
	t.Helper()
	runs := f.runs(t)
	if len(runs) == 0 {
		t.Fatal("no run recorded")
	}
	var list []ReconcileMismatch
	if len(runs[0].ReconcileMismatches) > 0 {
		if err := json.Unmarshal(runs[0].ReconcileMismatches, &list); err != nil {
			t.Fatalf("decode mismatches %s: %v", runs[0].ReconcileMismatches, err)
		}
	}
	return securitiesMismatches(ReconcileResult{Mismatches: list})
}

// A purchase the broker reports from before a split the registry already holds
// gets that split's row IN THE SAME RUN, and the run's own comparison with the
// broker is made against the journal that has it. Without the step the journal
// holds 100 against the broker's 1 000 until the daily sweep, and the verdict
// on the screen says so for as long.
func TestSyncWorkerAppliesTheRegistryBeforeItComparesWithTheBroker(t *testing.T) {
	f := newWorkerFixtureWith(t, true)
	sberSplit(t, f)
	brokerHoldsSberAfterTheSplit(t, f)

	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("Work: %v", err)
	}

	rows := f.registryRows(t)
	if len(rows) != 1 || rows[0].Type != operation.TypeSplit {
		t.Fatalf("the journal holds %d registry rows after one run, want the one split: %+v", len(rows), rows)
	}
	if got := f.paperDifferences(t); len(got) != 0 {
		t.Errorf("the run reports a difference about papers the registry had already closed: %+v", got)
	}
}

// The same run without a registry wired is what the step is measured against:
// the difference is there, so the test above is not passing on a broker that
// would have matched anyway.
func TestSyncWorkerWithoutARegistryReportsTheSplitAsADifference(t *testing.T) {
	f := newWorkerFixture(t)
	sberSplit(t, f)
	brokerHoldsSberAfterTheSplit(t, f)

	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("Work: %v", err)
	}

	if rows := f.registryRows(t); len(rows) != 0 {
		t.Fatalf("the journal holds %d registry rows with no registry wired, want none", len(rows))
	}
	if got := f.paperDifferences(t); len(got) != 1 {
		t.Errorf("got %d differences about papers, want the one the missing split leaves: %+v", len(got), got)
	}
}

// failingRegistry refuses every account.
type failingRegistry struct{ asked int }

func (r *failingRegistry) ForAccount(context.Context, uuid.UUID, uuid.UUID) (corporateaction.Stats, error) {
	r.asked++
	return corporateaction.Stats{}, errors.New("the registry is away")
}

// A registry that cannot be applied does not fail the run: the broker's rows
// are in the journal, the comparison is made against it as it stands, and the
// failure is in the log.
func TestSyncWorkerFinishesTheRunWhenTheRegistryCannotBeApplied(t *testing.T) {
	f := newWorkerFixture(t)
	registry := &failingRegistry{}
	f.worker.(*syncWorker).registry = registry
	f.broker.answer(rpcOperations, http.StatusOK,
		operationsPage(depositJSON("op-1", "2026-03-14T07:30:15Z", 1000)))
	f.broker.answer(rpcPositions, http.StatusOK, moneyPositions("rub", 1000))

	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("Work: %v, want the run to finish", err)
	}
	if registry.asked != 1 {
		t.Errorf("the registry was asked %d times, want once for the one linked account", registry.asked)
	}
	runs := f.runs(t)
	if len(runs) != 1 || runs[0].Status != RunOK || runs[0].ReconcileStatus != ReconcileMatched {
		t.Fatalf("runs = %+v, want one finished and matched", runs)
	}
	assertOneRecordAt(t, f.logs,
		"tinvest: the import was written but the registry's rows were not brought into line",
		slog.LevelError, "the registry is away")
}
