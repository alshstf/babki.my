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

	"babki.my/babki/internal/platform/secretbox"
)

// SyncDispatchArgs triggers one pass of the scheduler: it lists the active
// connections and queues a sync for each. Kind is namespaced with "tinvest."
// so job kinds registered by different domain modules never collide in the
// shared River queue (the same convention marketdata follows).
//
// IT IS A DISPATCHER AND NOT A LOOP ON PURPOSE. Syncing every connection
// inside one periodic job would give them a single timeout and a single retry
// between them, so one connection stuck behind a slow broker would starve the
// rest and one connection's failure would re-run everybody's work.
type SyncDispatchArgs struct{}

func (SyncDispatchArgs) Kind() string { return "tinvest.sync_dispatch" }

// SyncArgs is one connection's sync. Trigger is what caused it — the values of
// SyncTrigger, as a string, because job arguments are JSON on the wire and a
// named type would only give a false sense of a check that JSON cannot make
// (the check is syncTrigger, below).
//
// ConnectionID CARRIES THE `river:"unique"` TAG AND TRIGGER DELIBERATELY DOES
// NOT. River builds the uniqueness hash from the whole encoded args unless
// fields say otherwise, so without the tag the hourly schedule's job and the
// owner's "sync now" button would fall into two different unique classes and
// could run over one connection at the same time — the exact overlap the
// uniqueness exists to prevent. With the tag, both are one job: whichever
// arrives second is skipped as a duplicate while the first is still in flight.
type SyncArgs struct {
	ConnectionID uuid.UUID `json:"connection_id" river:"unique"`
	Trigger      string    `json:"trigger"`
}

func (SyncArgs) Kind() string { return "tinvest.sync" }

// syncUniqueStates is which job states a sync job is unique ACROSS: the states
// a job that has not finished can be in.
//
// IT IS STATED AND NOT LEFT TO RIVER'S DEFAULT, and the difference is the whole
// schedule. rivertype.UniqueOptsByStateDefault() includes `completed`, so a
// finished sync would go on occupying its connection's unique key until the job
// cleaner eventually removed the row — and every hourly dispatch in between
// would be skipped as a duplicate of a job that finished long ago. Measured, not
// reasoned about: with the default in place, the test named
// "a second sync is queued once the first has finished" fails and the connection
// is never synced a second time.
//
// Available, Pending, Running and Scheduled are the four River requires any
// ByState set to contain. Retryable is added deliberately: a job waiting out its
// backoff is work still in flight, and a second one queued beside it would be
// the overlap this list exists to prevent.
var syncUniqueStates = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRetryable,
	rivertype.JobStateRunning,
	rivertype.JobStateScheduled,
}

// SyncInsertOpts is how a sync job is queued, by the schedule and by the
// owner's button alike (see EnqueueSync, which is what both actually call).
//
// UniqueOpts{ByArgs: true} IS AVAILABLE ON THE RIVER THIS MODULE PINS
// (v0.41.0, checked with `go doc github.com/riverqueue/river.UniqueOpts`), and
// it is enforced by a unique index in the database rather than in this
// process — so two `babki worker` processes are covered by it as much as two
// goroutines of one.
//
// This is the FIRST of the two barriers that keep two runs of one connection
// from overlapping. The second is the lock SyncMirror takes on the connection
// row before it reads the mirror, which stands even where this one cannot —
// the moment after River has rescued a job whose worker is still running, say.
//
// THE SECOND BARRIER IS NOT REDUNDANT AND THE JOURNAL DOES NOT REPLACE IT. The
// journal's dedup index (operations_dedup_idx, migration 0005) refuses a second
// copy of a mirror row's own entry, which stops two runs that AGREE on the
// mirror from both writing it. It has nothing to say about two runs that each
// inserted the operation into the mirror: those are two mirror rows with two
// different ids, so the journal entries they produce have different names and
// the index sees nothing in common. Measured: with SyncMirror's lock removed,
// two simultaneous runs leave two mirror rows AND two journal operations (see
// TestTwoSimultaneousRunsOfOneConnectionLeaveOneMirrorAndOneJournal).
func SyncInsertOpts() *river.InsertOpts {
	return &river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: syncUniqueStates}}
}

