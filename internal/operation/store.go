package operation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/portfolio"
)

// ErrAccountNotInSpace means an operation named an account id that insertSQL's
// own WHERE clause could not find inside the caller's space (see insertSQL's
// doc: zero rows back means the account is not in the caller's space). Named
// so a caller can test for it instead of matching a bare pgx.ErrNoRows that,
// on its own, says nothing about which of the statement's two tables failed
// to produce a row.
var ErrAccountNotInSpace = errors.New("account not found in space")

// ErrRemovalCountMismatch means ApplyDelta's own DELETE found fewer rows than
// removeIDs named. Service.importRemovals already checks every id belongs to
// this space and is an importer's to remove before ApplyDelta ever runs (see
// ErrImportContract, the same fault named one layer up), so reaching this in
// practice means the journal moved between that check and this transaction.
// Either way, writing the part of the removal that still holds would leave
// half of a difference computed against a journal that no longer exists.
var ErrRemovalCountMismatch = errors.New("asked to remove operations that are not all there")

type Store struct{ db db.Executor }

func NewStore(x db.Executor) *Store { return &Store{db: x} }

// accountLockSQL takes the lock that makes an account's journal one writer's at
// a time, and doubles as the proof that the account is the caller's: no row back
// means it is not in this space, the same thing insertSQL's own WHERE clause
// says (see ErrAccountNotInSpace).
//
// FOR NO KEY UPDATE, not FOR UPDATE, and the difference is not cosmetic. Every
// INSERT into operations takes a FOR KEY SHARE lock on the account row for its
// foreign key, as does every balance written for that account — and FOR UPDATE
// conflicts with FOR KEY SHARE while FOR NO KEY UPDATE does not. The stronger
// mode would therefore have journal writers block anything that merely
// REFERENCES the account, which is a great deal more than the mutual exclusion
// wanted here. FOR NO KEY UPDATE conflicts with itself, which is exactly and
// only what this needs.
const accountLockSQL = `SELECT id FROM accounts WHERE space_id = $1 AND id = $2 FOR NO KEY UPDATE`

