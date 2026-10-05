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

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/portfolio"
)

// ErrAccountNotInSpace means an operation named an account outside the caller's
// space: insertSQL's WHERE clause found no row.
var ErrAccountNotInSpace = errors.New("account not found in space")

// ErrAccountArchived means a hand entry named an archived account. It is out of
// every total, so it is brought back from the archive before it is written to.
var ErrAccountArchived = fmt.Errorf("%w: the account is archived; bring it back from the archive to change it", family.ErrValidation)

// ErrRemovalCountMismatch means ApplyDelta's DELETE found fewer rows than
// removeIDs named: the journal moved after Service.importRemovals checked them.
// Writing the rest would apply half of a stale difference.
var ErrRemovalCountMismatch = errors.New("asked to remove operations that are not all there")

type Store struct{ db db.Executor }

func NewStore(x db.Executor) *Store { return &Store{db: x} }

// accountLockSQL takes the lock that gives an account's journal one writer at a
// time, and proves the account is the caller's (no row: not in this space).
//
// FOR NO KEY UPDATE, not FOR UPDATE: every insert into operations, and every
// balance written for the account, takes FOR KEY SHARE on the account row for its
// foreign key, and FOR UPDATE would block those too. FOR NO KEY UPDATE conflicts
// only with itself.
const accountLockSQL = `SELECT status FROM accounts WHERE space_id = $1 AND id = $2 FOR NO KEY UPDATE`

// WithAccountsLocked runs fn in one transaction holding an exclusive journal
// lock on each of accountIDs, with a Store bound to that transaction, so a caller
// can read a journal, decide and write with nothing slipping in between. Without
// it, two concurrent sells of one holding were both accepted and left a journal
// that no longer replays (#17).
//
// Locks are taken one account per statement in sorted order: sorted so that two
// opposite transfers cannot deadlock, one at a time so nothing rests on where the
// planner puts the lock relative to the sort. fn's error is returned unchanged
// and rolls back.
func (s *Store) WithAccountsLocked(ctx context.Context, spaceID uuid.UUID, accountIDs []uuid.UUID, fn func(*Store) error) error {
	return s.withAccountsLocked(ctx, spaceID, accountIDs, false, fn)
}

// WithOpenAccountsLocked is WithAccountsLocked for a hand entry: it also refuses
// an archived account with ErrAccountArchived, reading the status under the same
// lock. An importer uses WithAccountsLocked: what a broker reports is recorded
// whatever the family has done with the account since.
func (s *Store) WithOpenAccountsLocked(ctx context.Context, spaceID uuid.UUID, accountIDs []uuid.UUID, fn func(*Store) error) error {
	return s.withAccountsLocked(ctx, spaceID, accountIDs, true, fn)
}

