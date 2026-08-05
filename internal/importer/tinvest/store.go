package tinvest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// ErrConnectionNotFound means the connection a sync was asked to run for is
// not in the table. Named rather than left as a bare pgx.ErrNoRows because
// the statement that discovers it is a lock acquisition, and "there was
// nothing to lock" says something a caller acts on: the owner deleted the
// connection while the job for it was in flight.
var ErrConnectionNotFound = errors.New("tinvest: connection not found")

// ErrLinkNotInConnection means SyncMirror was handed a link that belongs to
// some other connection. Mirror rows are filed under both, so writing them
// from mismatched arguments would file one broker account's operations under
// another's connection — a fault no later read could untangle.
var ErrLinkNotInConnection = errors.New("tinvest: account link belongs to another connection")

// ErrUnparsedRowsMissing means SetUnparsedReasons named mirror rows that are
// not there. Its caller read those ids out of this very table moments before,
// and mirror rows are never deleted, so reaching this means the caller is
// working from ids that were never the mirror's — or the whole connection went
// away underneath it. Either way, marking the subset that still matches would
// leave a projection half-marked and silent about it.
var ErrUnparsedRowsMissing = errors.New("tinvest: some mirror rows named for an unparsed reason are not there")

// ConnectionStatus is the state of one space's link to the broker. It is the
// status column's CHECK constraint expressed in Go; the database refuses
// anything else.
type ConnectionStatus string

const (
	// StatusActive: the token works as far as anyone knows, and the
	// scheduler syncs this connection.
	StatusActive ConnectionStatus = "active"
	// StatusTokenRevoked: the broker rejected the token (HTTP 401 or
	// business code 40003 — see ErrTokenInvalid). Only a new token fixes it.
	StatusTokenRevoked ConnectionStatus = "token_revoked"
	// StatusDisabled: the owner switched the connection off. The mirror
	// stays exactly as it is.
	StatusDisabled ConnectionStatus = "disabled"
)

// Trigger values for a sync run: what caused it to start.
const (
	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
	TriggerInitial  = "initial"
)

// Status values for a sync run.
const (
	RunRunning = "running"
	RunOK      = "ok"
	RunFailed  = "failed"
)

// ReconcileNotChecked is the reconcile status a run carries until the
// reconciler has looked at it. "Not checked" is not "agrees" — the two are
// deliberately different values, and this is the one a run starts life with.
const ReconcileNotChecked = "not_checked"

