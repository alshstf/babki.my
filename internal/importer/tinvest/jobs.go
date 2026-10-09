package tinvest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/platform/jobs"
	"babki.my/babki/internal/platform/secretbox"
)

// SyncDispatchArgs triggers one scheduler pass: a sync is queued per active
// connection. A dispatcher rather than one job syncing everyone, so each
// connection has its own timeout and retry and a slow broker starves nobody.
type SyncDispatchArgs struct{}

func (SyncDispatchArgs) Kind() string { return "tinvest.sync_dispatch" }

// SyncArgs is one connection's sync. Trigger is a string (JSON cannot check a
// named type; syncTrigger does). Only ConnectionID is tagged unique, so the hourly
// job and the owner's "sync now" share one uniqueness class and cannot overlap.
type SyncArgs struct {
	ConnectionID uuid.UUID `json:"connection_id" river:"unique"`
	Trigger      string    `json:"trigger"`
}

func (SyncArgs) Kind() string { return "tinvest.sync" }

// syncUniqueStates are the states a sync is unique across: every unfinished
// one. River's default includes completed, which would make every hourly dispatch
// a duplicate of a long-finished job until the cleaner ran (the test "a second
// sync is queued once the first has finished" shows it). Retryable is included: a
// job waiting out its backoff is still in flight.
var syncUniqueStates = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRetryable,
	rivertype.JobStateRunning,
	rivertype.JobStateScheduled,
}

// SyncInsertOpts is how a sync is queued (via EnqueueSync). ByArgs uniqueness
// (River v0.41.0) is a database index, so it covers several worker processes.
//
// It is the first of two barriers against overlapping runs of one connection; the
// second is SyncMirror's lock on the connection, which still holds after River
// rescues a job whose worker is alive. The journal's dedup index does not replace
// it: two runs that each insert a mirror row produce differently named entries
// (see TestTwoSimultaneousRunsOfOneConnectionLeaveOneMirrorAndOneJournal).
//
// Attempts are bounded because a parked job holds the connection's slot while its
// backoff grows to hours (see SyncMaxAttempts).
func SyncInsertOpts() *river.InsertOpts {
	return &river.InsertOpts{
		MaxAttempts: SyncMaxAttempts,
		UniqueOpts:  river.UniqueOpts{ByArgs: true, ByState: syncUniqueStates},
	}
}

// SyncMaxAttempts: River waits attempt⁴ seconds, so seven attempts span about
// 38 minutes, inside the hour between dispatches. A test holds it against the
// interval.
const SyncMaxAttempts = 7

// jobInserter is the queue as this package uses it.
type jobInserter interface {
	Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// EnqueueSync queues one connection's sync; the dispatcher and the "sync now"
// handler both go through it so their options cannot diverge.
//
// The result says whether a job was queued. A duplicate means one is already
// queued, possibly a failed one parked in a backoff of hours, so callers must
// say "already queued", not "running". A non-nil result always comes with a nil
// error, enforced here so callers need no nil check.
func EnqueueSync(ctx context.Context, inserter jobInserter, connectionID uuid.UUID, trigger SyncTrigger) (
	*rivertype.JobInsertResult, error,
) {
	res, err := inserter.Insert(ctx, SyncArgs{ConnectionID: connectionID, Trigger: string(trigger)}, SyncInsertOpts())
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, fmt.Errorf("tinvest: queue a sync for connection %s: the queue answered "+
			"neither a result nor an error", connectionID)
	}
	return res, nil
}

// ErrUnknownTrigger: a job named a trigger the run log's CHECK cannot store;
// caught before a run is opened.
var ErrUnknownTrigger = errors.New("tinvest: unknown sync trigger")

// syncTrigger turns a job argument back into the run log's own vocabulary.
func syncTrigger(s string) (SyncTrigger, error) {
	switch t := SyncTrigger(s); t {
	case TriggerSchedule, TriggerManual, TriggerInitial, TriggerRegistry:
		return t, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownTrigger, s)
	}
}

