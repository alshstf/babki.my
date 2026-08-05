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

	"babki.my/babki/internal/instrument"
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

// ErrLinkOutsideSpace means CreateLink was asked to file a connection or a
// babki account that is not in the space the link names. The same fault
// SyncMirror refuses with ErrLinkNotInConnection, one step earlier: a link
// written across spaces would put one household's broker operations into
// another household's account, and no later read could tell that it happened.
var ErrLinkOutsideSpace = errors.New("tinvest: the connection or the account is not in that space")

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

// SyncTrigger is what caused a sync run to start. Like ConnectionStatus it is
// a named type and not a bare string, so that a misspelling is a compile
// error here rather than a CHECK constraint violation at run time, from
// inside a worker, on the one run that used it.
type SyncTrigger string

const (
	TriggerSchedule SyncTrigger = "schedule"
	TriggerManual   SyncTrigger = "manual"
	TriggerInitial  SyncTrigger = "initial"
)

// RunStatus is where one sync run stands. Named for the reason SyncTrigger
// is.
type RunStatus string

const (
	RunRunning RunStatus = "running"
	RunOK      RunStatus = "ok"
	RunFailed  RunStatus = "failed"
)

// ReconcileStatus is the reconciler's verdict on a run. Named for the reason
// SyncTrigger is; the values beyond the one below belong to the reconciler
// and arrive with it.
type ReconcileStatus string

// ReconcileNotChecked is the reconcile status a run carries until the
// reconciler has looked at it. "Not checked" is not "agrees" — the two are
// deliberately different values, and this is the one a run starts life with.
const ReconcileNotChecked ReconcileStatus = "not_checked"

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

// MirrorRow is one row of the mirror: one operation as the broker describes
// it NOW, plus what this program knows about the row itself.
//
// A ROW CARRIES THE LATEST OBSERVATION, NOT THE FIRST. Every attribute the
// broker sends is rewritten on each sync that finds the operation still
// there. What survives a refresh untouched is ID and where the row is filed,
// FirstSeenAt, UnparsedReason, and ContentKey together with the fields it was
// built from — which cannot change without the operation becoming a different
// one. See SyncMirror and mirrorConfirmSQL.
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
// person asking what the broker actually sent — which is why it is refreshed
// along with everything else rather than frozen at the first sighting. A Raw
// left at the first document while BrokerOperationID beside it followed the
// broker would be two answers to one question.
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
	Trigger                  SyncTrigger
	Status                   RunStatus
	StartedAt                time.Time
	FinishedAt               *time.Time

	ReadCount        int
	AddedCount       int
	DisappearedCount int
	UnparsedCount    int
	Error            string

	ReconcileStatus     ReconcileStatus
	ReconciledAt        *time.Time
	ReconcileMismatches json.RawMessage
}

// RunOutcome is what a finished run has to say for itself. Status is one of
// RunOK / RunFailed; the database refuses anything else (a run cannot finish
// as "running").
type RunOutcome struct {
	Status           RunStatus
	ReadCount        int
	AddedCount       int
	DisappearedCount int
	UnparsedCount    int
	Error            string
}

// Store is the data access layer of the T-Invest importer.
//
// SOME METHODS HERE TAKE NO SPACE AND CHECK NONE. They are the background
// worker's: it runs from a job's arguments and has no principal to check
// against. A request path must establish that the connection is the caller's
// — ConnectionByID with the caller's space does exactly that — before
// reaching any of them. The full list, so that a reader can tell it apart
// from the rest by looking rather than by remembering:
//
//   - reads: LinksByConnection, MirrorRowsByLink, UnparsedByConnection,
//     RunsByConnection, LastSuccessfulSyncAt. ListActiveConnections takes no
//     space either and is a wider thing still — see its own note.
//   - writes: UpdateConnectionStatus, SyncMirror, StartRun, FinishRun,
//     SetUnparsedReasons.
//
// SetUnparsedReasons is the sharpest of them and is called out on purpose: it
// takes bare mirror-row ids with no connection and no space anywhere in the
// statement, so it will mark ANY row in the table that an id names. A future
// handler that took those ids from a request body would be marking strangers'
// rows, and nothing in the SQL would stop it. Its ids must come from a read
// this program already scoped — UnparsedByConnection — and from nowhere else.
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

