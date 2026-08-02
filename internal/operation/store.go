package operation

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"babki.my/babki/internal/portfolio"
)

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

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
const insertSQL = `
	INSERT INTO operations (space_id, account_id, instrument_id, type,
		occurred_on, settled_on, quantity, price, amount_minor, currency,
		fee_minor, note, transfer_group_id, split_ratio, source, external_id)
	SELECT a.space_id, a.id, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
		COALESCE(NULLIF($15, ''), 'manual'), $16
	FROM accounts a WHERE a.id = $2 AND a.space_id = $1
	RETURNING ` + cols

func insertOne(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, spaceID uuid.UUID, op Operation,
) (Operation, error) {
	created, err := scan(q.QueryRow(ctx, insertSQL,
		spaceID, op.AccountID, op.InstrumentID, op.Type, op.OccurredOn,
		op.SettledOn, op.Quantity, op.Price, op.AmountMinor, op.Currency,
		op.FeeMinor, op.Note, op.TransferGroupID, op.SplitRatio,
		op.Source, op.ExternalID))
	if err == pgx.ErrNoRows {
		return Operation{}, fmt.Errorf("account not found in space: %w", pgx.ErrNoRows)
	}
	return created, err
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
		return insertOne(ctx, s.pool, spaceID, op)
	}
	tx, err := s.pool.Begin(ctx)
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
const insertLotSQL = `
	INSERT INTO operation_transfer_lots (operation_id, seq, quantity, cost_minor, acquired_on)
	VALUES ($1, $2, $3, $4, $5)
	RETURNING quantity, cost_minor, acquired_on`

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
func (s *Store) CreatePair(ctx context.Context, spaceID uuid.UUID, out, in Operation) (Operation, Operation, error) {
	tx, err := s.pool.Begin(ctx)
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
		stored := make([]ReleasedLot, 0, len(in.TransferLots))
		for i, lot := range in.TransferLots {
			var back ReleasedLot
			if err := tx.QueryRow(ctx, insertLotSQL,
				cIn.ID, i, lot.Quantity, lot.CostMinor, lot.AcquiredOn).
				Scan(&back.Quantity, &back.CostMinor, &back.AcquiredOn); err != nil {
				return Operation{}, Operation{}, fmt.Errorf("transfer lot %d: %w", i, err)
			}
			stored = append(stored, back)
		}
		cIn.TransferLots = stored
		// The departing leg gets them too. The rows are stored next to the
		// arriving leg only, but they describe THE PARCEL, and the pair is one
		// parcel with the opposite sign — which is exactly what
		// attachTransferLots decided for every later read (see its doc). Handing
		// back an out leg with an empty breakdown made the pair contradict itself
		// within a single response: whether a transfer knows when its shares were
		// bought is published per operation (Operation.has_undated_lots, see the
		// API contract), and the departing leg would have answered "no" about a
		// parcel whose dates are sitting in the same transaction.
		cOut.TransferLots = stored
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
	return cOut, cIn, tx.Commit(ctx)
}

func (s *Store) list(ctx context.Context, sql string, args ...any) ([]Operation, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
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
// the FIFO breakdown attached to the transfers that have one. The journal
// listing needs it for the same reason the engine does: a transfer's amount is
// a basis assembled from purchases on several days, and expressing it in the
// space's base currency means converting each piece at the rate of the day it
// was bought (see Handler.operationInBase). Without the pieces the row would
// have to be converted at the rate of the day the shares changed brokers,
// which is exactly the misvaluation this whole mechanism exists to prevent —
// and the journal would print a different number than the position screen for
// the same shares.
func (s *Store) ListByAccount(ctx context.Context, spaceID, accountID uuid.UUID, limit, offset int) ([]Operation, error) {
	ops, err := s.list(ctx, `SELECT `+cols+` FROM operations
		WHERE space_id = $1 AND account_id = $2
		ORDER BY occurred_on DESC, created_at DESC LIMIT $3 OFFSET $4`,
		spaceID, accountID, limit, offset)
	if err != nil {
		return nil, err
	}
	if err := s.attachTransferLots(ctx, spaceID, ops); err != nil {
		return nil, err
	}
	return ops, nil
}

// ListForEngine returns the account's full journal in engine order, with the
// FIFO breakdown attached to the transfers that have one. The breakdown is
// journal data the engine needs: it dates the lots a transfer brought in.
func (s *Store) ListForEngine(ctx context.Context, spaceID, accountID uuid.UUID) ([]Operation, error) {
	ops, err := s.list(ctx, `SELECT `+cols+` FROM operations
		WHERE space_id = $1 AND account_id = $2
		ORDER BY occurred_on ASC, created_at ASC`, spaceID, accountID)
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
// pieces keeps an empty list: every non-transfer, a transfer whose basis was
// given by hand, and a transfer recorded before the breakdown was kept.
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
	rows, err := s.pool.Query(ctx, `
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

func (s *Store) ByID(ctx context.Context, spaceID, id uuid.UUID) (Operation, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM operations
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
	err := s.pool.QueryRow(ctx, `SELECT MIN(occurred_on) FROM operations`).Scan(&on)
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
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT currency FROM operations ORDER BY currency`)
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
	ct, err := s.pool.Exec(ctx, `
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