// jobInserter is the queue as this package uses it — a narrow local interface,
// for the reason rebuild.go declares journalDelta. *river.Client[pgx.Tx]
// satisfies it structurally.
type jobInserter interface {
	Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// EnqueueSync queues one connection's sync. EVERY CALLER GOES THROUGH HERE —
// the hourly dispatcher below and the owner's "sync now" handler alike — so
// that the arguments and the insert options are stated once. Two places
// building them separately is how the button and the schedule would end up in
// different unique classes, which is exactly what SyncInsertOpts exists to
// prevent.
//
// The result says whether the job was actually queued: a
// UniqueSkippedAsDuplicate result means a sync for this connection is already
// on its way, which is a perfectly good answer to give the owner rather than
// an error.
func EnqueueSync(ctx context.Context, inserter jobInserter, connectionID uuid.UUID, trigger SyncTrigger) (
	*rivertype.JobInsertResult, error,
) {
	return inserter.Insert(ctx, SyncArgs{ConnectionID: connectionID, Trigger: string(trigger)}, SyncInsertOpts())
}

// ErrUnknownTrigger means a sync job named a trigger the run log cannot store.
// The column carries a CHECK constraint over exactly three words, so this is
// caught here, before a run is opened, rather than surfacing as a constraint
// violation from inside a worker on the one job that used it.
var ErrUnknownTrigger = errors.New("tinvest: unknown sync trigger")

// syncTrigger turns a job argument back into the run log's own vocabulary.
func syncTrigger(s string) (SyncTrigger, error) {
	switch t := SyncTrigger(s); t {
	case TriggerSchedule, TriggerManual, TriggerInitial:
		return t, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownTrigger, s)
	}
}

// syncTimeout overrides River's one-minute default. A first run walks an
// account's WHOLE history, a thousand operations to the page, resolves every
// instrument it has never seen (one broker call each), rebuilds the projection
// over the result and then reconciles every linked account against the broker.
// A minute is nowhere near that on a first import; fifteen leaves room for a
// long history without letting a wedged run hold a worker slot all day.
const syncTimeout = 15 * time.Minute

// apiEpoch is the day a run starts reading from when the link does not say when
// the broker account was opened. It is the year the T-Invest API opened (v1 was
// published as Tinkoff Invest OpenAPI in 2016).
//
// IT IS A CHOSEN FLOOR AND NOT A PROOF. An account opened earlier than 2016 has
// operations earlier than 2016, and this API will report them if asked — so on a
// link with no opening day, and only there, a run reading from here would not
// see them. What makes that acceptable is how a link gets its opening day:
// it comes from the broker's own GetAccounts (openedDate), so a link without one
// means the gateway sent none.
//
// The alternative is available and was not taken: passing the zero time makes
// OperationsAll omit the "from" filter entirely and ask for everything (see its
// doc). It is left alone because nothing here has confirmed against a live
// account what the gateway does with an omitted filter, and a floor whose cost
// is understood beats an unfiltered request whose behaviour is not.
var apiEpoch = time.Date(2016, 1, 1, 0, 0, 0, 0, time.UTC)

// historyFrom is the earliest moment a run asks the broker about: the day the
// account was opened, or apiEpoch when the link does not know it.
//
// IT IS NEVER NARROWED BY WHAT A PREVIOUS RUN ALREADY READ, and that is the
// design and not an oversight. The broker rewrites history after the fact —
// tax corrections arrive as separate operations, the identifiers of old
// operations drift, and a corporate action can leave the history incomplete —
// so a run bounded by the last successful sync would never see any of it. It
// would also be actively destructive: SyncMirror compares against the FULL
// history of the link and marks everything it does not find as gone, so the
// first bounded run would bury every operation before its own window. See
// Store.LastSuccessfulSyncAt, which exists to be shown to the owner and for
// nothing else.
//
// At personal scale the whole history costs single-digit requests against a
// limit of 200 a minute, which is why paying it every hour is affordable.
func historyFrom(link AccountLink) time.Time {
	if link.OpenedOn == nil {
		return apiEpoch
	}
	return link.OpenedOn.UTC()
}