// Connection is one space's read-only token for the broker. The token itself
// is never here in the clear: TokenCiphertext is what
// platform/secretbox.Box.Seal returned (nonce||ciphertext) and TokenLast4 is
// the tail the owner is shown so they can tell one token from another.
type Connection struct {
	ID, SpaceID     uuid.UUID
	Status          ConnectionStatus
	TokenCiphertext []byte
	TokenLast4      string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// AccountLink ties one broker account to the one babki account that mirrors
// it. BrokerAccountName/Type are what the broker called it at the time the
// link was made — a label, not something anything is looked up by.
type AccountLink struct {
	ID, ConnectionID, SpaceID, AccountID                  uuid.UUID
	BrokerAccountID, BrokerAccountName, BrokerAccountType string
	OpenedOn                                              *time.Time
}

// MirrorRow is one row of the mirror: one operation as the broker described
// it, plus what this program knows about the row itself.
//
// Payment, Price, Commission and AccruedInt are decimal, not minor units:
// the mirror stores the broker's own numbers unconverted, exactly as they
// arrived, and an amount this program cannot express in minor units still has
// to land here and become a visible unparsed row (see the plan's global
// constraints). Price, Commission and AccruedInt are pointers because their
// columns are nullable and the distinction is real: a broker that said
// nothing about commission did not say the commission was zero.
//
// Raw is the broker's element, not its bytes. The column is jsonb, so
// PostgreSQL normalizes whitespace, key order and duplicate keys on the way
// in. Nothing in this program computes anything from Raw; it is there for a
// person asking what the broker actually sent.
//
// BrokerOperationID is an ATTRIBUTE, never a key: the broker's own
// documentation says an operation's id may change over time. ContentKey is
// what identity is made of, and it is built once, from the wire (see
// contentKey), and read back from this column verbatim.
type MirrorRow struct {
	ID, ConnectionID, LinkID uuid.UUID

	BrokerOperationID string
	ParentOperationID string
	OpType            string
	State             string
	OccurredAt        time.Time

	Currency           string
	Payment            decimal.Decimal
	Price              *decimal.Decimal
	Commission         *decimal.Decimal
	CommissionCurrency string
	AccruedInt         *decimal.Decimal
	Quantity           int64

	FIGI           string
	InstrumentUID  string
	PositionUID    string
	AssetUID       string
	InstrumentType string
	Description    string
	Raw            json.RawMessage

	ContentKey      string
	FirstSeenAt     time.Time
	LastConfirmedAt time.Time
	// DisappearedAt is when the broker stopped returning this operation, or
	// nil while it still does. A mirror row is never deleted, only marked —
	// and the mark is cleared again if the operation comes back.
	DisappearedAt *time.Time
	// UnparsedReason is empty for a row the projection could turn into a
	// journal operation, and otherwise names, in a code, why it could not.
	UnparsedReason string
}

// SyncRun is one attempt to bring the mirror up to date with the broker, and
// the log the reconciler later writes its verdict onto.
type SyncRun struct {
	ID, ConnectionID, LinkID uuid.UUID
	Trigger                  string
	Status                   string
	StartedAt                time.Time
	FinishedAt               *time.Time

	ReadCount        int
	AddedCount       int
	DisappearedCount int
	UnparsedCount    int
	Error            string

	ReconcileStatus     string
	ReconciledAt        *time.Time
	ReconcileMismatches json.RawMessage
}

// RunOutcome is what a finished run has to say for itself. Status is one of
// RunOK / RunFailed; the database refuses anything else (a run cannot finish
// as "running").
type RunOutcome struct {
	Status           string
	ReadCount        int
	AddedCount       int
	DisappearedCount int
	UnparsedCount    int
	Error            string
}

// Store is the data access layer of the T-Invest importer.
//
// The reads that take a connection or a link but no space (LinksByConnection,
// MirrorRowsByLink, UnparsedByConnection, RunsByConnection,
// LastSuccessfulSyncAt, UpdateConnectionStatus, SyncMirror) are for the
// background worker, which has a job's arguments and no principal. A request
// path must establish that the connection is the caller's — ConnectionByID
// with the caller's space does exactly that — before reaching for any of them.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// connectionCols and scanConnection exist so every place that reads a
// connection reads the same columns into the same fields; see the same pair
// in internal/family/store.go for the fault that convention prevents (a
// hand-written copy that gets one column out of step fails at run time on a
// scan mismatch, not at compile time).
const connectionCols = `id, space_id, status, token_ciphertext, token_last4, created_at, updated_at`

func scanConnection(row pgx.Row) (Connection, error) {
	var c Connection
	err := row.Scan(&c.ID, &c.SpaceID, &c.Status, &c.TokenCiphertext, &c.TokenLast4,
		&c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func (s *Store) CreateConnection(ctx context.Context, spaceID uuid.UUID, tokenCiphertext []byte, tokenLast4 string) (Connection, error) {
	return scanConnection(s.pool.QueryRow(ctx, `
		INSERT INTO tinvest_connections (space_id, token_ciphertext, token_last4)
		VALUES ($1, $2, $3) RETURNING `+connectionCols, spaceID, tokenCiphertext, tokenLast4))
}

// ConnectionByID reads one connection of the caller's space. Returns
// pgx.ErrNoRows when there is no such connection IN THAT SPACE — which is
// also the answer for a connection that exists in another one, deliberately:
// a stranger learns nothing about whether the id names anything.
func (s *Store) ConnectionByID(ctx context.Context, spaceID, id uuid.UUID) (Connection, error) {
	return scanConnection(s.pool.QueryRow(ctx,
		`SELECT `+connectionCols+` FROM tinvest_connections WHERE id = $1 AND space_id = $2`, id, spaceID))
}

func (s *Store) ListConnections(ctx context.Context, spaceID uuid.UUID) ([]Connection, error) {
	return s.listConnections(ctx,
		`SELECT `+connectionCols+` FROM tinvest_connections WHERE space_id = $1 ORDER BY created_at, id`, spaceID)
}

// ListActiveConnections returns every active connection of the whole
// instance, across spaces. It is the scheduler's read: the hourly job runs
// for the instance and has no space of its own, so this one deliberately does
// not take a space and must never be reached from a request path.
func (s *Store) ListActiveConnections(ctx context.Context) ([]Connection, error) {
	return s.listConnections(ctx,
		`SELECT `+connectionCols+` FROM tinvest_connections WHERE status = $1 ORDER BY created_at, id`, StatusActive)
}

func (s *Store) listConnections(ctx context.Context, sql string, args ...any) ([]Connection, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Connection{}
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateConnectionToken replaces the stored secret and nothing else. The
// status is left exactly as it was, including token_revoked: whether the new
// token is to be trusted is a question only a successful call to the broker
// answers, and answering it here would mark a connection active on the
// strength of the owner having pasted something.
//
// Returns pgx.ErrNoRows when the connection is not the caller's.
func (s *Store) UpdateConnectionToken(ctx context.Context, spaceID, id uuid.UUID, tokenCiphertext []byte, tokenLast4 string) error {
	ct, err := s.pool.Exec(ctx, `UPDATE tinvest_connections
		SET token_ciphertext = $3, token_last4 = $4, updated_at = now()
		WHERE id = $1 AND space_id = $2`, id, spaceID, tokenCiphertext, tokenLast4)
	if err == nil && ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return err
}

// UpdateConnectionStatus is the worker's write: the sync job learns from the
// broker that a token is dead and has only the connection id to say so with.
// Space ownership is not checked here for that reason — a request path
// reaches this only after ConnectionByID has established the connection is
// the caller's.
func (s *Store) UpdateConnectionStatus(ctx context.Context, id uuid.UUID, status ConnectionStatus) error {
	ct, err := s.pool.Exec(ctx, `UPDATE tinvest_connections
		SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	if err == nil && ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return err
}

// DeleteConnection removes the owner's connection to the broker, and with it
// — by the foreign keys of migration 0014 — its links, its mirror, its
// instrument map and its run log.
//
// This is not an exception to "mirror rows are never deleted". That rule is
// about what a SYNC may do to a row while the connection lives: mark it, never
// remove it. Once the owner has withdrawn the authorization there is nothing
// left to mirror. The babki accounts the connection fed are untouched — they
// hold no foreign key back here, on purpose (see the migration's own note).
//
// Returns pgx.ErrNoRows when the connection is not the caller's.
func (s *Store) DeleteConnection(ctx context.Context, spaceID, id uuid.UUID) error {
	ct, err := s.pool.Exec(ctx,
		`DELETE FROM tinvest_connections WHERE id = $1 AND space_id = $2`, id, spaceID)
	if err == nil && ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return err
}

const linkCols = `id, connection_id, space_id, account_id, broker_account_id,
	broker_account_name, broker_account_type, opened_on`

func scanLink(row pgx.Row) (AccountLink, error) {
	var l AccountLink
	err := row.Scan(&l.ID, &l.ConnectionID, &l.SpaceID, &l.AccountID, &l.BrokerAccountID,
		&l.BrokerAccountName, &l.BrokerAccountType, &l.OpenedOn)
	return l, err
}

// CreateLink files one broker account against one babki account. link.ID is
// ignored — the table hands out its own.
func (s *Store) CreateLink(ctx context.Context, link AccountLink) (AccountLink, error) {
	return scanLink(s.pool.QueryRow(ctx, `
		INSERT INTO tinvest_account_links (connection_id, space_id, account_id,
			broker_account_id, broker_account_name, broker_account_type, opened_on)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+linkCols,
		link.ConnectionID, link.SpaceID, link.AccountID, link.BrokerAccountID,
		link.BrokerAccountName, link.BrokerAccountType, link.OpenedOn))
}

func (s *Store) LinksByConnection(ctx context.Context, connID uuid.UUID) ([]AccountLink, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+linkCols+` FROM tinvest_account_links WHERE connection_id = $1 ORDER BY created_at, id`, connID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccountLink{}
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

const mirrorCols = `id, connection_id, link_id, broker_operation_id,
	parent_operation_id, op_type, state, occurred_at, currency, payment, price,
	commission, commission_currency, accrued_int, quantity, figi,
	instrument_uid, position_uid, asset_uid, instrument_type, description, raw,
	content_key, first_seen_at, last_confirmed_at, disappeared_at, unparsed_reason`

func scanMirrorRow(row pgx.Row) (MirrorRow, error) {
	var m MirrorRow
	err := row.Scan(&m.ID, &m.ConnectionID, &m.LinkID, &m.BrokerOperationID,
		&m.ParentOperationID, &m.OpType, &m.State, &m.OccurredAt, &m.Currency,
		&m.Payment, &m.Price, &m.Commission, &m.CommissionCurrency, &m.AccruedInt,
		&m.Quantity, &m.FIGI, &m.InstrumentUID, &m.PositionUID, &m.AssetUID,
		&m.InstrumentType, &m.Description, &m.Raw, &m.ContentKey, &m.FirstSeenAt,
		&m.LastConfirmedAt, &m.DisappearedAt, &m.UnparsedReason)
	return m, err
}

func (s *Store) listMirrorRows(ctx context.Context, sql string, args ...any) ([]MirrorRow, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MirrorRow{}
	for rows.Next() {
		m, err := scanMirrorRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MirrorRowsByLink returns everything the mirror holds for one broker
// account, in the order the rows were first seen — including the rows the
// broker has stopped returning, which carry DisappearedAt to say so. The
// projection reads all of them: a row that vanished is a fact about the
// journal it produced, not something to hide.
func (s *Store) MirrorRowsByLink(ctx context.Context, linkID uuid.UUID) ([]MirrorRow, error) {
	return s.listMirrorRows(ctx,
		`SELECT `+mirrorCols+` FROM tinvest_operations_mirror
		 WHERE link_id = $1 ORDER BY first_seen_at, id`, linkID)
}

// UnparsedByConnection lists the connection's operations that the projection
// could not turn into journal entries, newest first, one page at a time.
//
// Rows the broker has since stopped returning stay on the list. They are
// still operations this program could not read, and dropping them from the
// list would be the silence this project forbids — each row carries
// DisappearedAt so the caller can say what happened to it.
//
// The second result is FETCHED, not inferred: the query asks for one row
// beyond the page and whether it arrives is the answer. Comparing the page's
// length against the limit would be right only until a caller clamps the
// limit it was given, which is how a truncated list once presented itself as
// a whole one (#86).
func (s *Store) UnparsedByConnection(ctx context.Context, connID uuid.UUID, limit, offset int) ([]MirrorRow, bool, error) {
	if limit < 1 {
		return nil, false, fmt.Errorf("tinvest: list unparsed: limit must be positive, got %d", limit)
	}
	rows, err := s.listMirrorRows(ctx,
		`SELECT `+mirrorCols+` FROM tinvest_operations_mirror
		 WHERE connection_id = $1 AND unparsed_reason <> ''
		 ORDER BY occurred_at DESC, id LIMIT $2 OFFSET $3`, connID, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	return rows, hasMore, nil
}

// SetUnparsedReasons records, for each mirror row named, why the projection
// could not read it — or clears the mark, when the reason is empty.
//
// All of them in one statement, and that statement inside a transaction that
// is thrown away unless every named row was there. Both halves are needed: one
// statement is what keeps a projection from writing half its verdict and
// stopping, and the transaction is what makes the refusal mean something —
// without it the rows that did match would keep their new reasons while the
// caller was told the write failed. See ErrUnparsedRowsMissing.
func (s *Store) SetUnparsedReasons(ctx context.Context, reasons map[uuid.UUID]string) error {
	if len(reasons) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(reasons))
	texts := make([]string, 0, len(reasons))
	for id, reason := range reasons {
		ids = append(ids, id)
		texts = append(texts, reason)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ct, err := tx.Exec(ctx, `
		UPDATE tinvest_operations_mirror m
		SET unparsed_reason = u.reason
		FROM unnest($1::uuid[], $2::text[]) AS u(id, reason)
		WHERE m.id = u.id`, ids, texts)
	if err != nil {
		return err
	}
	if int(ct.RowsAffected()) != len(reasons) {
		return fmt.Errorf("%w: %d of %d", ErrUnparsedRowsMissing, ct.RowsAffected(), len(reasons))
	}
	return tx.Commit(ctx)
}

const runCols = `id, connection_id, link_id, trigger, status, started_at,
	finished_at, read_count, added_count, disappeared_count, unparsed_count,
	error, reconcile_status, reconciled_at, reconcile_mismatches`

func scanRun(row pgx.Row) (SyncRun, error) {
	var r SyncRun
	err := row.Scan(&r.ID, &r.ConnectionID, &r.LinkID, &r.Trigger, &r.Status,
		&r.StartedAt, &r.FinishedAt, &r.ReadCount, &r.AddedCount,
		&r.DisappearedCount, &r.UnparsedCount, &r.Error, &r.ReconcileStatus,
		&r.ReconciledAt, &r.ReconcileMismatches)
	return r, err
}

// StartRun opens the log entry for one attempt, before the attempt is made.
// A run that never reaches FinishRun stays "running" for good, and that is
// the point: a crash mid-sync is visible as one, rather than as nothing
// having happened.
func (s *Store) StartRun(ctx context.Context, connID, linkID uuid.UUID, trigger string) (SyncRun, error) {
	return scanRun(s.pool.QueryRow(ctx, `
		INSERT INTO tinvest_sync_runs (connection_id, link_id, trigger, status)
		VALUES ($1, $2, $3, '`+RunRunning+`') RETURNING `+runCols, connID, linkID, trigger))
}

// FinishRun closes the log entry. The reconcile columns are left alone: what
// the reconciler found is its own write, and a sync that has not been checked
// against the broker must keep saying "not checked" rather than borrow this
// run's verdict.
//
// Returns pgx.ErrNoRows when there is no such run.
func (s *Store) FinishRun(ctx context.Context, runID uuid.UUID, outcome RunOutcome) error {
	ct, err := s.pool.Exec(ctx, `UPDATE tinvest_sync_runs
		SET status = $2, finished_at = now(), read_count = $3, added_count = $4,
		    disappeared_count = $5, unparsed_count = $6, error = $7
		WHERE id = $1`, runID, outcome.Status, outcome.ReadCount, outcome.AddedCount,
		outcome.DisappearedCount, outcome.UnparsedCount, outcome.Error)
	if err == nil && ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return err
}

// RunsByConnection returns the connection's run log, newest first, one page
// at a time. The second result is fetched rather than inferred, for the
// reason UnparsedByConnection gives.
func (s *Store) RunsByConnection(ctx context.Context, connID uuid.UUID, limit, offset int) ([]SyncRun, bool, error) {
	if limit < 1 {
		return nil, false, fmt.Errorf("tinvest: list runs: limit must be positive, got %d", limit)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+runCols+` FROM tinvest_sync_runs
		WHERE connection_id = $1 ORDER BY started_at DESC, id LIMIT $2 OFFSET $3`,
		connID, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []SyncRun{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

// LastSuccessfulSyncAt returns when the connection last synced successfully,
// or nil if it never has. There is no last_sync_at column: two independent
// computations of one value diverge eventually, so this one is derived from
// the run log (see migration 0014).
//
// It is the START of that run, deliberately, not its finish. An operation the
// broker recorded while the run was in flight can carry a timestamp earlier
// than the moment the run ended, so a next fetch bounded by the finish time
// would step over it and never come back for it. Bounded by the start, the
// worst case is re-reading operations already mirrored — which costs a
// comparison and nothing else, because the mirror is matched on content.
func (s *Store) LastSuccessfulSyncAt(ctx context.Context, connID uuid.UUID) (*time.Time, error) {
	var at time.Time
	err := s.pool.QueryRow(ctx, `SELECT started_at FROM tinvest_sync_runs
		WHERE connection_id = $1 AND status = $2
		ORDER BY started_at DESC LIMIT 1`, connID, RunOK).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &at, nil
}
