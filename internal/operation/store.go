package operation

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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

func (s *Store) Create(ctx context.Context, spaceID uuid.UUID, op Operation) (Operation, error) {
	return insertOne(ctx, s.pool, spaceID, op)
}

// insertLotSQL writes one piece of a transfer's FIFO breakdown. seq keeps
// the pieces in the FIFO order they were released in; the table's foreign
// key removes them with the operation they describe.
const insertLotSQL = `
	INSERT INTO operation_transfer_lots (operation_id, seq, quantity, cost_minor, acquired_on)
	VALUES ($1, $2, $3, $4, $5)`

// CreatePair inserts a transfer_out/transfer_in pair atomically with a
// shared transfer_group_id, together with the FIFO breakdown carried on the
// receiving leg (in.TransferLots). All of it lands in one transaction: a
// transfer_in that lost its breakdown would silently re-date every moved lot
// to the transfer day, which is exactly the loss this records against.
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
	for i, lot := range in.TransferLots {
		if _, err := tx.Exec(ctx, insertLotSQL,
			cIn.ID, i, lot.Quantity, lot.CostMinor, lot.AcquiredOn); err != nil {
			return Operation{}, Operation{}, fmt.Errorf("transfer lot %d: %w", i, err)
		}
	}
	cIn.TransferLots = in.TransferLots
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

func (s *Store) ListByAccount(ctx context.Context, spaceID, accountID uuid.UUID, limit, offset int) ([]Operation, error) {
	return s.list(ctx, `SELECT `+cols+` FROM operations
		WHERE space_id = $1 AND account_id = $2
		ORDER BY occurred_on DESC, created_at DESC LIMIT $3 OFFSET $4`,
		spaceID, accountID, limit, offset)
}

// ListForEngine returns the account's full journal in engine order, with the
// FIFO breakdown attached to the transfers that have one. The breakdown is
// journal data the engine needs (it dates the lots a transfer brought in);
// the read paths that feed the API deliberately do not carry it.
func (s *Store) ListForEngine(ctx context.Context, spaceID, accountID uuid.UUID) ([]Operation, error) {
	ops, err := s.list(ctx, `SELECT `+cols+` FROM operations
		WHERE space_id = $1 AND account_id = $2
		ORDER BY occurred_on ASC, created_at ASC`, spaceID, accountID)
	if err != nil {
		return nil, err
	}
	if err := s.attachTransferLots(ctx, spaceID, accountID, ops); err != nil {
		return nil, err
	}
	return ops, nil
}

// attachTransferLots fills TransferLots on the account's operations. It is a
// separate query rather than a join onto the journal so that an operation
// with several pieces stays a single journal entry. Anything without stored
// pieces keeps an empty list: every non-transfer, a transfer whose basis was
// given by hand, and a transfer recorded before the breakdown was kept.
func (s *Store) attachTransferLots(ctx context.Context, spaceID, accountID uuid.UUID, ops []Operation) error {
	if len(ops) == 0 {
		return nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT l.operation_id, l.quantity, l.cost_minor, l.acquired_on
		FROM operation_transfer_lots l
		JOIN operations o ON o.id = l.operation_id
		WHERE o.space_id = $1 AND o.account_id = $2
		ORDER BY l.operation_id, l.seq`, spaceID, accountID)
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