func (s *Store) withAccountsLocked(ctx context.Context, spaceID uuid.UUID, accountIDs []uuid.UUID, open bool, fn func(*Store) error) error {
	ids := slices.Clone(accountIDs)
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	ids = slices.Compact(ids)

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, id := range ids {
		var status account.Status
		if err := tx.QueryRow(ctx, accountLockSQL, spaceID, id).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: %w", ErrAccountNotInSpace, pgx.ErrNoRows)
			}
			return err
		}
		if open && status == account.StatusArchived {
			return ErrAccountArchived
		}
	}
	if err := fn(NewStore(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const cols = `id, space_id, account_id, instrument_id, type, occurred_on,
	settled_on, quantity, price, amount_minor, currency, fee_minor, note,
	trading_mode, transfer_group_id, split_ratio, source, external_id, created_at,
	occurred_at, face_before_minor`

func scan(row pgx.Row) (Operation, error) {
	var o Operation
	err := row.Scan(&o.ID, &o.SpaceID, &o.AccountID, &o.InstrumentID, &o.Type,
		&o.OccurredOn, &o.SettledOn, &o.Quantity, &o.Price, &o.AmountMinor,
		&o.Currency, &o.FeeMinor, &o.Note, &o.TradingMode, &o.TransferGroupID,
		&o.SplitRatio, &o.Source, &o.ExternalID, &o.CreatedAt, &o.OccurredAt, &o.FaceBeforeMinor)
	return o, err
}

// insertSQL checks the account belongs to the space in the same statement: no
// row back means it does not.
//
// created_at may be stated (ApplyDelta, which knows the order it checked) or left
// NULL, which takes clock_timestamp(). Not now(): several rows written under one
// commit (a transfer pair, the demo seed) would share one instant, and same-day
// rows with equal created_at give ListForEngine nothing to order by, so a same-day
// sell could fold before its buy.
const insertSQL = `
	INSERT INTO operations (space_id, account_id, instrument_id, type,
		occurred_on, settled_on, quantity, price, amount_minor, currency,
		fee_minor, note, trading_mode, transfer_group_id, split_ratio, source,
		external_id, created_at, occurred_at, face_before_minor)
	SELECT a.space_id, a.id, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
		COALESCE(NULLIF($16, ''), 'manual'), $17, COALESCE($18::timestamptz, clock_timestamp()), $19, $20
	FROM accounts a WHERE a.id = $2 AND a.space_id = $1
	RETURNING ` + cols

// insertArgs is insertSQL's argument list, shared by the single-row and batch
// callers.
func insertArgs(spaceID uuid.UUID, op Operation, createdAt *time.Time) []any {
	return []any{
		spaceID, op.AccountID, op.InstrumentID, op.Type, op.OccurredOn,
		op.SettledOn, op.Quantity, op.Price, op.AmountMinor, op.Currency,
		op.FeeMinor, op.Note, op.TradingMode, op.TransferGroupID, op.SplitRatio,
		op.Source, op.ExternalID, createdAt, op.OccurredAt, op.FaceBeforeMinor,
	}
}

// scanInserted reads back one row insertSQL returned.
func scanInserted(row pgx.Row) (Operation, error) {
	created, err := scan(row)
	if err == pgx.ErrNoRows {
		return Operation{}, fmt.Errorf("%w: %w", ErrAccountNotInSpace, pgx.ErrNoRows)
	}
	return created, err
}

// insertOne writes one operation and lets the database date it: nil created_at,
// whatever CreatedAt the operation carries (see insertSQL).
func insertOne(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, spaceID uuid.UUID, op Operation,
) (Operation, error) {
	return scanInserted(q.QueryRow(ctx, insertSQL, insertArgs(spaceID, op, nil)...))
}

// Create inserts one operation and hands the row as stored to verify before the
// commit. quantity and split_ratio are stored on a fixed scale, so the stored row
// may differ from the checked one; verifying it keeps the committed journal one
// that replays. verify's error is returned as is: it is a disagreement between
// this program and its storage, not a bad request. A nil verify is a plain insert,
// for storage tests.
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

// insertLotSQL writes one piece of a transfer's FIFO breakdown; seq keeps FIFO
// order and the foreign key removes pieces with their operation. It returns the
// stored row because quantity is NUMERIC(30,10) and may come back rounded.
const insertLotSQL = `
	INSERT INTO operation_transfer_lots (operation_id, seq, quantity, cost_minor, acquired_on, rate_on)
	VALUES ($1, $2, $3, $4, $5, $6)
	RETURNING quantity, cost_minor, acquired_on, rate_on`

// writeTransferLots stores a breakdown next to the operation carrying it and
// returns the pieces as stored, in release order. The pieces go out as one
// pgx.Batch, so a long-held position's 120 pieces cost one round trip, not 120
// (#73). Each statement returns its stored row, and those rows travel on, never
// the arguments.
//
// Results are read in queue order, as pgx documents. On failure the first error
// names the failing piece: Postgres discards everything queued after it, and pgx
// keeps the first error sticky. The enclosing transaction rolls back the pieces
// written before it. Results are closed either way, since the connection is
// unusable while they are outstanding.
func writeTransferLots(ctx context.Context, tx pgx.Tx, operationID uuid.UUID, lots []ReleasedLot) ([]ReleasedLot, error) {
	batch := &pgx.Batch{}
	for i, lot := range lots {
		batch.Queue(insertLotSQL, operationID, i, lot.Quantity, lot.CostMinor, lot.AcquiredOn, lot.RateOn)
	}
	br := tx.SendBatch(ctx, batch)
	stored := make([]ReleasedLot, 0, len(lots))
	for i := range lots {
		var back ReleasedLot
		if err := br.QueryRow().Scan(&back.Quantity, &back.CostMinor, &back.AcquiredOn, &back.RateOn); err != nil {
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

// CreatePair inserts a transfer_out/transfer_in pair with a shared
// transfer_group_id and the breakdown carried on the arriving leg, in one
// transaction: an arrival that lost its breakdown would lose its purchase dates
// for good (see portfolio.Lot.AcquiredOn).
//
// Everything returned is read back from the database, and the stored pieces pass
// portfolio.CheckTransferLots before the commit. verify is the caller's last look
// at the stored pair, as for Create: the departing leg replays the stored pieces
// (see portfolio.Position.releaseRecorded), so it is that row, not the checked
// one, that later reads fold. A nil verify is a plain insert.
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
		// The departing leg of a transfer gets the same pieces: one parcel with
		// the opposite sign, as attachTransferLots reads it later, so the pair
		// does not contradict itself on has_undated_lots. A conversion is two
		// parcels; its out leg stores its own below (see carriesOwnLots).
		if !carriesOwnLots(out) {
			cOut.TransferLots = stored
		}
		if err := checkStoredLots(cIn); err != nil {
			// Refusing rolls the rows back. A breakdown the storage cannot hold
			// faithfully is this program's bug, so it surfaces as a server error on
			// the request that caused it.
			return Operation{}, Operation{}, fmt.Errorf("transfer lots as stored: %w", err)
		}
	}
	// A conversion's out leg: N units of the old paper, checked against its
	// own row.
	if carriesOwnLots(out) {
		stored, err := writeTransferLots(ctx, tx, cOut.ID, out.TransferLots)
		if err != nil {
			return Operation{}, Operation{}, err
		}
		cOut.TransferLots = stored
		if err := checkStoredLots(cOut); err != nil {
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

// checkStoredLots holds a stored breakdown to what adding up means for its leg:
// pieces summing to the row's quantity and basis (portfolio.CheckTransferLots),
// or, for a spin-off's departing leg, which has no quantity, to its basis alone
// (portfolio.CheckSpinoffLots).
func checkStoredLots(op Operation) error {
	if op.Type == TypeSpinoffOut {
		return portfolio.CheckSpinoffLots(op)
	}
	return portfolio.CheckTransferLots(op)
}

// carriesOwnLots reports whether an operation's breakdown is stored next to it.
// It mirrors attachTransferLots: a transfer_out in a group reads its sibling's
// pieces and stores none; a transfer_out with no sibling (shares that left for
// another broker) stores its own. Both conversion legs store their own, since they
// describe different parcels (N old, M new).
func carriesOwnLots(op Operation) bool {
	if len(op.TransferLots) == 0 {
		return false
	}
	switch op.Type {
	case TypeTransferIn, TypeExchangeOut, TypeExchangeIn, TypeSpinoffOut, TypeSpinoffIn:
		return true
	}
	return op.TransferGroupID == nil
}

// ApplyDelta applies an importer's difference in one transaction, across any
// accounts of the space: removals, then all insertions as one batch, then verify
// on the rows as stored, then the commit.
//
// Removals go first because a corrected broker record keeps its external id,
// which the unique index would refuse while the old row exists. Every id in
// removeIDs must still be there; otherwise the difference was computed against
// another journal (ErrRemovalCountMismatch).
//
// created_at is stated by the caller, who alone knows the order it checked: rows
// sent back to back can share a clock reading, and equal same-day created_at
// leaves the fold order to the database. verify sees the stored rows, as for
// Create and CreatePair; its error is returned as is.
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

// insertBatch writes a delta's operations as one batch and returns the stored
// rows in the order given. A first broker load is thousands of rows, so one round
// trip rather than thousands. Failure handling is writeTransferLots'.
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

// storeBreakdowns writes the breakdown of every transfer in the delta that owns
// one (see carriesOwnLots) and puts the stored pieces on every row that reads
// them, the departing leg included, so verify folds what the table gave back.
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
		// Not CheckTransferLots: a spin-off's departing leg has no quantity.
		if err := checkStoredLots(stored[i]); err != nil {
			// A breakdown the storage cannot hold faithfully: rolled back, as in
			// CreatePair.
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
			// Nothing would store these pieces or read them back, and the row
			// would fold a fresh FIFO slice instead of the checked parcel.
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

// JournalFilter narrows a journal listing; a zero field narrows nothing.
type JournalFilter struct {
	Types        []Type
	InstrumentID *uuid.UUID
	From, To     *time.Time
}

// ListByAccount returns one page of the account's journal, newest first, with
// breakdowns attached so each piece can be valued at its purchase day, and
// whether anything lies beyond the page.
func (s *Store) ListByAccount(ctx context.Context, spaceID, accountID uuid.UUID, limit, offset int, f JournalFilter) ([]Operation, bool, error) {
	return s.listJournal(ctx, spaceID, &accountID, limit, offset, f)
}

// ListByInstrument is one paper's rows across every account of the space,
// newest first, a page at a time — the paper's own journal.
func (s *Store) ListByInstrument(ctx context.Context, spaceID, instrumentID uuid.UUID, limit, offset int) ([]Operation, bool, error) {
	return s.listJournal(ctx, spaceID, nil, limit, offset, JournalFilter{InstrumentID: &instrumentID})
}

// listJournal is a page of the space's rows, of one account when accountID is
// given, narrowed by f. hasMore is fetched, not inferred: one extra row is
// asked for and trimmed, because a full page cannot tell whether the journal
// continues (#86). limit must be positive; the handler refuses anything else
// first, so a bad one here is a program error.
func (s *Store) listJournal(ctx context.Context, spaceID uuid.UUID, accountID *uuid.UUID, limit, offset int, f JournalFilter) ([]Operation, bool, error) {
	if limit < 1 {
		return nil, false, fmt.Errorf("list operations: limit must be positive, got %d", limit)
	}
	types := make([]string, 0, len(f.Types))
	for _, t := range f.Types {
		types = append(types, string(t))
	}
	ops, err := s.list(ctx, `SELECT `+cols+` FROM operations
		WHERE space_id = $1 AND ($2::uuid IS NULL OR account_id = $2)
			AND (cardinality($5::text[]) = 0 OR type = ANY($5))
			AND ($6::uuid IS NULL OR instrument_id = $6)
			AND ($7::date IS NULL OR occurred_on >= $7)
			AND ($8::date IS NULL OR occurred_on <= $8)
		`+listingOrder+` LIMIT $3 OFFSET $4`,
		spaceID, accountID, limit+1, offset, types, f.InstrumentID, f.From, f.To)
	if err != nil {
		return nil, false, err
	}
	hasMore := len(ops) > limit
	if hasMore {
		ops = ops[:limit]
	}
	// After the trim: the probe row is not part of the page.
	if err := s.attachTransferLots(ctx, spaceID, ops); err != nil {
		return nil, false, err
	}
	return ops, hasMore, nil
}

// ListForEngine returns the account's whole journal in engine order, with
// breakdowns attached: they date the lots a transfer brought in.
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

// attachTransferLots fills TransferLots on ops with a separate query, so a
// many-piece operation stays one journal entry. Rows without stored pieces keep
// an empty list.
//
// Both legs of a transfer get the breakdown stored with the arriving leg: it
// describes the parcel, and the departing leg is the same parcel leaving, so the
// source journal values it at the purchase days too. Conversion legs each read
// their own: the sibling join applies to transfer_out only (see carriesOwnLots).
// Pieces are selected by the operations in hand, and every join stays within the
// caller's space.
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
		SELECT c.id, l.quantity, l.cost_minor, l.acquired_on, l.rate_on
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
		if err := rows.Scan(&id, &lot.Quantity, &lot.CostMinor, &lot.AcquiredOn, &lot.RateOn); err != nil {
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

// ListBySource returns the account's rows from one source, in engine order, with
// breakdowns attached: the importer folds them to learn what the account holds,
// and a transfer without its pieces would fold undated.
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

// ByIDs returns the space's operations with the given ids, in engine order, with
// breakdowns attached. Ids outside the space are simply absent.
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

// ByTransferGroup returns the two legs of a transfer pair, which live on two
// accounts.
func (s *Store) ByTransferGroup(ctx context.Context, spaceID, groupID uuid.UUID) ([]Operation, error) {
	return s.list(ctx, `SELECT `+cols+` FROM operations
		WHERE space_id = $1 AND transfer_group_id = $2`, spaceID, groupID)
}

// AccountsWithInstrument returns the space's accounts whose journal names the
// paper on any row, in no particular order.
func (s *Store) AccountsWithInstrument(ctx context.Context, spaceID, instrumentID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.db.Query(ctx, `SELECT DISTINCT account_id FROM operations
		WHERE space_id = $1 AND instrument_id = $2`, spaceID, instrumentID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// CounterpartAccounts returns, for each of ids that is one half of a move
// between two accounts, the account the other half is on. A pair on one
// account — a conversion, a spin-off — has no counterpart.
func (s *Store) CounterpartAccounts(ctx context.Context, spaceID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	out := make(map[uuid.UUID]uuid.UUID)
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT o.id, peer.account_id FROM operations o
		JOIN operations peer ON peer.space_id = o.space_id
			AND peer.transfer_group_id = o.transfer_group_id
			AND peer.id <> o.id AND peer.account_id <> o.account_id
		WHERE o.space_id = $1 AND o.id = ANY($2)`, spaceID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, account uuid.UUID
		if err := rows.Scan(&id, &account); err != nil {
			return nil, err
		}
		out[id] = account
	}
	return out, rows.Err()
}

// FirstDaysByInstrument is, for every paper any journal names, the day of its
// first operation — instance-wide, like the market data it is asked for.
func (s *Store) FirstDaysByInstrument(ctx context.Context) (map[uuid.UUID]time.Time, error) {
	rows, err := s.db.Query(ctx, `SELECT instrument_id, min(occurred_on) FROM operations
		WHERE instrument_id IS NOT NULL GROUP BY instrument_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]time.Time{}
	for rows.Next() {
		var (
			id  uuid.UUID
			day time.Time
		)
		if err := rows.Scan(&id, &day); err != nil {
			return nil, err
		}
		out[id] = day
	}
	return out, rows.Err()
}

// EarliestRecordedDay returns the earliest occurred_on across the instance, or
// the earliest breakdown purchase date when older: stated purchases can predate
// every operation, and their cost is converted at those days' rates. Not scoped
// to a space, as the fx backfill it feeds is shared. pgx.ErrNoRows when nothing
// is recorded.
func (s *Store) EarliestRecordedDay(ctx context.Context) (time.Time, error) {
	var on *time.Time
	err := s.db.QueryRow(ctx, `SELECT LEAST(
		(SELECT MIN(occurred_on) FROM operations),
		(SELECT MIN(acquired_on) FROM operation_transfer_lots))`).Scan(&on)
	if err != nil {
		return time.Time{}, err
	}
	if on == nil {
		return time.Time{}, pgx.ErrNoRows
	}
	return *on, nil
}

// DistinctCurrencies returns the sorted currencies of every operation in the
// instance (fx coverage is shared). A currency can appear here without an
// account in it. Empty, not an error, when there are no operations.
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