// EVERY ERROR THIS PACKAGE RETURNS CARRIES ITS NAME. An error coming off the
// driver is wrapped with the package and the operation that failed —
// "tinvest: read connection: ..." — and the named sentinels above spell
// "tinvest:" out themselves. This is a rule and not a habit because the
// package had both kinds side by side, and a bare driver error says nothing
// about where it came from once it has been passed up two layers.
//
// Wrapping with %w leaves errors.Is working, so callers looking for
// pgx.ErrNoRows still find it. The "Returns pgx.ErrNoRows" notes below mean
// it in that sense: the error IS one, it is no longer the bare one.

func (s *Store) CreateConnection(ctx context.Context, spaceID uuid.UUID, tokenCiphertext []byte, tokenLast4 string) (Connection, error) {
	c, err := scanConnection(s.pool.QueryRow(ctx, `
		INSERT INTO tinvest_connections (space_id, token_ciphertext, token_last4)
		VALUES ($1, $2, $3) RETURNING `+connectionCols, spaceID, tokenCiphertext, tokenLast4))
	if err != nil {
		return Connection{}, fmt.Errorf("tinvest: create connection: %w", err)
	}
	return c, nil
}

// ConnectionByID reads one connection of the caller's space. Returns
// pgx.ErrNoRows when there is no such connection IN THAT SPACE — which is
// also the answer for a connection that exists in another one, deliberately:
// a stranger learns nothing about whether the id names anything.
func (s *Store) ConnectionByID(ctx context.Context, spaceID, id uuid.UUID) (Connection, error) {
	c, err := scanConnection(s.pool.QueryRow(ctx,
		`SELECT `+connectionCols+` FROM tinvest_connections WHERE id = $1 AND space_id = $2`, id, spaceID))
	if err != nil {
		return Connection{}, fmt.Errorf("tinvest: read connection: %w", err)
	}
	return c, nil
}

func (s *Store) ListConnections(ctx context.Context, spaceID uuid.UUID) ([]Connection, error) {
	return s.listConnections(ctx, "list connections",
		`SELECT `+connectionCols+` FROM tinvest_connections WHERE space_id = $1 ORDER BY created_at, id`, spaceID)
}

// ListActiveConnections returns every active connection of the whole
// instance, across spaces. It is the scheduler's read: the hourly job runs
// for the instance and has no space of its own, so this one deliberately does
// not take a space and must never be reached from a request path.
func (s *Store) ListActiveConnections(ctx context.Context) ([]Connection, error) {
	return s.listConnections(ctx, "list active connections",
		`SELECT `+connectionCols+` FROM tinvest_connections WHERE status = $1 ORDER BY created_at, id`, StatusActive)
}

func (s *Store) listConnections(ctx context.Context, what, sql string, args ...any) ([]Connection, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("tinvest: %s: %w", what, err)
	}
	defer rows.Close()
	out := []Connection{}
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, fmt.Errorf("tinvest: %s: %w", what, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tinvest: %s: %w", what, err)
	}
	return out, nil
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
	if err != nil {
		return fmt.Errorf("tinvest: replace connection token: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("tinvest: replace connection token: %w", pgx.ErrNoRows)
	}
	return nil
}