// syncTimeout: a first run walks the whole history, resolves new instruments,
// rebuilds and reconciles; fifteen minutes allows a long history without letting
// a wedged run hold a worker all day.
const syncTimeout = 15 * time.Minute

// apiEpoch is where a run reads from when the link has no opening day (the
// broker's GetAccounts sent none): 2016, when the T-Invest API opened. An older
// account's earlier operations would be missed on such a link. The zero time
// (no "from" filter) was not used because its live behaviour is unconfirmed.
var apiEpoch = time.Date(2016, 1, 1, 0, 0, 0, 0, time.UTC)

// historyFrom is the earliest moment a run asks about: the account's opening
// day, or apiEpoch. Never narrowed by previous runs: the broker rewrites history,
// and SyncMirror marks everything not fetched as gone. At personal scale the
// whole history is a handful of requests against 200 a minute.
func historyFrom(link AccountLink) time.Time {
	if link.OpenedOn == nil {
		return apiEpoch
	}
	return link.OpenedOn.UTC()
}

// The dispatcher.

// nothingToSyncMessage: no broker connected, the ordinary fresh install.
const nothingToSyncMessage = "tinvest: no connection is active, nothing to sync"

type dispatchWorker struct {
	river.WorkerDefaults[SyncDispatchArgs]
	store    *Store
	inserter jobInserter
	log      *slog.Logger
}

// NewDispatchWorker builds the worker queueing one sync per active
// connection.
func NewDispatchWorker(store *Store, inserter jobInserter, log *slog.Logger) river.Worker[SyncDispatchArgs] {
	if log == nil {
		log = slog.Default()
	}
	return &dispatchWorker{store: store, inserter: inserter, log: log}
}

// Work queues a sync for every active connection; disabled or revoked ones are
// skipped so a dead token is not offered hourly. A failed insert is returned so
// River retries the dispatch.
func (w *dispatchWorker) Work(ctx context.Context, _ *river.Job[SyncDispatchArgs]) error {
	conns, err := w.store.ListActiveConnections(ctx)
	if err != nil {
		logAt(ctx, w.log, err, "tinvest: list the active connections failed", "err", err)
		return err
	}
	if len(conns) == 0 {
		w.log.Debug(nothingToSyncMessage, "connections", 0)
		return nil
	}

	queued, alreadyRunning := 0, 0
	for _, conn := range conns {
		res, err := EnqueueSync(ctx, w.inserter, conn.ID, TriggerSchedule)
		if err != nil {
			logAt(ctx, w.log, err, "tinvest: queue a sync failed", "connection", conn.ID, "err", err)
			return err
		}
		if res.UniqueSkippedAsDuplicate {
			// Last hour's sync is still running and reads the whole history anyway.
			alreadyRunning++
			continue
		}
		queued++
	}
	w.log.Info("tinvest: queued the hourly syncs",
		"connections", len(conns), "queued", queued, "already_running", alreadyRunning)
	return nil
}

// One connection's sync.

const (
	// connectionGoneMessage: the owner deleted the connection while its job
	// waited.
	connectionGoneMessage = "tinvest: the connection this sync was queued for is no longer there"
	// connectionNotActiveMessage: switched off, or waiting for a new token.
	connectionNotActiveMessage = "tinvest: the connection is not active, skipping its sync"
	// noLinksMessage: no broker accounts chosen yet.
	noLinksMessage = "tinvest: the connection has no linked accounts, nothing to sync"
)

// clientFactory builds a broker client per token; the transport and its
// certificate pool are configured once in cmd/babki.
type clientFactory func(token string) (*Client, error)

// rebuilderFactory builds a Rebuilder per run: its Resolver's passport cache
// is a plain map, unsafe across concurrent jobs and unbounded if shared.
type rebuilderFactory func() *Rebuilder

// registryAligner brings an account in line with the corporate-actions
// registry (*corporateaction.Materializer). Nil means no registry.
type registryAligner interface {
	ForAccount(ctx context.Context, spaceID, accountID uuid.UUID) (corporateaction.Stats, error)
}