// -------------------------------------------------------------------------
// the dispatcher
// -------------------------------------------------------------------------

// nothingToSyncMessage is the line for an instance where nobody has connected
// a broker, which is the ordinary state of a fresh install.
const nothingToSyncMessage = "tinvest: no connection is active, nothing to sync"

type dispatchWorker struct {
	river.WorkerDefaults[SyncDispatchArgs]
	store    *Store
	inserter jobInserter
	log      *slog.Logger
}

// NewDispatchWorker builds the River worker that queues one sync per active
// connection. Register it with river.AddWorker.
func NewDispatchWorker(store *Store, inserter jobInserter, log *slog.Logger) river.Worker[SyncDispatchArgs] {
	if log == nil {
		log = slog.Default()
	}
	return &dispatchWorker{store: store, inserter: inserter, log: log}
}

// Work queues a sync for every active connection of the instance. A connection
// that is disabled or whose token the broker refused is not active and is
// passed over — otherwise a dead token would be offered to the broker every
// hour for as long as the connection existed.
//
// A queue that will not take a job is this worker's own failure: it is logged
// and returned, so River retries the dispatch rather than losing the hour.
func (w *dispatchWorker) Work(ctx context.Context, _ *river.Job[SyncDispatchArgs]) error {
	conns, err := w.store.ListActiveConnections(ctx)
	if err != nil {
		w.log.Error("tinvest: list the active connections failed", "err", err)
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
			w.log.Error("tinvest: queue a sync failed", "connection", conn.ID, "err", err)
			return err
		}
		if res != nil && res.UniqueSkippedAsDuplicate {
			// The previous hour's sync is still going. Nothing is lost: that
			// run reads the whole history anyway, so it will carry whatever
			// this one would have.
			alreadyRunning++
			continue
		}
		queued++
	}
	w.log.Info("tinvest: queued the hourly syncs",
		"connections", len(conns), "queued", queued, "already_running", alreadyRunning)
	return nil
}

// -------------------------------------------------------------------------
// one connection's sync
// -------------------------------------------------------------------------

const (
	// connectionGoneMessage is for a job whose connection the owner deleted
	// while it sat in the queue.
	connectionGoneMessage = "tinvest: the connection this sync was queued for is no longer there"
	// connectionNotActiveMessage is for a connection switched off, or parked
	// waiting for a new token.
	connectionNotActiveMessage = "tinvest: the connection is not active, skipping its sync"
	// noLinksMessage is for a connection whose owner has not chosen which
	// broker accounts to import yet.
	noLinksMessage = "tinvest: the connection has no linked accounts, nothing to sync"
)

// clientFactory builds a broker client for one token. A factory rather than a
// client because the token is per connection and is only known once the job
// has read one, while the transport (and the certificate pool behind it) is
// configured once in cmd/babki.
type clientFactory func(token string) (*Client, error)

// rebuilderFactory builds the projection rebuilder for ONE run.
//
// A FACTORY AND NOT AN INSTANCE, for a reason Rebuilder states about itself:
// the Resolver it carries caches the broker's instrument passports in a plain
// map for the life of the run, and it is not safe for concurrent use. One
// Rebuilder shared by this worker would be shared by every connection the
// queue runs at once — a concurrent map write, which crashes the process
// outright — and its cache would never be bounded by anything, so a passport
// read once would be believed for as long as the process lived.
type rebuilderFactory func() *Rebuilder

type syncWorker struct {
	river.WorkerDefaults[SyncArgs]
	store        *Store
	box          *secretbox.Box
	newClient    clientFactory
	newRebuilder rebuilderFactory
	reconciler   *Reconciler
	log          *slog.Logger
}

// NewSyncWorker builds the River worker that brings one connection's mirror,
// projection and reconciliation up to date. Register it with river.AddWorker.
func NewSyncWorker(store *Store, box *secretbox.Box, newClient clientFactory,
	newRebuilder rebuilderFactory, reconciler *Reconciler, log *slog.Logger,
) river.Worker[SyncArgs] {
	if log == nil {
		log = slog.Default()
	}
	return &syncWorker{
		store: store, box: box, newClient: newClient,
		newRebuilder: newRebuilder, reconciler: reconciler, log: log,
	}
}