// UpdateConnectionStatus is the worker's write: the sync job learns from the
// broker that a token is dead and has only the connection id to say so with.
// Space ownership is not checked here for that reason — a request path
// reaches this only after ConnectionByID has established the connection is
// the caller's.
func (s *Store) UpdateConnectionStatus(ctx context.Context, id uuid.UUID, status ConnectionStatus) error {
	ct, err := s.pool.Exec(ctx, `UPDATE tinvest_connections
		SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	if err != nil {
		return fmt.Errorf("tinvest: set connection status: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("tinvest: set connection status: %w", pgx.ErrNoRows)
	}
	return nil
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
	if err != nil {
		return fmt.Errorf("tinvest: delete connection: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("tinvest: delete connection: %w", pgx.ErrNoRows)
	}
	return nil
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
//
// THE SPACE IS CHECKED HERE, on both of the other two things the link names:
// the connection has to be in link.SpaceID and so does the account. The
// foreign keys cannot do it — each of the three columns points at a valid row
// on its own, and nothing in the schema says the three have to agree — so a
// caller that mixed up two spaces would write a link that files one
// household's broker operations into another household's account. Returns
// ErrLinkOutsideSpace when they do not agree; SyncMirror refuses the same
// class of mismatch with ErrLinkNotInConnection one step later.
//
// It is one statement rather than a read followed by a write on purpose: two
// statements would leave a window in which the connection is deleted between
// the check and the insert, and the insert would then fail on the foreign key
// with an error that says something else entirely.
func (s *Store) CreateLink(ctx context.Context, link AccountLink) (AccountLink, error) {
	l, err := scanLink(s.pool.QueryRow(ctx, `
		INSERT INTO tinvest_account_links (connection_id, space_id, account_id,
			broker_account_id, broker_account_name, broker_account_type, opened_on)
		SELECT $1::uuid, $2::uuid, $3::uuid, $4::text, $5::text, $6::text, $7::date
		WHERE EXISTS (SELECT 1 FROM tinvest_connections WHERE id = $1 AND space_id = $2)
		  AND EXISTS (SELECT 1 FROM accounts WHERE id = $3 AND space_id = $2)
		RETURNING `+linkCols,
		link.ConnectionID, link.SpaceID, link.AccountID, link.BrokerAccountID,
		link.BrokerAccountName, link.BrokerAccountType, link.OpenedOn))
	if errors.Is(err, pgx.ErrNoRows) {
		// The insert wrote nothing, and the only thing that can stop it
		// writing is the WHERE above.
		return AccountLink{}, fmt.Errorf("%w: connection %s, account %s, space %s",
			ErrLinkOutsideSpace, link.ConnectionID, link.AccountID, link.SpaceID)
	}
	if err != nil {
		return AccountLink{}, fmt.Errorf("tinvest: create account link: %w", err)
	}
	return l, nil
}

func (s *Store) LinksByConnection(ctx context.Context, connID uuid.UUID) ([]AccountLink, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+linkCols+` FROM tinvest_account_links WHERE connection_id = $1 ORDER BY created_at, id`, connID)
	if err != nil {
		return nil, fmt.Errorf("tinvest: list account links: %w", err)
	}
	defer rows.Close()
	out := []AccountLink{}
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, fmt.Errorf("tinvest: list account links: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tinvest: list account links: %w", err)
	}
	return out, nil
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

func (s *Store) listMirrorRows(ctx context.Context, what, sql string, args ...any) ([]MirrorRow, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("tinvest: %s: %w", what, err)
	}
	defer rows.Close()
	out := []MirrorRow{}
	for rows.Next() {
		m, err := scanMirrorRow(rows)
		if err != nil {
			return nil, fmt.Errorf("tinvest: %s: %w", what, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tinvest: %s: %w", what, err)
	}
	return out, nil
}

// MirrorRowsByLink returns everything the mirror holds for one broker
// account, in the order the rows were first seen — including the rows the
// broker has stopped returning, which carry DisappearedAt to say so. The
// projection reads all of them: a row that vanished is a fact about the
// journal it produced, not something to hide.
func (s *Store) MirrorRowsByLink(ctx context.Context, linkID uuid.UUID) ([]MirrorRow, error) {
	return s.listMirrorRows(ctx, "list mirror rows",
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
	rows, err := s.listMirrorRows(ctx, "list unparsed",
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
		return fmt.Errorf("tinvest: set unparsed reasons: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ct, err := tx.Exec(ctx, `
		UPDATE tinvest_operations_mirror m
		SET unparsed_reason = u.reason
		FROM unnest($1::uuid[], $2::text[]) AS u(id, reason)
		WHERE m.id = u.id`, ids, texts)
	if err != nil {
		return fmt.Errorf("tinvest: set unparsed reasons: %w", err)
	}
	if int(ct.RowsAffected()) != len(reasons) {
		return fmt.Errorf("%w: %d of %d", ErrUnparsedRowsMissing, ct.RowsAffected(), len(reasons))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("tinvest: set unparsed reasons: %w", err)
	}
	return nil
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
func (s *Store) StartRun(ctx context.Context, connID, linkID uuid.UUID, trigger SyncTrigger) (SyncRun, error) {
	r, err := scanRun(s.pool.QueryRow(ctx, `
		INSERT INTO tinvest_sync_runs (connection_id, link_id, trigger, status)
		VALUES ($1, $2, $3, $4) RETURNING `+runCols, connID, linkID, trigger, RunRunning))
	if err != nil {
		return SyncRun{}, fmt.Errorf("tinvest: start sync run: %w", err)
	}
	return r, nil
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
	if err != nil {
		return fmt.Errorf("tinvest: finish sync run: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("tinvest: finish sync run: %w", pgx.ErrNoRows)
	}
	return nil
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
		return nil, false, fmt.Errorf("tinvest: list sync runs: %w", err)
	}
	defer rows.Close()
	out := []SyncRun{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, false, fmt.Errorf("tinvest: list sync runs: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("tinvest: list sync runs: %w", err)
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

// LastSuccessfulSyncAt returns the moment the connection's last successful
// run STARTED, or nil if it never had one. There is no last_sync_at column:
// two independent computations of one value diverge eventually, so this one
// is derived from the run log (see migration 0014).
//
// IT IS FOR SHOWING THE OWNER WHEN THE IMPORT LAST WORKED, and for nothing
// else. Two things about it have to be said plainly, because both are easy to
// assume the other way round:
//
//   - It is the run's START, not its finish. A caller reading it as "the
//     mirror is up to date as of this instant" would be reading it wrong by
//     however long that run took.
//   - It is keyed by CONNECTION, while runs are made per (connection, link).
//     For a connection with several broker accounts it therefore means "at
//     least one of them synced successfully at that moment", never "all of
//     them did". A link whose every run has failed leaves no trace here.
//
// In particular it is NOT a lower bound for the next fetch of history.
// SyncMirror compares against the FULL history of the link and marks
// everything it does not find as disappeared, so a fetch bounded by this
// value would mark the whole history before the bound as gone on the very
// first run. See the precondition on SyncMirror.
func (s *Store) LastSuccessfulSyncAt(ctx context.Context, connID uuid.UUID) (*time.Time, error) {
	var at time.Time
	err := s.pool.QueryRow(ctx, `SELECT started_at FROM tinvest_sync_runs
		WHERE connection_id = $1 AND status = $2
		ORDER BY started_at DESC LIMIT 1`, connID, RunOK).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("tinvest: read last successful sync: %w", err)
	}
	return &at, nil
}

// mapMatch is one hit of the connection's instrument map, joined against the
// catalog's own type/isin/ticker columns (see (*Store).mapByInstrumentUID).
// The join exists so a hit answers Resolve fully — id, type, and what
// Resolve's own final write needs — without a second round trip through
// instrumentCatalog: the entire point of checking this table before the
// broker's passport is to skip that call, and paying for a catalog read here
// would give back half the saving.
type mapMatch struct {
	InstrumentID uuid.UUID
	Type         instrument.Type
	ISIN, Ticker string
}

// mapByInstrumentUID and mapByFIGI are the resolver's two lookups into
// tinvest_instrument_map, tried in that order by lookupMap. Both return
// pgx.ErrNoRows on a miss (this package's usual wrapping — see the note
// above scanConnection in store.go — leaves errors.Is working).
//
// A row can never dangle: instrument_id references instruments(id) ON
// DELETE CASCADE (migration 0014), so a match here is always joinable.
func (s *Store) mapByInstrumentUID(ctx context.Context, connectionID uuid.UUID, instrumentUID string) (mapMatch, error) {
	if instrumentUID == "" {
		// An empty instrument_uid is not something the table's own
		// uniqueness (connection_id, instrument_uid) could ever narrow to
		// one row on purpose — see Resolve's own guard on writing one.
		// Refusing before the query keeps that from silently matching
		// whatever row a previous empty-uid write happened to leave.
		return mapMatch{}, fmt.Errorf("tinvest: instrument map by instrument_uid: %w", pgx.ErrNoRows)
	}
	var m mapMatch
	err := s.pool.QueryRow(ctx, `
		SELECT im.instrument_id, i.type, i.isin, i.ticker
		FROM tinvest_instrument_map im
		JOIN instruments i ON i.id = im.instrument_id
		WHERE im.connection_id = $1 AND im.instrument_uid = $2`,
		connectionID, instrumentUID).Scan(&m.InstrumentID, &m.Type, &m.ISIN, &m.Ticker)
	if err != nil {
		return mapMatch{}, fmt.Errorf("tinvest: instrument map by instrument_uid: %w", err)
	}
	return m, nil
}

// mapByFIGI is lookupMap's fallback for when the operation's instrument_uid
// is not one this connection recognizes: an operation that predates
// instrument_uid, or one where it has drifted since the last sync, may still
// carry a figi this connection has already resolved.
//
// The index behind this query (tinvest_map_figi_idx) is not unique, so a
// connection can hold more than one row for one figi — most plausibly an
// older row an instrument_uid has since drifted away from. The most
// recently updated one is taken, on the reasoning that a later write is the
// more likely one to still be right about what this figi means today.
func (s *Store) mapByFIGI(ctx context.Context, connectionID uuid.UUID, figi string) (mapMatch, error) {
	if figi == "" {
		// Every row this connection has never set a figi for shares the
		// empty string; picking "most recently updated" among them would
		// return an unrelated instrument, not "no match" — see the same
		// guard on mapByInstrumentUID above.
		return mapMatch{}, fmt.Errorf("tinvest: instrument map by figi: %w", pgx.ErrNoRows)
	}
	var m mapMatch
	err := s.pool.QueryRow(ctx, `
		SELECT im.instrument_id, i.type, i.isin, i.ticker
		FROM tinvest_instrument_map im
		JOIN instruments i ON i.id = im.instrument_id
		WHERE im.connection_id = $1 AND im.figi = $2
		ORDER BY im.updated_at DESC
		LIMIT 1`,
		connectionID, figi).Scan(&m.InstrumentID, &m.Type, &m.ISIN, &m.Ticker)
	if err != nil {
		return mapMatch{}, fmt.Errorf("tinvest: instrument map by figi: %w", err)
	}
	return m, nil
}

// saveMap records instrumentID as the answer for ref.InstrumentUID, along
// with every other identifier ref carries and the catalog's own isin/ticker
// for it (ON CONFLICT (connection_id, instrument_uid), migration 0014's own
// uniqueness). It is called on every resolution Resolve completes, a map hit
// as much as a freshly created row, so that a drift in
// figi/position_uid/asset_uid ALONE — with instrument_uid unchanged, and so
// still hitting mapByInstrumentUID on its own — is still captured rather
// than left on a row nothing ever revisits again.
//
// AN EMPTY IDENTIFIER NEVER ERASES A STORED ONE, and this is the reason the
// three broker identifiers are written through COALESCE(NULLIF(...)) instead
// of being assigned. They are read off ONE OPERATION (see InstrumentRef),
// while the row is what this connection has learned across ALL of them, so a
// write that assigns makes the row weaker than the sum of what was observed —
// and what it would erase first is the figi, which is the entire fallback that
// lets a resolution survive an instrument_uid drifting (see mapByFIGI). The
// mechanism built for the day an identifier changes would be destroyed by an
// ordinary operation that merely failed to mention one.
//
// WHAT DOES NOT SUPPORT THIS, checked rather than repeated: this package's own
// operation fixtures do not contain the "dividend without a figi" that the
// review reported. Their dividend carries figi, positionUid and assetUid like
// the trade does, and the one operation there with an empty figi is a broker
// fee, which carries no instrument_uid either and so never reaches this
// statement at all (see Resolve's guard). What the rule stands on instead is
// the shape of the data — four independent strings, any of which can arrive
// empty, as that same broker fee shows — and on what this row is for: it
// accumulates what the connection has learned, and a write that can only be
// built from one operation must not be allowed to subtract from it.
//
// isin and ticker ARE assigned outright, and the difference is where they come
// from: they are the catalog's own columns as of this resolution, not
// something one operation happened to mention, so an empty one is a true
// statement about the catalog row rather than a gap in what the broker sent.
//
// NOTHING IS WRITTEN WHEN NOTHING CHANGED (the WHERE below). A full history
// resolves the same instrument on every one of its operations, and an
// unconditional DO UPDATE made every one of those a row write plus a fresh
// updated_at, saying exactly what the row already said — one write per
// operation for as long as the history is. Every column here is NOT NULL
// (migration 0014), so plain
// <> is enough and no three-valued surprise hides in it; the three broker
// identifiers are compared only when the operation actually carries them, for
// the same reason they are not assigned when it does not.
//
// Callers must not call this with ref.InstrumentUID == "" — see Resolve's
// own guard, which is the only caller and never does.
func (s *Store) saveMap(ctx context.Context, connectionID, instrumentID uuid.UUID, ref InstrumentRef, isin, ticker string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO tinvest_instrument_map
			(connection_id, instrument_id, figi, instrument_uid, position_uid, asset_uid, isin, ticker)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (connection_id, instrument_uid) DO UPDATE SET
			instrument_id = EXCLUDED.instrument_id,
			figi          = COALESCE(NULLIF(EXCLUDED.figi, ''), tinvest_instrument_map.figi),
			position_uid  = COALESCE(NULLIF(EXCLUDED.position_uid, ''), tinvest_instrument_map.position_uid),
			asset_uid     = COALESCE(NULLIF(EXCLUDED.asset_uid, ''), tinvest_instrument_map.asset_uid),
			isin          = EXCLUDED.isin,
			ticker        = EXCLUDED.ticker,
			updated_at    = now()
		WHERE tinvest_instrument_map.instrument_id <> EXCLUDED.instrument_id
		   OR (EXCLUDED.figi         <> '' AND tinvest_instrument_map.figi         <> EXCLUDED.figi)
		   OR (EXCLUDED.position_uid <> '' AND tinvest_instrument_map.position_uid <> EXCLUDED.position_uid)
		   OR (EXCLUDED.asset_uid    <> '' AND tinvest_instrument_map.asset_uid    <> EXCLUDED.asset_uid)
		   OR tinvest_instrument_map.isin   <> EXCLUDED.isin
		   OR tinvest_instrument_map.ticker <> EXCLUDED.ticker`,
		connectionID, instrumentID, ref.FIGI, ref.InstrumentUID, ref.PositionUID, ref.AssetUID, isin, ticker)
	if err != nil {
		return fmt.Errorf("tinvest: save instrument map: %w", err)
	}
	return nil
}