type syncWorker struct {
	river.WorkerDefaults[SyncArgs]
	store        *Store
	box          *secretbox.Box
	newClient    clientFactory
	newRebuilder rebuilderFactory
	registry     registryAligner
	reconciler   *Reconciler
	log          *slog.Logger
}

// NewSyncWorker builds the worker that updates one connection's mirror,
// projection and reconciliation. registry may be nil.
func NewSyncWorker(store *Store, box *secretbox.Box, newClient clientFactory,
	newRebuilder rebuilderFactory, registry registryAligner, reconciler *Reconciler, log *slog.Logger,
) river.Worker[SyncArgs] {
	if log == nil {
		log = slog.Default()
	}
	return &syncWorker{
		store: store, box: box, newClient: newClient,
		newRebuilder: newRebuilder, registry: registry, reconciler: reconciler, log: log,
	}
}

// Timeout raises River's one-minute default; see syncTimeout.
func (w *syncWorker) Timeout(*river.Job[SyncArgs]) time.Duration { return syncTimeout }

// Work runs one connection's sync. Order matters: every link's mirror first,
// then one rebuild over the whole connection (a transfer's legs sit under two
// links), then the registry (the import door does not call the hand-entry hook),
// then reconciliation against the journal just produced. No step spans the
// others; each is atomic and derived from the one before, so a crash leaves work
// for the next run, not damage.
func (w *syncWorker) Work(ctx context.Context, job *river.Job[SyncArgs]) error {
	trigger, err := syncTrigger(job.Args.Trigger)
	if err != nil {
		// Not returned: the job's arguments never change, so every retry would
		// fail the same way. No run is recorded either, since the log's CHECK is
		// what refuses this trigger; this line is the record.
		w.log.Error("tinvest: a sync job names a trigger the run log cannot store, dropping it",
			"connection", job.Args.ConnectionID, "trigger", job.Args.Trigger, "err", err)
		return nil
	}

	conn, err := w.store.connectionForSync(ctx, job.Args.ConnectionID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Deleted while queued: nothing to do.
		w.log.Debug(connectionGoneMessage, "connection", job.Args.ConnectionID)
		return nil
	}
	if err != nil {
		logAt(ctx, w.log, err, "tinvest: read the connection to sync failed",
			"connection", job.Args.ConnectionID, "err", err)
		return err
	}
	if conn.Status != StatusActive {
		w.log.Debug(connectionNotActiveMessage, "connection", conn.ID, "status", conn.Status)
		return nil
	}

	token, err := w.box.Open(conn.TokenCiphertext)
	if err != nil {
		// The stored secret will not decrypt: a changed or corrupted key, not
		// the broker. Parked like a revoked token, since the remedy is the same
		// (UpdateConnection reseals a pasted token with the current key). nil,
		// because retrying cannot find a missing key.
		w.log.Error("tinvest: decrypt the stored broker token failed", "connection", conn.ID, "err", err)
		if err := w.store.UpdateConnectionStatus(ctx, conn.ID, StatusTokenRevoked); err != nil {
			return fmt.Errorf("tinvest: park a connection whose token will not decrypt: %w", err)
		}
		return nil
	}
	client, err := w.newClient(string(token))
	if err != nil {
		w.log.Error("tinvest: build the broker client failed", "connection", conn.ID, "err", err)
		return err
	}

	links, err := w.store.LinksByConnection(ctx, conn.ID)
	if err != nil {
		logAt(ctx, w.log, err, "tinvest: list the connection's account links failed", "connection", conn.ID, "err", err)
		return err
	}
	if len(links) == 0 {
		w.log.Debug(noLinksMessage, "connection", conn.ID)
		return nil
	}
	// The linked accounts' figures are not final until the run ends.
	jobs.ProgressFrom(ctx).Scope(ctx, &conn.SpaceID, accountsOf(links))
	return w.sync(ctx, conn, links, client, trigger)
}

