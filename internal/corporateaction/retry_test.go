package corporateaction_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/marketdata/moex"
)

const amazonSplitRequest = `{
	"kind": "split", "isin": "` + amazonISIN + `", "effective_on": "2022-06-06",
	"ratio_from": 1, "ratio_to": 20,
	"source_ref": "https://ir.aboutamazon.com/news-release/2022"
}`

// workISIN runs the retry job once, as the queue would.
func workISIN(t *testing.T, f apiFixture, args river.JobArgs) error {
	t.Helper()
	isinArgs, ok := args.(corporateaction.MaterializeISINArgs)
	if !ok {
		t.Fatalf("queued %T, want a MaterializeISINArgs", args)
	}
	worker := corporateaction.NewMaterializeISINWorker(f.materializer, nil)
	return worker.Work(f.ctx, &river.Job[corporateaction.MaterializeISINArgs]{
		JobRow: &rivertype.JobRow{ID: 1, Attempt: 1}, Args: isinArgs,
	})
}

// An event recorded while the journals could not be written is queued for
// another attempt, which also asks for the recheck.
func TestAnEventTheJournalsCouldNotTakeIsQueuedForAnotherAttempt(t *testing.T) {
	f := newAPIFixture(t)
	f.buy(t, f.accountID, "2021-05-04", "1", -320_000)

	f.journal.away = true
	resp, body := f.do(t, http.MethodPost, "/api/v1/instrument-events", amazonSplitRequest)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d, want 201 — the event is recorded whether or not a journal took it: %s", resp.StatusCode, body)
	}
	if held := f.held(t, f.accountID); held.String() != "1" {
		t.Fatalf("the account holds %s with the journal away, want the 1 it had", held)
	}
	if len(f.queue.args) != 1 {
		t.Fatalf("%d jobs queued, want 1", len(f.queue.args))
	}
	if got, ok := f.queue.args[0].(corporateaction.MaterializeISINArgs); !ok || got.ISIN != amazonISIN {
		t.Fatalf("queued %+v, want the paper the event is about", f.queue.args[0])
	}
	opts := f.queue.opts[0]
	if opts == nil || !opts.UniqueOpts.ByArgs || opts.MaxAttempts == 0 || opts.MaxAttempts > 10 {
		t.Errorf("queued with %+v, want one job per paper and a bounded number of attempts", opts)
	}

	// Still away: the attempt fails, so the queue retries.
	if err := workISIN(t, f, f.queue.args[0]); err == nil {
		t.Fatal("the job reported success with the journal still away")
	}

	f.journal.away = false
	if err := workISIN(t, f, f.queue.args[0]); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if held := f.held(t, f.accountID); held.String() != "20" {
		t.Errorf("the account holds %s after the retry, want 20", held)
	}
	if f.recheck.calls != 1 {
		t.Errorf("the rechecker was asked %d times, want 1 — the retry changed a journal", f.recheck.calls)
	}
}

// A deleted event's rows are taken out by the retry.
func TestADeletedEventTheJournalsCouldNotDropIsQueuedForAnotherAttempt(t *testing.T) {
	f := newAPIFixture(t)
	f.buy(t, f.accountID, "2021-05-04", "1", -320_000)
	resp, body := f.do(t, http.MethodPost, "/api/v1/instrument-events", amazonSplitRequest)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	events, err := f.store.List(f.ctx)
	if err != nil || len(events) != 1 {
		t.Fatalf("List: %v, %d events", err, len(events))
	}

	f.journal.away = true
	resp, body = f.do(t, http.MethodDelete, "/api/v1/instrument-events/"+events[0].ID.String(), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", resp.StatusCode, body)
	}
	if held := f.held(t, f.accountID); held.String() != "20" {
		t.Fatalf("the account holds %s with the journal away, want the 20 the deleted split left", held)
	}
	if len(f.queue.args) != 1 {
		t.Fatalf("%d jobs queued, want 1", len(f.queue.args))
	}

	f.journal.away = false
	if err := workISIN(t, f, f.queue.args[0]); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if held := f.held(t, f.accountID); held.String() != "1" {
		t.Errorf("the account holds %s after the retry, want 1 — the split no longer exists", held)
	}
}