// WithAccountsLocked runs fn inside ONE transaction that holds an exclusive
// journal lock on each of accountIDs, with a Store bound to that transaction —
// so a caller can read an account's journal, decide on it, and write, with
// nothing able to slip in between.
//
// THAT WINDOW IS THE WHOLE REASON IT EXISTS (issue #17). The write paths judge a
// request by replaying the account's journal through the engine, and until this
// existed the replay ran on the pool, outside any transaction: two sells of the
// same holding, submitted at once, each saw a journal without the other, each
// was told it fitted, and both landed. The account then held a journal that no
// longer replays at all — every later read of its positions answering 422, for
// two requests that were each individually fine. Reading INSIDE the lock is what
// makes the second request see the first, so it is refused exactly as it would
// have been had the two arrived a second apart.
//
// The lock is taken one account at a time, in a sorted order, and both halves of
// that matter. Sorted, because a transfer locks two accounts and two transfers
// in opposite directions would otherwise take them in opposite orders and
// deadlock. One statement at a time, because a single statement locking several
// rows would have to rest on where the planner puts its locking step relative to
// its sort — a claim about the planner that nothing here needs to make. Two
// round trips is the most any caller of this package pays.
//
// fn's error is returned unchanged and rolls the transaction back: it is the
// caller's own decision about the caller's own domain, and dressing it up here
// would hide which of the two the failure was.
func (s *Store) WithAccountsLocked(ctx context.Context, spaceID uuid.UUID, accountIDs []uuid.UUID, fn func(*Store) error) error {
	ids := slices.Clone(accountIDs)
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	ids = slices.Compact(ids)

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, id := range ids {
		var locked uuid.UUID
		if err := tx.QueryRow(ctx, accountLockSQL, spaceID, id).Scan(&locked); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: %w", ErrAccountNotInSpace, pgx.ErrNoRows)
			}
			return err
		}
	}
	if err := fn(NewStore(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const cols = `id, space_id, account_id, instrument_id, type, occurred_on,
	settled_on, quantity, price, amount_minor, currency, fee_minor, note,
	transfer_group_id, split_ratio, source, external_id, created_at`

func scan(row pgx.Row) (Operation, error) {
	var o Operation
	err := row.Scan(&o.ID, &o.SpaceID, &o.AccountID, &o.InstrumentID, &o.Type,
		&o.OccurredOn, &o.SettledOn, &o.Quantity, &o.Price, &o.AmountMinor,
		&o.Currency, &o.FeeMinor, &o.Note, &o.TransferGroupID, &o.SplitRatio,
		&o.Source, &o.ExternalID, &o.CreatedAt)
	return o, err
}

// insertSQL guards space ownership of the account in the same statement:
// zero rows returned means the account is not in the caller's space.
//
// created_at is the one column of this statement a caller may either state or
// leave alone: NULL — what every path but ApplyDelta passes — takes this
// statement's own clock, and anything else is used as given. It became statable
// because a batch writes a whole history under one commit and the caller is the
// only one who knows the order it checked that history in (see ApplyDelta).
//
// THE CLOCK IS clock_timestamp() AND NOT now(), which is the difference between
// "when this row was written" and "when the enclosing transaction began". The
// two agree, to within the moment it took, for a caller that writes one row per
// transaction; they do not for one that writes several under one commit — a
// transfer pair, or the demo seed, which writes its whole journal a row at a
// time inside a single transaction. With now() every one of those rows claims
// one and the same instant, and
// two operations of the same date sharing one created_at leave the reads below
// nothing to order them by: ListForEngine would fold them in an order the
// database picks — in one of which a same-day sell precedes its buy and is an
// oversell — and the paged listing would have no total order to page over.
const insertSQL = `
	INSERT INTO operations (space_id, account_id, instrument_id, type,
		occurred_on, settled_on, quantity, price, amount_minor, currency,
		fee_minor, note, transfer_group_id, split_ratio, source, external_id,
		created_at)
	SELECT a.space_id, a.id, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
		COALESCE(NULLIF($15, ''), 'manual'), $16, COALESCE($17::timestamptz, clock_timestamp())
	FROM accounts a WHERE a.id = $2 AND a.space_id = $1
	RETURNING ` + cols

// insertArgs is insertSQL's argument list, in one place because two callers
// send that statement — one row at a time, and a whole delta as a batch.
func insertArgs(spaceID uuid.UUID, op Operation, createdAt *time.Time) []any {
	return []any{
		spaceID, op.AccountID, op.InstrumentID, op.Type, op.OccurredOn,
		op.SettledOn, op.Quantity, op.Price, op.AmountMinor, op.Currency,
		op.FeeMinor, op.Note, op.TransferGroupID, op.SplitRatio,
		op.Source, op.ExternalID, createdAt,
	}
}

// scanInserted reads back one row insertSQL returned, naming the one thing its
// WHERE clause can refuse: an account that is not the caller's.
func scanInserted(row pgx.Row) (Operation, error) {
	created, err := scan(row)
	if err == pgx.ErrNoRows {
		return Operation{}, fmt.Errorf("%w: %w", ErrAccountNotInSpace, pgx.ErrNoRows)
	}
	return created, err
}

// insertOne writes one operation and lets the database date it. The literal nil
// is the point: the row-at-a-time paths (Create, CreatePair) leave created_at to
// the statement's own clock, and cannot be made to state one by an operation
// that happens to carry a CreatedAt from somewhere. What that leaves them is the
// moment each row was actually written, rather than one moment shared by every
// row a caller wrote under the same commit — see insertSQL on why the clock has
// to be the statement's and not the transaction's.
func insertOne(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, spaceID uuid.UUID, op Operation,
) (Operation, error) {
	return scanInserted(q.QueryRow(ctx, insertSQL, insertArgs(spaceID, op, nil)...))
}

// Create inserts one operation and hands the row AS STORED to verify before
// the transaction commits, so a row the database cannot hold faithfully is
// rolled back instead of published.
//
// The distinction between the two is the whole point: quantity and split_ratio
// are stored on a fixed scale, so the operation Postgres keeps is not
// necessarily the one the caller checked, and a quantity that comes back a
// shade larger than the one validated is an oversell nobody learns about until
// somebody else's later read (see Service.Create for the fault this caught).
// Confirming the returned row closes that gap whatever rounding a column
// applies: the committed journal is always one that replays.
//
// verify's error is returned as-is and aborts the write. It describes a
// disagreement between this program and its own storage, not a bad request, so
// callers must not dress it up as a domain error. The transfer pair has the
// same guard for the same reason (see CreatePair).
//
// A nil verify means the caller has nothing to confirm — it is a plain insert
// then, for tests exercising storage itself rather than the journal.
func (s *Store) Create(ctx context.Context, spaceID uuid.UUID, op Operation, verify func(Operation) error) (Operation, error) {
	if verify == nil {
		return insertOne(ctx, s.db, spaceID, op)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Operation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created, err := insertOne(ctx, tx, spaceID, op)
	if err != nil {
		return Operation{}, err
	}
	if err := verify(created); err != nil {
		return Operation{}, err
	}
	return created, tx.Commit(ctx)
}

// insertLotSQL writes one piece of a transfer's FIFO breakdown. seq keeps
// the pieces in the FIFO order they were released in; the table's foreign
// key removes them with the operation they describe.
//
// It RETURNS the stored row rather than nothing, because quantity is
// NUMERIC(30,10) and what goes in is not always what comes out — the column
// has a scale and the value in memory does not. The caller publishes and
// checks the row Postgres kept, not the one it sent (see CreatePair).
//
// One statement per piece, but not one round trip per piece: the pieces are
// queued into a single pgx.Batch (see writeTransferLots).
const insertLotSQL = `
	INSERT INTO operation_transfer_lots (operation_id, seq, quantity, cost_minor, acquired_on)
	VALUES ($1, $2, $3, $4, $5)
	RETURNING quantity, cost_minor, acquired_on`

// writeTransferLots stores a transfer's FIFO breakdown next to the operation
// carrying it and returns the pieces AS THE DATABASE KEPT THEM, in the order
// they were released in.
//
// The pieces go out as one batch rather than one statement at a time. What they
// cost used to grow with the parcel's history rather than with the transfer: a
// position built up by a decade of monthly buying moves as some 120 pieces, and
// that was 120 statements, each waiting for the one before it (#73). Queued
// together they are a single write and a single wait, whatever the parcel has
// been through.
//
// What the batch was NOT allowed to change is which pieces come back. Each
// statement still RETURNS its stored row, and it is those rows that are handed
// on to be checked and published — never the arguments echoed back. The
// distinction is the whole reason the RETURNING is there: quantity is stored on
// a fixed scale, so a piece can come back a shade different from the one that
// went in, and a breakdown that was checked before the rounding and committed
// after it is the fault this project has already met twice (see CreatePair).
//
// Reading them back in queue order is the driver's documented behaviour, not an
// observation: pgx.BatchResults.Query "reads the results from the NEXT query in
// the batch", and the underlying protocol returns one result per queued
// statement in the order they were sent. Nothing here depends on the rows of
// any one statement arriving in a particular order — each statement writes and
// returns exactly one piece.
//
// A failure part way through behaves as the one-at-a-time loop did: the caller
// gets the FIRST error, naming the piece that caused it. That is not because
// the pieces queued behind it come back with some other, distinguishable
// error — they come back with nothing at all. pgx sends every statement in
// the batch before reading a single result back — one pipeline, synced once,
// after every statement is queued (see (*pgx.Conn).sendBatchExtendedWithDescription)
// — so when Postgres reaches the bad statement it discards whatever is still
// queued behind it, unread and unexecuted, all the way to that sync; the
// statement that would have been read next produces no result of its own, not
// an "aborted" one. Even a loop that kept reading past the first failure would
// not see a different error: pgx's own reader makes the first error sticky
// (pipelineBatchResults.Query returns the error already recorded on the batch
// rather than reading further once one read has failed), so what actually
// names the failing piece is the two things this loop does — it reads results
// in order, and it returns on the first error, before asking for a second one.
// Since every statement of the batch runs inside the transaction CreatePair
// opened, the pieces written before the bad one go with the pair when it is
// rolled back. The results are closed before returning either way: the
// connection cannot be used again, not even to roll back, while a batch's
// results are outstanding.
func writeTransferLots(ctx context.Context, tx pgx.Tx, operationID uuid.UUID, lots []ReleasedLot) ([]ReleasedLot, error) {
	batch := &pgx.Batch{}
	for i, lot := range lots {
		batch.Queue(insertLotSQL, operationID, i, lot.Quantity, lot.CostMinor, lot.AcquiredOn)
	}
	br := tx.SendBatch(ctx, batch)
	stored := make([]ReleasedLot, 0, len(lots))
	for i := range lots {
		var back ReleasedLot
		if err := br.QueryRow().Scan(&back.Quantity, &back.CostMinor, &back.AcquiredOn); err != nil {
			_ = br.Close()
			return nil, fmt.Errorf("transfer lot %d: %w", i, err)
		}
		stored = append(stored, back)
	}
	if err := br.Close(); err != nil {
		return nil, fmt.Errorf("transfer lots: %w", err)
	}
	return stored, nil
}

// CreatePair inserts a transfer_out/transfer_in pair atomically with a
// shared transfer_group_id, together with the FIFO breakdown carried on the
// receiving leg (in.TransferLots). All of it lands in one transaction: a
// transfer_in that lost its breakdown would arrive as a single lot that knows
// nothing about when it was bought (see portfolio.Lot.AcquiredOn), and the
// destination's whole ruble basis with it — dates that were resolvable at
// write time and are not resolvable ever again.
//
// Both returned legs carry that breakdown, though only one of them stores it,
// for the reason attachTransferLots gives at every later read: the pieces
// describe one parcel and the departing leg is that same parcel leaving.
//
// Everything it returns has been read back out of the database, never handed
// through from the arguments — both operations come from the INSERT's
// RETURNING, and so now do the pieces. That is not ceremony: quantities are
// stored with a fixed scale, so a piece can come back a shade different from
// the one that went in, and a response describing pieces that are not in the
// table is a response nobody can act on. The pieces as stored are then run
// through the engine's own check before the transaction commits, so a pair
// whose breakdown does not add up in the database is never committed at all
// — instead of being accepted and failing every later read of the receiving
// account (see portfolio.CheckTransferLots).
//
// verify is the caller's own last look at the pair AS STORED, before the
// transaction commits, and it is the same guard Create has for a single
// operation — for a reason that has grown sharper. The departing leg no longer
// merely records a quantity: it RELEASES the very pieces stored here (see
// portfolio.Position.releaseRecorded), so the row that will be replayed on the
// source account from now on is this one, with the quantities the columns
// rounded to and the pieces the table gave back, not the one the service
// checked in memory. Confirming the stored pair replays is what keeps "accepted
// with a 201, then refused on every later read" from returning by a new door;
// the project has been through that door twice already (see quantizeLots and
// normalizeForStorage). A nil verify means the caller has nothing to confirm —
// a plain insert, for tests exercising storage itself.
func (s *Store) CreatePair(ctx context.Context, spaceID uuid.UUID, out, in Operation, verify func(out, in Operation) error) (Operation, Operation, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Operation{}, Operation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	group := uuid.New()
	out.TransferGroupID = &group
	in.TransferGroupID = &group

	cOut, err := insertOne(ctx, tx, spaceID, out)
	if err != nil {
		return Operation{}, Operation{}, fmt.Errorf("transfer out: %w", err)
	}
	cIn, err := insertOne(ctx, tx, spaceID, in)
	if err != nil {
		return Operation{}, Operation{}, fmt.Errorf("transfer in: %w", err)
	}
	if len(in.TransferLots) > 0 {
		stored, err := writeTransferLots(ctx, tx, cIn.ID, in.TransferLots)
		if err != nil {
			return Operation{}, Operation{}, err
		}
		cIn.TransferLots = stored
		// The departing leg gets them too, WHEN THE PAIR IS ONE PARCEL. The rows
		// are stored next to the arriving leg only, but they describe THE
		// PARCEL, and a transfer's pair is one parcel with the opposite sign —
		// which is exactly what attachTransferLots decided for every later read
		// (see its doc). Handing back an out leg with an empty breakdown made
		// the pair contradict itself within a single response: whether a
		// transfer knows when its shares were bought is published per operation
		// (Operation.has_undated_lots, see the API contract), and the departing
		// leg would have answered "no" about a parcel whose dates are sitting in
		// the same transaction.
		//
		// A CONVERSION IS NOT ONE PARCEL and so is not copied here: its legs
		// name different papers and different counts, and the departing one
		// carries and stores a breakdown of its own just below (see
		// carriesOwnLots). Copying the arriving leg's pieces onto it would put M
		// units of the NEW paper on the row that gave up N of the old — a
		// breakdown that does not sum to the quantity of the row carrying it,
		// which is precisely what the check below refuses.
		if !carriesOwnLots(out) {
			cOut.TransferLots = stored
		}
		if err := portfolio.CheckTransferLots(cIn); err != nil {
			// The rows are already in this transaction, so refusing here rolls
			// them back. Reaching this means the write path built a breakdown
			// the storage cannot hold faithfully — a bug in this program, not
			// something the caller did — so it is not one of the domain errors
			// and surfaces as a server error, loudly, on the request that
			// caused it rather than on every future read by someone else.
			return Operation{}, Operation{}, fmt.Errorf("transfer lots as stored: %w", err)
		}
	}
	// The departing leg's own breakdown, for the pair whose two legs are two
	// different parcels. Written and checked exactly like the arriving one, and
	// against ITS row: a conversion's out leg claims N units of the old paper and
	// its pieces must sum to N, while the in leg's sum to M of the new.
	if carriesOwnLots(out) {
		stored, err := writeTransferLots(ctx, tx, cOut.ID, out.TransferLots)
		if err != nil {
			return Operation{}, Operation{}, err
		}
		cOut.TransferLots = stored
		if err := portfolio.CheckTransferLots(cOut); err != nil {
			return Operation{}, Operation{}, fmt.Errorf("transfer lots as stored: %w", err)
		}
	}
	if verify != nil {
		if err := verify(cOut, cIn); err != nil {
			return Operation{}, Operation{}, err
		}
	}
	return cOut, cIn, tx.Commit(ctx)
}

// carriesOwnLots reports whether this operation's FIFO breakdown is stored next
// to the operation itself.
//
// It is the WRITE side of attachTransferLots' carrier resolution and has to
// stay its mirror: that query reads a departing leg's pieces off the arriving
// leg it shares a group with, and falls back to the row itself only when there
// is no such sibling. So a transfer_out that has a group stores nothing of its
// own — the pieces would be written twice and read once, and two copies of one
// fact eventually disagree — while a transfer_out with no sibling anywhere
// (shares that left for another broker, which only an import can record) stores
// its own, because it is then the only row that can hold them.
//
// BOTH LEGS OF A CONVERSION STORE THEIR OWN, and that is not an exception to the
// rule above but the same rule applied to a pair whose legs describe DIFFERENT
// parcels: a conversion changes the paper and the number of units, so the
// departing leg's pieces sum to N of the old and the arriving leg's to M of the
// new (see portfolio.TypeExchangeOut). Neither list can be read off the other,
// and the query above never tries — its sibling join fires for transfer_out
// alone, so an exchange_out resolves to itself and finds the rows written here.
func carriesOwnLots(op Operation) bool {
	if len(op.TransferLots) == 0 {
		return false
	}
	switch op.Type {
	case TypeTransferIn, TypeExchangeOut, TypeExchangeIn:
		return true
	}
	return op.TransferGroupID == nil
}

// ApplyDelta applies one importer's difference to the journal: removals first,
// then every insertion as a single batch, then the caller's own look at the
// result AS STORED, and only then a commit. All of it in ONE transaction, over
// however many accounts of the space the difference touches.
//
// THE ORDER OF THE TWO HALVES IS PART OF THE CONTRACT. A broker record that was
// corrected keeps its identity, so the row replacing it carries the very
// external id the row being replaced still holds, and the journal's unique
// index would refuse the pair of them. Removing first is what makes a
// correction expressible at all.
//
// EVERY id IN removeIDs MUST BE THERE. A caller that asks to remove a row that
// is gone computed its difference against a journal that has since moved, and
// the rest of that difference cannot be trusted either; obeying the part that
// still applies would write half of a stale decision.
//
// created_at is stated by the caller rather than left to the statement's clock
// (see insertSQL). It is not decoration. The rows of a delta go out as one
// batch, back to back, and a clock read microseconds apart is not a promise
// that two of them differ — while two operations of the same date sharing one
// created_at leave ListForEngine's ORDER BY nothing to separate them by, so
// the journal would fold in an order the database picks rather than the order
// the caller checked. That is the shape of "accepted on write, refused on every
// later read" this package has met twice. The caller is also the only one that
// knows that order, and the only one that can put a rewritten row back where
// the row it replaces stood (see ImportDelta).
//
// verify is handed the rows AS STORED — from the INSERT's RETURNING, with the
// breakdowns the lot table gave back — for the reason Create and CreatePair
// both give: quantities are stored on a fixed scale, so what was checked in
// memory is not necessarily what the columns kept, and a delta whose stored
// form no longer replays must be rolled back on the sync that caused it rather
// than break every later read. Its error is returned as-is: it describes a
// disagreement between this program and its own storage, not a bad request.
func (s *Store) ApplyDelta(ctx context.Context, spaceID uuid.UUID, add []Operation, removeIDs []uuid.UUID,
	verify func(stored []Operation) error,
) ([]Operation, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if len(removeIDs) > 0 {
		ct, err := tx.Exec(ctx, `DELETE FROM operations WHERE space_id = $1 AND id = ANY($2)`, spaceID, removeIDs)
		if err != nil {
			return nil, fmt.Errorf("apply delta: %w", err)
		}
		if int(ct.RowsAffected()) != len(removeIDs) {
			return nil, fmt.Errorf("%w: asked to remove %d operations, found %d",
				ErrRemovalCountMismatch, len(removeIDs), ct.RowsAffected())
		}
	}

	stored, err := insertBatch(ctx, tx, spaceID, add)
	if err != nil {
		return nil, err
	}
	if err := storeBreakdowns(ctx, tx, add, stored); err != nil {
		return nil, err
	}
	if verify != nil {
		if err := verify(stored); err != nil {
			return nil, err
		}
	}
	return stored, tx.Commit(ctx)
}

// insertBatch writes every operation of a delta as one batch and returns the
// rows the database kept, in the order they were given.
//
// One batch and not one statement apiece: the first load of a broker's history
// is thousands of operations, and a statement that waits for the one before it
// makes that a thousand waits (the same argument writeTransferLots makes for
// the pieces of one transfer). What the batch is not allowed to change is which
// rows come back — each statement still RETURNS its stored row, and it is those
// that travel on, never the arguments echoed back.
//
// A failure part way through behaves as a loop would: the caller gets the FIRST
// error, naming the row that caused it, and the rows written before it go with
// the transaction when it rolls back. See writeTransferLots for why the
// statements queued behind the failing one cannot produce a competing error.
func insertBatch(ctx context.Context, tx pgx.Tx, spaceID uuid.UUID, add []Operation) ([]Operation, error) {
	if len(add) == 0 {
		return nil, nil
	}
	batch := &pgx.Batch{}
	for _, op := range add {
		var createdAt *time.Time
		if !op.CreatedAt.IsZero() {
			at := op.CreatedAt
			createdAt = &at
		}
		batch.Queue(insertSQL, insertArgs(spaceID, op, createdAt)...)
	}
	br := tx.SendBatch(ctx, batch)
	stored := make([]Operation, 0, len(add))
	for i := range add {
		o, err := scanInserted(br.QueryRow())
		if err != nil {
			_ = br.Close()
			return nil, fmt.Errorf("operation %d: %w", i, err)
		}
		stored = append(stored, o)
	}
	if err := br.Close(); err != nil {
		return nil, fmt.Errorf("operations: %w", err)
	}
	return stored, nil
}

// storeBreakdowns writes the FIFO breakdown of every transfer in the delta that
// owns one (see carriesOwnLots) and puts the STORED pieces on every row that
// reads them — including the departing leg of a pair, whose pieces live next to
// its sibling. Both halves matter to what verify then sees: the source account
// folds the pieces the table gave back, not the ones that went in.
func storeBreakdowns(ctx context.Context, tx pgx.Tx, add, stored []Operation) error {
	byGroup := make(map[uuid.UUID][]ReleasedLot)
	for i := range stored {
		if !carriesOwnLots(add[i]) {
			continue
		}
		back, err := writeTransferLots(ctx, tx, stored[i].ID, add[i].TransferLots)
		if err != nil {
			return err
		}
		stored[i].TransferLots = back
		if err := portfolio.CheckTransferLots(stored[i]); err != nil {
			// The rows are already in this transaction, so refusing here rolls
			// them back. Same reasoning as CreatePair: a breakdown the storage
			// cannot hold faithfully is a bug in this program, surfaced loudly
			// now rather than on every future read.
			return fmt.Errorf("transfer lots as stored: %w", err)
		}
		if stored[i].TransferGroupID != nil {
			byGroup[*stored[i].TransferGroupID] = back
		}
	}
	for i := range stored {
		if len(stored[i].TransferLots) > 0 || stored[i].TransferGroupID == nil {
			continue
		}
		if pieces, ok := byGroup[*stored[i].TransferGroupID]; ok {
			stored[i].TransferLots = pieces
			continue
		}
		if len(add[i].TransferLots) > 0 {
			// A departing leg was given a breakdown but its arriving leg is not
			// in this delta, so nothing stored those pieces and nothing will read
			// them back. Silently dropping them would leave the row folding a
			// fresh slice of the queue instead of the parcel that was checked.
			return fmt.Errorf("operation %d carries a transfer breakdown but the leg that stores it is not in this delta", i)
		}
	}
	return nil
}

func (s *Store) list(ctx context.Context, sql string, args ...any) ([]Operation, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Operation
	for rows.Next() {
		o, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ListByAccount returns one page of the account's journal, newest first, with
// the FIFO breakdown attached to the transfers that have one, and whether the
// journal holds anything beyond that page.
//
// The breakdown is here for the same reason the engine gets it: a transfer's
// amount is a basis assembled from purchases on several days, and expressing it
// in the space's base currency means converting each piece at the rate of the
// day it was bought (see Handler.operationInBase). Without the pieces the row
// would have to be converted at the rate of the day the shares changed brokers,
// which is exactly the misvaluation this whole mechanism exists to prevent —
// and the journal would print a different number than the position screen for
// the same shares.
//
// THE SECOND RESULT IS FETCHED, NOT INFERRED. One row beyond the page is asked
// for, and whether it arrives IS the answer: the query reads it, and the trim
// three statements later drops it again before anything downstream can mistake
// it for part of the page. Nothing may substitute a comparison of the page's
// length against the limit for this: a full page is exactly where that
// comparison has nothing to say, and it was wrong outright while the handler
// clamped an over-large limit — which is how a truncated journal came to present
// itself as a whole one (#86). The clamp is gone (#118) and the substitution is
// still not available, because the full-page case is the one the flag is for.
// Counting the
// table instead would cost a second pass over rows just read, to publish a total
// nothing displays and that a concurrent write could put at odds with the very
// page it travels with.
//
// limit is the size of the page the caller wants and must be positive — enforced
// below rather than merely asked for, since a zero asks the query for the probe
// row alone and then trims the page down to nothing, which publishes an empty
// page with hasMore true: a journal showing nothing behind a control that loads
// nothing however often it is pressed, and a negative panics on the same trim.
// The refusal is a plain error, not a validation one: today's caller defaults
// and refuses before it reaches here (parsePage, called from
// handleListByAccount), so a bad limit arriving means the program is wrong, not
// the person using it.
func (s *Store) ListByAccount(ctx context.Context, spaceID, accountID uuid.UUID, limit, offset int) ([]Operation, bool, error) {
	if limit < 1 {
		return nil, false, fmt.Errorf("list operations: limit must be positive, got %d", limit)
	}
	ops, err := s.list(ctx, `SELECT `+cols+` FROM operations
		WHERE space_id = $1 AND account_id = $2
		ORDER BY occurred_on DESC, created_at DESC LIMIT $3 OFFSET $4`,
		spaceID, accountID, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	hasMore := len(ops) > limit
	if hasMore {
		ops = ops[:limit]
	}
	// After the trim, never before: the probe row is not part of the page and
	// must not have its breakdown fetched, let alone published.
	if err := s.attachTransferLots(ctx, spaceID, ops); err != nil {
		return nil, false, err
	}
	return ops, hasMore, nil
}

// ListForEngine returns the account's full journal in engine order, with the
// FIFO breakdown attached to the transfers that have one. The breakdown is
// journal data the engine needs: it dates the lots a transfer brought in.
func (s *Store) ListForEngine(ctx context.Context, spaceID, accountID uuid.UUID) ([]Operation, error) {
	ops, err := s.list(ctx, `SELECT `+cols+` FROM operations
		WHERE space_id = $1 AND account_id = $2
		`+engineOrder, spaceID, accountID)
	if err != nil {
		return nil, err
	}
	if err := s.attachTransferLots(ctx, spaceID, ops); err != nil {
		return nil, err
	}
	return ops, nil
}

// attachTransferLots fills TransferLots on the given operations. It is a
// separate query rather than a join onto the journal so that an operation
// with several pieces stays a single journal entry. Anything without stored
// pieces keeps an empty list: every row that is neither a transfer nor a
// conversion leg, a transfer whose basis was given by hand, and a transfer
// recorded before the breakdown was kept.
//
// A CONVERSION'S TWO LEGS EACH READ THEIR OWN, and the query below already does
// that without a word about them: its sibling join is conditioned on
// `o.type = 'transfer_out'`, so an exchange_out resolves to itself and picks up
// the rows written next to it (see carriesOwnLots for why it has its own at
// all — the legs of a conversion are two different parcels, N units of the old
// paper and M of the new).
//
// BOTH legs of a transfer pair get the breakdown, though only one of them
// stores it. The rows are written next to the receiving leg, whose account
// cannot recover the acquisition dates any other way (see the 0007 migration),
// but the pieces do not describe an arrival — they describe the parcel, and the
// departing leg is the same parcel with the opposite sign: same instrument,
// same quantity, same basis, same purchases behind it. Leaving the sending leg
// without them meant it was the one row in the system still converting that
// basis at the rate of the day the shares changed brokers, so the source
// account's journal printed 149 150 ₽ for the very shares the destination's
// journal and positions both printed 118 000 ₽ for. One pair, one set of
// purchases, one answer.
//
// Resolving the sibling here rather than duplicating rows keeps the breakdown a
// single fact with a single owner: nothing can drift, and a transfer recorded
// before this table still has no pieces on either leg, which is the honest
// answer for it.
//
// It selects by the operations in hand rather than by their account, so one
// page of a journal costs one page's worth of pieces rather than every
// transfer the account has ever received. Every join stays scoped to the
// caller's space: an id is not by itself proof of ownership, and neither is a
// transfer_group_id.
func (s *Store) attachTransferLots(ctx context.Context, spaceID uuid.UUID, ops []Operation) error {
	if len(ops) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(ops))
	for _, o := range ops {
		ids = append(ids, o.ID)
	}
	rows, err := s.db.Query(ctx, `
		WITH carriers AS (
			SELECT o.id, COALESCE(peer.id, o.id) AS carrier
			FROM operations o
			LEFT JOIN operations peer
				ON o.type = 'transfer_out'
				AND peer.space_id = o.space_id
				AND peer.transfer_group_id = o.transfer_group_id
				AND peer.type = 'transfer_in'
			WHERE o.space_id = $1 AND o.id = ANY($2)
		)
		SELECT c.id, l.quantity, l.cost_minor, l.acquired_on
		FROM carriers c
		JOIN operation_transfer_lots l ON l.operation_id = c.carrier
		ORDER BY c.id, l.seq`, spaceID, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	byOperation := make(map[uuid.UUID][]ReleasedLot)
	for rows.Next() {
		var id uuid.UUID
		var lot ReleasedLot
		if err := rows.Scan(&id, &lot.Quantity, &lot.CostMinor, &lot.AcquiredOn); err != nil {
			return err
		}
		byOperation[id] = append(byOperation[id], lot)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range ops {
		ops[i].TransferLots = byOperation[ops[i].ID]
	}
	return nil
}

// ListBySource returns every operation of the account that came from the named
// source, in engine order, with the FIFO breakdown attached — the journal side
// of what an importer has to diff its own rows against.
//
// It carries the breakdown for the same reason ListForEngine does, and it is
// not optional here either: these rows are folded to work out what the account
// already holds, and a transfer that lost its pieces folds into a position with
// no acquisition dates and a basis nothing can convert, while nothing about the
// row says it came back incomplete.
func (s *Store) ListBySource(ctx context.Context, spaceID, accountID uuid.UUID, source string) ([]Operation, error) {
	ops, err := s.list(ctx, `SELECT `+cols+` FROM operations
		WHERE space_id = $1 AND account_id = $2 AND source = $3
		`+engineOrder, spaceID, accountID, source)
	if err != nil {
		return nil, err
	}
	if err := s.attachTransferLots(ctx, spaceID, ops); err != nil {
		return nil, err
	}
	return ops, nil
}

// ByIDs returns the operations of the space with the given ids, in engine
// order, with the FIFO breakdown attached (see ListBySource for why that is not
// optional). Ids that are not in the space simply do not come back: whether a
// caller may act on a missing row is the caller's rule, not this query's.
func (s *Store) ByIDs(ctx context.Context, spaceID uuid.UUID, ids []uuid.UUID) ([]Operation, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ops, err := s.list(ctx, `SELECT `+cols+` FROM operations
		WHERE space_id = $1 AND id = ANY($2)
		`+engineOrder, spaceID, ids)
	if err != nil {
		return nil, err
	}
	if err := s.attachTransferLots(ctx, spaceID, ops); err != nil {
		return nil, err
	}
	return ops, nil
}

func (s *Store) ByID(ctx context.Context, spaceID, id uuid.UUID) (Operation, error) {
	return scan(s.db.QueryRow(ctx, `SELECT `+cols+` FROM operations
		WHERE space_id = $1 AND id = $2`, spaceID, id))
}

// ByTransferGroup returns every operation sharing groupID (the two legs of a
// transfer pair, which live on two different accounts).
func (s *Store) ByTransferGroup(ctx context.Context, spaceID, groupID uuid.UUID) ([]Operation, error) {
	return s.list(ctx, `SELECT `+cols+` FROM operations
		WHERE space_id = $1 AND transfer_group_id = $2`, spaceID, groupID)
}

// EarliestOccurredOn returns the earliest occurred_on across all operations
// in the instance (not scoped to a space: the fx backfill it feeds is
// shared, not per-space). This is a plain data query for the range's start,
// not a decision about what to backfill. pgx.ErrNoRows if there are no
// operations at all.
func (s *Store) EarliestOccurredOn(ctx context.Context) (time.Time, error) {
	var on *time.Time
	err := s.db.QueryRow(ctx, `SELECT MIN(occurred_on) FROM operations`).Scan(&on)
	if err != nil {
		return time.Time{}, err
	}
	if on == nil {
		return time.Time{}, pgx.ErrNoRows
	}
	return *on, nil
}

// DistinctCurrencies returns the sorted set of currencies used by any
// operation in the instance (not scoped to a space: same rationale as
// EarliestOccurredOn — fx coverage is shared, not per-space). A currency can
// appear here without appearing in account.Store's list (e.g. a one-off
// operation in a currency no account is denominated in), so this queries
// operations directly rather than reusing account currencies. Deciding what
// to backfill is not this method's job — that is the fx backfill job's.
// Returns an empty slice, not an error, when there are no operations: unlike
// EarliestOccurredOn, "no currencies in use" is itself a meaningful answer,
// not a missing value.
func (s *Store) DistinctCurrencies(ctx context.Context) ([]string, error) {
	rows, err := s.db.Query(ctx, `SELECT DISTINCT currency FROM operations ORDER BY currency`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Delete removes the operation; if it belongs to a transfer group, the whole
// group is removed. Returns the number of deleted rows.
func (s *Store) Delete(ctx context.Context, spaceID, id uuid.UUID) (int, error) {
	ct, err := s.db.Exec(ctx, `
		DELETE FROM operations
		WHERE space_id = $1 AND (id = $2 OR transfer_group_id = (
			SELECT transfer_group_id FROM operations
			WHERE space_id = $1 AND id = $2 AND transfer_group_id IS NOT NULL
		))`, spaceID, id)
	if err != nil {
		return 0, err
	}
	if ct.RowsAffected() == 0 {
		return 0, pgx.ErrNoRows
	}
	return int(ct.RowsAffected()), nil
}