// linkRun is one link's run entry and what this run has learned so far, so a
// failure halfway can close every run with what it recorded (see failed).
type linkRun struct {
	link      AccountLink
	runID     uuid.UUID
	stats     MirrorSyncStats
	unparsed  int
	reconcile ReconcileResult
}

func (w *syncWorker) sync(ctx context.Context, conn Connection, links []AccountLink,
	client *Client, trigger SyncTrigger,
) error {
	runs := make([]*linkRun, 0, len(links))
	progress := jobs.ProgressFrom(ctx)

	for i, link := range links {
		progress.Stage(ctx, "operations", i, len(links))
		run, err := w.store.StartRun(ctx, conn.ID, link.ID, trigger)
		if err != nil {
			return w.failed(ctx, conn, runs, err)
		}
		runs = append(runs, &linkRun{link: link, runID: run.ID})

		// The whole history every time (historyFrom); SyncMirror collapses
		// repeated rows itself.
		items, err := client.OperationsAll(ctx, link.BrokerAccountID, historyFrom(link))
		if err != nil {
			return w.failed(ctx, conn, runs, err)
		}
		stats, err := w.store.SyncMirror(ctx, conn.ID, link, items, time.Now().UTC())
		if err != nil {
			return w.failed(ctx, conn, runs, err)
		}
		runs[len(runs)-1].stats = stats
	}

	// Settlement days (Р-3), on the hourly run only: the report is slow and
	// rate-limited, and "sync now" is waiting for the journal. A trade without
	// a day keeps its trade day until a later hour.
	if trigger == TriggerSchedule {
		progress.Stage(ctx, "settlements", 0, 0)
		readSettlements(ctx, w.store, client, links, time.Now, settlementBudget, w.log)
	}

	// One rebuild over every link (see Work and Rebuild).
	rebuilt, err := w.newRebuilder().Rebuild(ctx, conn, links, client)
	if err != nil {
		return w.failed(ctx, conn, runs, err)
	}

	w.alignWithRegistry(ctx, links)

	for i, lr := range runs {
		progress.Stage(ctx, "reconcile", i, len(runs))
		// Kept whatever comes back: "not checked" arrives with the error.
		res, err := w.reconciler.ReconcileLink(ctx, client, conn, lr.link)
		lr.reconcile = res
		if err != nil {
			return w.failed(ctx, conn, runs, err)
		}
		// Per link, not the rebuild's connection-wide figure.
		n, err := w.store.unparsedCountByLink(ctx, lr.link.ID)
		if err != nil {
			return w.failed(ctx, conn, runs, err)
		}
		lr.unparsed = n
	}

	var read, added, gone, unparsed int
	var closeErr error
	for _, lr := range runs {
		read += lr.stats.Read
		added += lr.stats.Added
		gone += lr.stats.Disappeared
		unparsed += lr.unparsed
		if err := w.store.FinishRun(ctx, lr.runID, RunOutcome{
			Status:           RunOK,
			ReadCount:        lr.stats.Read,
			AddedCount:       lr.stats.Added,
			DisappearedCount: lr.stats.Disappeared,
			UnparsedCount:    lr.unparsed,
			Reconcile:        lr.reconcile,
		}); err != nil {
			// Carry on so the remaining runs close; this one stays "running".
			logAt(ctx, w.log, err, "tinvest: close a finished sync run failed, it will stay open in the log",
				"connection", conn.ID, "run", lr.runID, "err", err)
			if closeErr == nil {
				closeErr = err
			}
		}
	}
	// Logged before the close error is returned: the work did happen.
	w.log.Info("tinvest: a sync run finished",
		"connection", conn.ID, "trigger", trigger, "links", len(links),
		"read", read, "added", added, "disappeared", gone, "unparsed", unparsed,
		"journal_added", rebuilt.Added, "journal_removed", rebuilt.Removed, "withdrawn", rebuilt.Withdrawn)
	if closeErr != nil {
		// The work is done; the retry is for the log.
		return closeErr
	}
	return nil
}