// Nothing is queued when the journals took the event on the spot.
func TestAnEventTheJournalsTookQueuesNothing(t *testing.T) {
	f := newAPIFixture(t)
	f.buy(t, f.accountID, "2021-05-04", "1", -320_000)
	resp, body := f.do(t, http.MethodPost, "/api/v1/instrument-events", amazonSplitRequest)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if len(f.queue.args) != 0 {
		t.Errorf("%d jobs queued for an event that was applied in the request, want none", len(f.queue.args))
	}
}

// The sweep asks for a recheck of what it changed, and nothing if it
// changed nothing.
func TestTheSweepAsksForAFreshCheckOfWhatItChanged(t *testing.T) {
	f := newAPIFixture(t)
	f.buy(t, f.accountID, "2021-05-04", "1", -320_000)
	f.splitEvent(t, "2022-06-06", 1, 20)

	sweep := corporateaction.NewMaterializeAllWorker(f.materializer, nil)
	job := &river.Job[corporateaction.MaterializeAllArgs]{JobRow: &rivertype.JobRow{ID: 1}}
	if err := sweep.Work(f.ctx, job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if held := f.held(t, f.accountID); held.String() != "20" {
		t.Fatalf("the account holds %s after the sweep, want 20", held)
	}
	if f.recheck.calls != 1 || len(f.recheck.accounts) != 1 || f.recheck.accounts[0] != f.accountID {
		t.Errorf("the rechecker was asked %d times about %v, want once about the account the sweep changed",
			f.recheck.calls, f.recheck.accounts)
	}

	if err := sweep.Work(f.ctx, job); err != nil {
		t.Fatalf("second Work: %v", err)
	}
	if f.recheck.calls != 1 {
		t.Errorf("the rechecker was asked %d times after a sweep that changed nothing, want still 1", f.recheck.calls)
	}
}

// exchangeSplits is the exchange's splits table with one row in it.
type exchangeSplits struct{ rows []moex.Split }

func (e exchangeSplits) Splits(context.Context) ([]moex.Split, error) { return e.rows, nil }

func (e exchangeSplits) ISINBySecID(_ context.Context, secid string) (string, error) {
	if secid == "AMZN-RM" {
		return amazonISIN, nil
	}
	return "", nil
}

// An exchange split is recorded, materialized and followed by a recheck,
// like a hand-recorded one.
func TestASplitTheExchangePublishesReachesTheJournalAndAsksForAFreshCheck(t *testing.T) {
	f := newAPIFixture(t)
	f.buy(t, f.accountID, "2021-05-04", "1", -320_000)

	provider := exchangeSplits{rows: []moex.Split{
		{SecID: "AMZN-RM", EffectiveOn: date("2022-06-06"), From: 1, To: 20},
	}}
	worker := corporateaction.NewRefreshMoexSplitsWorker(f.store, f.materializer, provider, nil)
	job := &river.Job[corporateaction.RefreshMoexSplitsArgs]{JobRow: &rivertype.JobRow{ID: 1}}
	if err := worker.Work(f.ctx, job); err != nil {
		t.Fatalf("Work: %v", err)
	}

	events, err := f.store.List(f.ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(events) != 1 || events[0].Source != corporateaction.SourceMOEX || events[0].ISIN != amazonISIN {
		t.Fatalf("the registry holds %+v, want the one split, from the exchange, under the paper's ISIN", events)
	}
	if held := f.held(t, f.accountID); held.String() != "20" {
		t.Errorf("the account holds %s after the exchange job, want 20", held)
	}
	if f.recheck.calls != 1 {
		t.Errorf("the rechecker was asked %d times, want 1", f.recheck.calls)
	}

	// The same table again changes nothing and asks for nothing.
	if err := worker.Work(f.ctx, job); err != nil {
		t.Fatalf("second Work: %v", err)
	}
	if f.recheck.calls != 1 {
		t.Errorf("the rechecker was asked %d times after a run that learned nothing, want still 1", f.recheck.calls)
	}
}