// Timeout raises River's one-minute default; see syncTimeout.
func (w *syncWorker) Timeout(*river.Job[SyncArgs]) time.Duration { return syncTimeout }

// Work runs one connection's sync.
//
// THE ORDER IS LOAD-BEARING. Every link's mirror is brought up to date first,
// and only then is the projection rebuilt over the WHOLE connection: a transfer
// between two accounts of one connection is two mirror rows under two different
// links, and a rebuild that ran per link would see one leg at a time and pair
// nothing. The reconciliation comes last, when the journal it compares against
// is the one this run just produced.
//
// Nothing here is one transaction and nothing pretends to be. Each step is
// atomic on its own, the mirror is derived from the broker's whole history and
// everything after it from the mirror, so a crash between two steps leaves work
// for the next run rather than damage: the next run reads the same history
// again, computes the same projection, and finds most of it already there.
func (w *syncWorker) Work(ctx context.Context, job *river.Job[SyncArgs]) error {
	trigger, err := syncTrigger(job.Args.Trigger)
	if err != nil {
		w.log.Error("tinvest: a sync job names a trigger the run log cannot store",
			"connection", job.Args.ConnectionID, "trigger", job.Args.Trigger, "err", err)
		return err
	}

	conn, err := w.store.connectionForSync(ctx, job.Args.ConnectionID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The owner deleted the connection while this job waited its turn.
		// Nothing to do and nothing to retry: Debug, like every other routine
		// absence in this file.
		w.log.Debug(connectionGoneMessage, "connection", job.Args.ConnectionID)
		return nil
	}
	if err != nil {
		w.log.Error("tinvest: read the connection to sync failed", "connection", job.Args.ConnectionID, "err", err)
		return err
	}
	if conn.Status != StatusActive {
		w.log.Debug(connectionNotActiveMessage, "connection", conn.ID, "status", conn.Status)
		return nil
	}

	token, err := w.box.Open(conn.TokenCiphertext)
	if err != nil {
		// The stored secret will not decrypt — a changed or corrupted
		// encryption key, not a broker problem. Returned so the failure is
		// visible in the queue, and NOT parked as token_revoked: that word
		// would tell the owner to paste a new token, which would not help.
		w.log.Error("tinvest: decrypt the stored broker token failed", "connection", conn.ID, "err", err)
		return err
	}
	client, err := w.newClient(string(token))
	if err != nil {
		w.log.Error("tinvest: build the broker client failed", "connection", conn.ID, "err", err)
		return err
	}

	links, err := w.store.LinksByConnection(ctx, conn.ID)
	if err != nil {
		w.log.Error("tinvest: list the connection's account links failed", "connection", conn.ID, "err", err)
		return err
	}
	if len(links) == 0 {
		w.log.Debug(noLinksMessage, "connection", conn.ID)
		return nil
	}
	return w.sync(ctx, conn, links, client, trigger)
}

// linkRun is one link's run log entry and what this run has learned about it so
// far. It is filled in as the pipeline goes, so that a failure halfway through
// can still close every run with the counters it really achieved.
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

	for _, link := range links {
		run, err := w.store.StartRun(ctx, conn.ID, link.ID, trigger)
		if err != nil {
			return w.failed(ctx, conn, runs, err)
		}
		runs = append(runs, &linkRun{link: link, runID: run.ID})

		// The whole history, every time — see historyFrom. The broker's own
		// repeated rows are collapsed by SyncMirror itself (dedupInPage, called
		// on the way in), so this does not do it a second time: one rule, one
		// place, and MirrorSyncStats.Read is already the count after it.
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

	// One rebuild for the whole connection, over every link at once — see
	// Work's own note, and Rebuild's, on why a subset is not merely narrower.
	rebuilt, err := w.newRebuilder().Rebuild(ctx, conn, links, client)
	if err != nil {
		return w.failed(ctx, conn, runs, err)
	}

	for _, lr := range runs {
		// The verdict is kept whatever comes back with it: ReconcileLink
		// returns "not checked" alongside its error, and that is the value the
		// run has to carry.
		res, err := w.reconciler.ReconcileLink(ctx, client, conn, lr.link)
		lr.reconcile = res
		if err != nil {
			return w.failed(ctx, conn, runs, err)
		}
		// Counted per link and not taken from the rebuild, whose figure is for
		// the whole connection: a run belongs to one broker account, and a
		// connection-wide number on it would read as that account's own on
		// every screen that shows it.
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
			// The loop carries on: one refused write must not leave the
			// remaining runs open in the log as well. The run this one was for
			// stays "running" for good, which is what the store's own doc
			// describes an interrupted sync as looking like.
			w.logAt(ctx, err, "tinvest: close a finished sync run failed, it will stay open in the log",
				"connection", conn.ID, "run", lr.runID, "err", err)
			if closeErr == nil {
				closeErr = err
			}
		}
	}
	if closeErr != nil {
		// The work itself is done and the retry will find it done; what failed
		// is the log of it, and that is worth another attempt rather than a
		// silent gap.
		return closeErr
	}

	w.log.Info("tinvest: a sync run finished",
		"connection", conn.ID, "trigger", trigger, "links", len(links),
		"read", read, "added", added, "disappeared", gone, "unparsed", unparsed,
		"journal_added", rebuilt.Added, "journal_removed", rebuilt.Removed, "withdrawn", rebuilt.Withdrawn)
	return nil
}