// alignWithRegistry brings every linked account in line with the registry,
// between rebuild and reconciliation. A failure does not fail the run: the
// reconciliation shows what a missing split leaves, and the daily sweep
// retries.
func (w *syncWorker) alignWithRegistry(ctx context.Context, links []AccountLink) {
	if w.registry == nil {
		return
	}
	for i, link := range links {
		jobs.ProgressFrom(ctx).Stage(ctx, "registry", i, len(links))
		stats, err := w.registry.ForAccount(ctx, link.SpaceID, link.AccountID)
		if err != nil {
			logAt(ctx, w.log, err, "tinvest: the import was written but the registry's rows were not brought into line",
				"account", link.AccountID, "err", err)
		}
		if stats.Added+stats.Removed+stats.Refused > 0 {
			w.log.Info("tinvest: the registry's rows were brought into line after an import",
				"account", link.AccountID, "added", stats.Added, "removed", stats.Removed, "refused", stats.Refused)
		}
	}
}

// failed closes every run this attempt opened and decides what River is told.
// A refused token parks the connection and returns nil, so a dead credential is
// not retried; every other failure is returned. Runs that had succeeded are
// closed as failed too: nothing was rebuilt or reconciled for them. The mirror
// counters of an unfinished pass are truly zero (rolled back); the unparsed count
// is not, so it is taken here (unparsedNow).
func (w *syncWorker) failed(ctx context.Context, conn Connection, runs []*linkRun, cause error) error {
	revoked := errors.Is(cause, ErrTokenInvalid)

	for _, lr := range runs {
		if err := w.store.FinishRun(ctx, lr.runID, RunOutcome{
			Status:           RunFailed,
			ReadCount:        lr.stats.Read,
			AddedCount:       lr.stats.Added,
			DisappearedCount: lr.stats.Disappeared,
			UnparsedCount:    w.unparsedNow(ctx, conn, lr),
			Error:            cause.Error(),
			Reconcile:        lr.reconcile,
		}); err != nil {
			// It stays "running" for good, as an interrupted sync looks.
			logAt(ctx, w.log, err, "tinvest: close a failed sync run failed, it will stay open in the log",
				"connection", conn.ID, "run", lr.runID, "err", err)
		}
	}

	if revoked {
		if err := w.store.UpdateConnectionStatus(ctx, conn.ID, StatusTokenRevoked); err != nil {
			// Still active; returned so the retry can park it.
			logAt(ctx, w.log, err, "tinvest: park a connection whose token the broker refused failed",
				"connection", conn.ID, "err", err)
			return err
		}
		w.log.Warn("tinvest: the broker refused this connection's token, it needs a new one",
			"connection", conn.ID, "err", cause)
		return nil
	}

	logAt(ctx, w.log, cause, "tinvest: a sync run failed", "connection", conn.ID, "err", cause)
	return cause
}

// unparsedNow counts the link's unreadable rows for a run being closed as
// failed, since the reconciliation loop that normally counts them was never
// reached; a zero would read as "counted, none". A failed count is logged.
func (w *syncWorker) unparsedNow(ctx context.Context, conn Connection, lr *linkRun) int {
	n, err := w.store.unparsedCountByLink(ctx, lr.link.ID)
	if err != nil {
		logAt(ctx, w.log, err,
			"tinvest: count a failed run's unreadable operations failed, it is recorded as zero unparsed",
			"connection", conn.ID, "run", lr.runID, "link", lr.link.ID, "err", err)
		return 0
	}
	return n
}

// logAt logs at Error, or Debug when the failure is only the caller going away
// (shutdown or the job's timeout): these lines answer "did the import break?".
// Used for every failure from a call taking ctx; failures that cannot be a
// cancellation are logged directly.
func logAt(ctx context.Context, log *slog.Logger, err error, msg string, args ...any) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		log.Debug(msg, args...)
		return
	}
	log.Error(msg, args...)
}