// failed closes every run this attempt opened and decides what the queue is
// told.
//
// A TOKEN THE BROKER REFUSED IS NOT WORTH RETRYING. The connection is parked as
// needing a new one and nil is returned, so River lets the job go instead of
// offering a dead credential to the broker on every backoff for the rest of the
// day. Every other failure is returned, so the job is retried: a broker that
// answered 500, a database that was briefly away, a passport that did not
// arrive are all things a later attempt can get past.
//
// Every run opened is closed, including the ones that had already succeeded
// when a later link failed. Their counters are the ones they really achieved
// and their status is failed all the same: the projection was never rebuilt and
// nothing was reconciled, so the attempt did not do for them what a run is.
func (w *syncWorker) failed(ctx context.Context, conn Connection, runs []*linkRun, cause error) error {
	revoked := errors.Is(cause, ErrTokenInvalid)

	for _, lr := range runs {
		if err := w.store.FinishRun(ctx, lr.runID, RunOutcome{
			Status:           RunFailed,
			ReadCount:        lr.stats.Read,
			AddedCount:       lr.stats.Added,
			DisappearedCount: lr.stats.Disappeared,
			UnparsedCount:    lr.unparsed,
			Error:            cause.Error(),
			Reconcile:        lr.reconcile,
		}); err != nil {
			// The run stays "running" for good, which the store's own doc
			// describes as what an interrupted sync looks like. Said out loud
			// rather than swallowed, because from here on the run log is
			// telling the owner something slightly wrong.
			w.logAt(ctx, err, "tinvest: close a failed sync run failed, it will stay open in the log",
				"connection", conn.ID, "run", lr.runID, "err", err)
		}
	}

	if revoked {
		if err := w.store.UpdateConnectionStatus(ctx, conn.ID, StatusTokenRevoked); err != nil {
			// The connection stays active and the schedule will try again in an
			// hour, meeting the same refusal. Returned, so the queue retries
			// this job and gets another chance to park it.
			w.log.Error("tinvest: park a connection whose token the broker refused failed",
				"connection", conn.ID, "err", err)
			return err
		}
		w.log.Warn("tinvest: the broker refused this connection's token, it needs a new one",
			"connection", conn.ID, "err", cause)
		return nil
	}

	w.logAt(ctx, cause, "tinvest: a sync run failed", "connection", conn.ID, "err", cause)
	return cause
}

// logAt writes msg at Error, or at Debug when the failure is nothing but the
// caller having gone away — a shutdown cancelling the job's context, or the
// job's own timeout. The rule is local to this worker and deliberately not made
// general: this line answers "did the sync break?", and a cancelled run is not
// a lesser yes but a no. (marketdata's Converter makes the same distinction for
// the same reason, and says the same about not spreading it.)
func (w *syncWorker) logAt(ctx context.Context, err error, msg string, args ...any) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		w.log.Debug(msg, args...)
		return
	}
	w.log.Error(msg, args...)
}
