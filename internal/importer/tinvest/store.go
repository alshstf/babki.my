package tinvest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/platform/db"
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

// ErrUnparsedRowsMissing means SetUnparsedVerdicts named mirror rows that are
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
	// TriggerRegistry is a run the corporate-actions registry asked for,
	// because it changed a journal this connection is reconciled against. It is
	// a word of its own rather than one of the three above for the reason
	// migration 0025 states: none of them is true of it, and the run log's
	// trigger is a sentence a reader is shown.
	TriggerRegistry SyncTrigger = "registry"
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
	// Quantity and QuantityDone are the broker's two counts — the order and
	// the part of it that was executed. OperationItem says which belongs
	// where and why; the mirror keeps both because it keeps what the broker
	// sent.
	Quantity     int64
	QuantityDone int64
	// Ticker is what the OPERATION called the paper — and for an instrument the
	// broker has since forgotten, the ISIN, which it puts in this field for
	// exactly those. It is the last identifier such a paper has (see
	// Resolver.resolveOne and migration 0019).
	Ticker string

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
	// UnparsedDetail is what the refuser said in its own words, and it is
	// EMPTY FOR TWO DIFFERENT ROWS: one that was read successfully (there is
	// no refusal to detail) and one refused with nothing to add beyond its
	// code. UnparsedReason is what tells those apart, and it is the only field
	// of the pair anything may compute from — see UnparsedVerdict.
	UnparsedDetail string
	// ExplainedBy is the manual operation the owner entered to account for
	// this row, or nil for a row nobody has explained. It is NOT a column of
	// this table — it is attached by the queries that list rows for a person
	// to read (see attachExplanations), and left nil by the ones the
	// projection reads, which ask the explanations table its own question.
	//
	// A row with one is not unparsed: UnparsedReason is cleared when the
	// explanation is applied, which is what keeps every count of unparsed rows
	// agreeing with the list without a single one of them naming this field
	// (see projectAll).
	ExplainedBy *RowExplanation
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
//
// Reconcile is the verdict of the check against the broker, and its ZERO VALUE
// MEANS "NOT CHECKED": a caller that reconciled nothing leaves it alone and
// the run keeps saying so. See FinishRun.
type RunOutcome struct {
	Status           RunStatus
	ReadCount        int
	AddedCount       int
	DisappearedCount int
	UnparsedCount    int
	Error            string
	Reconcile        ReconcileResult
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
//     RunsByConnection, LastSuccessfulSyncAt, LastReconcileByLink, connectionForSync,
//     unparsedCountByLink. ListActiveConnections takes no space either and is a
//     wider thing still — see its own note.
//   - writes: UpdateConnectionStatus, SyncMirror, StartRun, FinishRun,
//     SetUnparsedVerdicts.
//
// The last two reads are UNEXPORTED, and that is the whole of their protection:
// they answer about any connection in the instance, so nothing outside this
// package can reach them at all and no request path can grow a caller by
// accident.
//
// SetUnparsedVerdicts is the sharpest of them and is called out on purpose: it
// takes bare mirror-row ids with no connection and no space anywhere in the
// statement, so it will mark ANY row in the table that an id names. A future
// handler that took those ids from a request body would be marking strangers'
// rows, and nothing in the SQL would stop it. Its ids must come from a read
// this program already scoped — UnparsedByConnection — and from nowhere else.
type Store struct{ db db.Executor }

func NewStore(x db.Executor) *Store { return &Store{db: x} }

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

// CreateConnection files one connection to the broker.
//
// THE STATUS IS THE CALLER'S TO STATE and is not left to the column's default,
// which is 'active'. Creating the connection is the first of several writes that
// make up a working connection — accounts, links, the first sync — and a row
// that is active from its first instant is one the hourly scheduler may pick up
// while the rest is still being written, or one that stays behind fully armed if
// the rest fails and the cleanup fails too. Service.CreateConnection therefore
// asks for StatusDisabled here and switches the connection on when everything
// else is in place; see its own doc for what each failure leaves behind.
func (s *Store) CreateConnection(ctx context.Context, spaceID uuid.UUID, tokenCiphertext []byte,
	tokenLast4 string, status ConnectionStatus,
) (Connection, error) {
	c, err := scanConnection(s.db.QueryRow(ctx, `
		INSERT INTO tinvest_connections (space_id, token_ciphertext, token_last4, status)
		VALUES ($1, $2, $3, $4) RETURNING `+connectionCols, spaceID, tokenCiphertext, tokenLast4, status))
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
	c, err := scanConnection(s.db.QueryRow(ctx,
		`SELECT `+connectionCols+` FROM tinvest_connections WHERE id = $1 AND space_id = $2`, id, spaceID))
	if err != nil {
		return Connection{}, fmt.Errorf("tinvest: read connection: %w", err)
	}
	return c, nil
}

// connectionForSync reads one connection by its id alone. It is the sync
// worker's read: a job carries a connection id and nothing else, and the worker
// has no principal whose space it could be checked against — the space is what
// this row is then read FROM (see Connection.SpaceID, which the rebuild and the
// reconciliation both scope themselves by).
//
// Unexported deliberately; see the note on Store above. It differs from
// ListActiveConnections in returning a connection WHATEVER its status, because
// "this connection is switched off" is something the worker has to be able to
// say — and to tell apart from "this connection is gone", which comes back as
// pgx.ErrNoRows.
func (s *Store) connectionForSync(ctx context.Context, id uuid.UUID) (Connection, error) {
	c, err := scanConnection(s.db.QueryRow(ctx,
		`SELECT `+connectionCols+` FROM tinvest_connections WHERE id = $1`, id))
	if err != nil {
		return Connection{}, fmt.Errorf("tinvest: read connection for sync: %w", err)
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
	rows, err := s.db.Query(ctx, sql, args...)
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
	ct, err := s.db.Exec(ctx, `UPDATE tinvest_connections
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
	ct, err := s.db.Exec(ctx, `UPDATE tinvest_connections
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
	ct, err := s.db.Exec(ctx,
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
	l, err := scanLink(s.db.QueryRow(ctx, `
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

// ConnectionsOfAccounts names the connections that reconcile any of these
// accounts, each one once, in a stable order.
//
// IT IS DELIBERATELY NOT SCOPED TO A SPACE. The caller is the corporate-actions
// registry, whose facts are instance-wide (a split happens to the paper, not to
// a household), and it hands over the accounts its own write touched — which it
// found through the journals, already knowing each one's space. Asking for a
// space here would mean either taking the caller's word for it or refusing
// accounts it has every right to name, and the ids themselves are unguessable
// primary keys rather than anything a request supplies.
//
// An account no connection feeds contributes nothing, which is the ordinary
// case for a household that keeps its own records.
func (s *Store) ConnectionsOfAccounts(ctx context.Context, accountIDs []uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT connection_id FROM tinvest_account_links
		WHERE account_id = ANY($1) ORDER BY connection_id`, accountIDs)
	if err != nil {
		return nil, fmt.Errorf("tinvest: find the connections of %d accounts: %w", len(accountIDs), err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("tinvest: find the connections of %d accounts: %w", len(accountIDs), err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) LinksByConnection(ctx context.Context, connID uuid.UUID) ([]AccountLink, error) {
	rows, err := s.db.Query(ctx,
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
	commission, commission_currency, accrued_int, quantity, quantity_done, figi,
	ticker, instrument_uid, position_uid, asset_uid, instrument_type, description, raw,
	content_key, first_seen_at, last_confirmed_at, disappeared_at, unparsed_reason,
	unparsed_detail`

func scanMirrorRow(row pgx.Row) (MirrorRow, error) {
	var m MirrorRow
	err := row.Scan(&m.ID, &m.ConnectionID, &m.LinkID, &m.BrokerOperationID,
		&m.ParentOperationID, &m.OpType, &m.State, &m.OccurredAt, &m.Currency,
		&m.Payment, &m.Price, &m.Commission, &m.CommissionCurrency, &m.AccruedInt,
		&m.Quantity, &m.QuantityDone, &m.FIGI, &m.Ticker, &m.InstrumentUID, &m.PositionUID, &m.AssetUID,
		&m.InstrumentType, &m.Description, &m.Raw, &m.ContentKey, &m.FirstSeenAt,
		&m.LastConfirmedAt, &m.DisappearedAt, &m.UnparsedReason, &m.UnparsedDetail)
	return m, err
}

func (s *Store) listMirrorRows(ctx context.Context, what, sql string, args ...any) ([]MirrorRow, error) {
	rows, err := s.db.Query(ctx, sql, args...)
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
// could not turn into journal entries, newest first, one page at a time —
// AND the rows the owner has accounted for by hand, which it could turn into
// journal entries and deliberately did not.
//
// THE SECOND KIND IS NOT UNPARSED AND IS STILL LISTED HERE, because this is
// the only screen those rows appear on: an explained row carries no reason,
// so no count of unparsed rows includes it (see projectAll, which clears the
// verdict), and leaving it off the list as well would hide the owner's own
// answer from them and leave nowhere to take it back. Each such row arrives
// with ExplainedBy filled in, which is what tells the two kinds apart —
// never the presence of a reason, which an explained row also lacks while it
// is being rebuilt.
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
		`SELECT `+mirrorCols+` FROM tinvest_operations_mirror m
		 WHERE m.connection_id = $1
		   AND (m.unparsed_reason <> ''
		        OR EXISTS (SELECT 1 FROM tinvest_mirror_explanations e
		                    WHERE e.link_id = m.link_id AND e.content_key = m.content_key))
		 ORDER BY m.occurred_at DESC, m.id LIMIT $2 OFFSET $3`, connID, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	if err := s.attachExplanations(ctx, rows); err != nil {
		return nil, false, err
	}
	return rows, hasMore, nil
}

// UnmappedHeldInstrument is a catalog row this space's journal names and this
// connection has no broker listing for.
type UnmappedHeldInstrument struct {
	InstrumentID uuid.UUID
	ISIN         string
	Ticker       string
	Type         string
	Currency     string
}

// UnmappedHeldInstruments lists the securities this space actually holds a
// history of and that this connection's instrument map says nothing about.
//
// THEY ARE THE HOLDINGS ENTERED BY HAND. The map is built from IMPORTED
// operations, so a paper the owner typed in — at a second broker, or before the
// connection existed — is never in it, and the price worker walking the map
// alone will never price it however plainly the broker lists that same paper.
// On the owner's account this is exactly two rows, and they are the only two
// holdings left with no price at all.
//
// Ordered by the catalog id so a run is reproducible, and bounded by the
// caller: this is a search per row against the broker, not a lookup.
func (s *Store) UnmappedHeldInstruments(ctx context.Context, spaceID, connID uuid.UUID) ([]UnmappedHeldInstrument, error) {
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT i.id, i.isin, i.ticker, i.type::text, i.currency
		FROM operations o
		JOIN instruments i ON i.id = o.instrument_id
		WHERE o.space_id = $1
		  AND NOT EXISTS (
			SELECT 1 FROM tinvest_instrument_map m
			WHERE m.connection_id = $2 AND m.instrument_id = i.id)
		ORDER BY i.id`, spaceID, connID)
	if err != nil {
		return nil, fmt.Errorf("tinvest: list unmapped held instruments: %w", err)
	}
	defer rows.Close()
	out := []UnmappedHeldInstrument{}
	for rows.Next() {
		var u UnmappedHeldInstrument
		if err := rows.Scan(&u.InstrumentID, &u.ISIN, &u.Ticker, &u.Type, &u.Currency); err != nil {
			return nil, fmt.Errorf("tinvest: list unmapped held instruments: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tinvest: list unmapped held instruments: %w", err)
	}
	return out, nil
}

// CurrencyTradesUnparsedByLink counts, per linked broker account, the currency
// purchases and sales this program does not import (ReasonCurrencyTrade).
//
// IT EXISTS TO EXPLAIN A DIFFERENCE THAT CANNOT CLOSE. Every such operation is
// money that left one currency and arrived in another, and the journal records
// neither half — so the account's cash comparison against the broker is off by
// their whole sum, in both currencies, and stays off however many times it is
// re-run. A reconciliation that reports that difference beside the securities
// ones, with nothing to tell them apart, teaches a reader to ignore the lot.
//
// It speaks for the MONEY side only. A currency trade moves no securities, so a
// difference in units of a share has nothing to do with this figure and must not
// be captioned by it.
//
// Counted at read time rather than stored on the run: it is a property of the
// mirror as it stands, and a number frozen into a run row would go on claiming
// an old count after a rebuild changed it.
func (s *Store) CurrencyTradesUnparsedByLink(ctx context.Context, connID uuid.UUID) (map[uuid.UUID]int, error) {
	rows, err := s.db.Query(ctx, `
		SELECT link_id, count(*) FROM tinvest_operations_mirror
		WHERE connection_id = $1 AND unparsed_reason = $2
		GROUP BY link_id`, connID, string(ReasonCurrencyTrade))
	if err != nil {
		return nil, fmt.Errorf("tinvest: count unparsed currency trades: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]int{}
	for rows.Next() {
		var linkID uuid.UUID
		var n int
		if err := rows.Scan(&linkID, &n); err != nil {
			return nil, fmt.Errorf("tinvest: count unparsed currency trades: %w", err)
		}
		out[linkID] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tinvest: count unparsed currency trades: %w", err)
	}
	return out, nil
}

// unparsedCountByLink counts the mirror rows of ONE broker account that the
// projection could not read. The sync worker writes it onto that link's run.
//
// It exists because the rebuild's own Unparsed figure is for the whole
// CONNECTION while a run belongs to one link, and a connection-wide number
// filed under one account would be read as that account's own by every screen
// that shows it — the caption that does not match the figure beside it, which
// this project has been bitten by four times.
//
// Unexported deliberately; see the note on Store above.
func (s *Store) unparsedCountByLink(ctx context.Context, linkID uuid.UUID) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM tinvest_operations_mirror
		WHERE link_id = $1 AND unparsed_reason <> ''`, linkID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("tinvest: count the unparsed rows of a link: %w", err)
	}
	return n, nil
}

// UnparsedVerdict is what one pass over the mirror decided about one row: the
// CODE that says which refusal it was, and the refuser's own words about this
// particular row.
//
// The two are not one field on purpose. Reason is a closed set — every value
// is declared in the contract, and the interface chooses the sentence the
// owner reads from it and from nothing else, so it is the half that may be
// computed from. Detail is prose from whatever refused (the journal's
// validation, the engine replaying an account, the resolver failing to match a
// security): it names the security, the quantity, the figure that made this row
// impossible, and it exists for the person reading ONE row. Nothing may branch
// on its wording, and no caption may be picked out of it.
//
// The zero value is the verdict for a row that was read: no code, and nothing
// to detail. A refused row always carries a Reason and MAY carry an empty
// Detail — plenty of codes are the whole story by themselves.
type UnparsedVerdict struct {
	Reason string
	Detail string
}

// SetUnparsedVerdicts records, for each mirror row named, why the projection
// could not read it and what the refuser said — or clears the mark, when the
// verdict is the zero value.
//
// All of them in one statement, and that statement inside a transaction that
// is thrown away unless every named row was there. Both halves are needed: one
// statement is what keeps a projection from writing half its verdict and
// stopping, and the transaction is what makes the refusal mean something —
// without it the rows that did match would keep their new reasons while the
// caller was told the write failed. See ErrUnparsedRowsMissing.
//
// The two columns move TOGETHER, in one assignment, and that is the whole
// guarantee against the fault this project keeps meeting: a detail left over
// from a refusal that no longer applies, sitting under a code that now says
// something else, is a caption disagreeing with the figure beside it.
func (s *Store) SetUnparsedVerdicts(ctx context.Context, verdicts map[uuid.UUID]UnparsedVerdict) error {
	if len(verdicts) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(verdicts))
	reasons := make([]string, 0, len(verdicts))
	details := make([]string, 0, len(verdicts))
	for id, v := range verdicts {
		ids = append(ids, id)
		reasons = append(reasons, v.Reason)
		details = append(details, v.Detail)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("tinvest: set unparsed verdicts: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ct, err := tx.Exec(ctx, `
		UPDATE tinvest_operations_mirror m
		SET unparsed_reason = u.reason, unparsed_detail = u.detail
		FROM unnest($1::uuid[], $2::text[], $3::text[]) AS u(id, reason, detail)
		WHERE m.id = u.id`, ids, reasons, details)
	if err != nil {
		return fmt.Errorf("tinvest: set unparsed verdicts: %w", err)
	}
	if int(ct.RowsAffected()) != len(verdicts) {
		return fmt.Errorf("%w: %d of %d", ErrUnparsedRowsMissing, ct.RowsAffected(), len(verdicts))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("tinvest: set unparsed verdicts: %w", err)
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
	r, err := scanRun(s.db.QueryRow(ctx, `
		INSERT INTO tinvest_sync_runs (connection_id, link_id, trigger, status)
		VALUES ($1, $2, $3, $4) RETURNING `+runCols, connID, linkID, trigger, RunRunning))
	if err != nil {
		return SyncRun{}, fmt.Errorf("tinvest: start sync run: %w", err)
	}
	return r, nil
}

// FinishRun closes the log entry, carrying the reconciliation's verdict onto
// it.
//
// A RUN NOBODY CHECKED KEEPS SAYING SO. The zero value of RunOutcome.Reconcile
// is "not checked" (its Status is the empty string, which the column's own
// CHECK would refuse), and it is written out as not_checked with a null
// reconciled_at and a null list: "never looked" is a different statement from
// "looked and found nothing", which is an EMPTY list. A screen that cannot
// tell those apart draws a tick over a check that never happened, and this
// program's whole reconciliation exists not to.
//
// reconciled_at is the database's own clock at this statement, the same
// instant as finished_at: the check was made within this run, moments before
// it was closed.
//
// Returns pgx.ErrNoRows when there is no such run, and
// ErrReconcileVerdictContradictsItself when the verdict and the list disagree.
func (s *Store) FinishRun(ctx context.Context, runID uuid.UUID, outcome RunOutcome) error {
	status, mismatches, err := reconcileColumns(outcome.Reconcile)
	if err != nil {
		return err
	}
	ct, err := s.db.Exec(ctx, `UPDATE tinvest_sync_runs
		SET status = $2, finished_at = now(), read_count = $3, added_count = $4,
		    disappeared_count = $5, unparsed_count = $6, error = $7,
		    reconcile_status = $8,
		    reconciled_at = CASE WHEN $8 = $9 THEN NULL ELSE now() END,
		    reconcile_mismatches = $10
		WHERE id = $1`, runID, outcome.Status, outcome.ReadCount, outcome.AddedCount,
		outcome.DisappearedCount, outcome.UnparsedCount, outcome.Error,
		status, ReconcileNotChecked, mismatches)
	if err != nil {
		return fmt.Errorf("tinvest: finish sync run: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("tinvest: finish sync run: %w", pgx.ErrNoRows)
	}
	return nil
}

// ErrReconcileVerdictContradictsItself means a run was asked to record a
// verdict its own list of differences denies: "there are differences" with
// nothing to show, or any other verdict — "everything agrees", "not checked" —
// while carrying some.
//
// The column's CHECK constrains the word alone, and this is the pairing. It is
// refused rather than repaired because either half could be the true one, and
// a screen showing the wrong half is precisely the failure — a caption that
// does not match the figures beside it — this project has been bitten by four
// times.
var ErrReconcileVerdictContradictsItself = errors.New("tinvest: the reconcile verdict and its list of differences disagree")

// reconcileColumns turns a verdict into the two values the run log stores: the
// status word, and the differences as jsonb (null when nothing was checked).
func reconcileColumns(rec ReconcileResult) (ReconcileStatus, []byte, error) {
	status := rec.Status
	if status == "" {
		status = ReconcileNotChecked
	}
	if (status == ReconcileMismatched) != (len(rec.Mismatches) > 0) {
		return "", nil, fmt.Errorf("%w: %q with %d of them",
			ErrReconcileVerdictContradictsItself, status, len(rec.Mismatches))
	}
	if status == ReconcileNotChecked {
		return status, nil, nil
	}
	// Never nil: an agreement is an EMPTY list of differences, and json's null
	// would say the same as a run nobody checked.
	list := rec.Mismatches
	if list == nil {
		list = []ReconcileMismatch{}
	}
	encoded, err := json.Marshal(list)
	if err != nil {
		return "", nil, fmt.Errorf("tinvest: finish sync run: encode the differences found: %w", err)
	}
	return status, encoded, nil
}

// RunsByConnection returns the connection's run log, newest first, one page
// at a time. The second result is fetched rather than inferred, for the
// reason UnparsedByConnection gives.
func (s *Store) RunsByConnection(ctx context.Context, connID uuid.UUID, limit, offset int) ([]SyncRun, bool, error) {
	if limit < 1 {
		return nil, false, fmt.Errorf("tinvest: list runs: limit must be positive, got %d", limit)
	}
	rows, err := s.db.Query(ctx, `SELECT `+runCols+` FROM tinvest_sync_runs
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
	err := s.db.QueryRow(ctx, `SELECT started_at FROM tinvest_sync_runs
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

// LastReconcileByLink returns, for each of the connection's links, the most
// recent run OF THAT LINK which actually made a check against the broker. A
// link nothing ever reconciled is absent from the map, and that absence is the
// caller's "not checked".
//
// A VERDICT BELONGS TO ONE ACCOUNT. A run is made for the pair (connection,
// link) and the reconciliation happens inside it, so what it found is a
// statement about one broker account and about no other. A single verdict for
// the whole connection — the connection's newest reconciled run, whichever
// account it was for — is therefore not a fact about the connection at all: two
// accounts checked in one sync, the differing one first and the agreeing one a
// moment later, would publish agreement, and the differing account's verdict,
// being older, would never appear.
//
// IT IS NOT "THE NEWEST RUN'S VERDICT" within a link either. Runs that fail —
// and runs whose own side the engine refused to compute — finish at
// ReconcileNotChecked, and reading the newest run of the link alone would let
// one of those erase what a check made an hour earlier had found. "We checked
// and found nothing" would then be published as "nobody has checked", which is
// precisely the pair this program's three-valued verdict exists to keep apart.
//
// Like every other read in this list it takes no space and checks none; see the
// note on Store. A request path reaches it only after ConnectionByID has
// established the connection is the caller's.
func (s *Store) LastReconcileByLink(ctx context.Context, connID uuid.UUID) (map[uuid.UUID]SyncRun, error) {
	rows, err := s.db.Query(ctx, `SELECT DISTINCT ON (link_id) `+runCols+`
		FROM tinvest_sync_runs
		WHERE connection_id = $1 AND reconcile_status <> $2
		ORDER BY link_id, reconciled_at DESC, id`, connID, ReconcileNotChecked)
	if err != nil {
		return nil, fmt.Errorf("tinvest: read last reconcile per account: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]SyncRun{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("tinvest: read last reconcile per account: %w", err)
		}
		out[r.LinkID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tinvest: read last reconcile per account: %w", err)
	}
	return out, nil
}

// mapMatch is one hit of the connection's instrument map, joined against the
// catalog's own type/currency/isin/ticker columns (see
// (*Store).mapByInstrumentUID). The join exists so a hit answers Resolve fully
// — id, type, the money the paper is denominated in, and what Resolve's own
// final write needs — without a second round trip through instrumentCatalog:
// the entire point of checking this table before the broker's passport is to
// skip that call, and paying for a catalog read here would give back half the
// saving.
type mapMatch struct {
	InstrumentID uuid.UUID
	Type         instrument.Type
	Currency     string
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
	err := s.db.QueryRow(ctx, `
		SELECT im.instrument_id, i.type, i.currency, i.isin, i.ticker
		FROM tinvest_instrument_map im
		JOIN instruments i ON i.id = im.instrument_id
		WHERE im.connection_id = $1 AND im.instrument_uid = $2`,
		connectionID, instrumentUID).Scan(&m.InstrumentID, &m.Type, &m.Currency, &m.ISIN, &m.Ticker)
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
	err := s.db.QueryRow(ctx, `
		SELECT im.instrument_id, i.type, i.currency, i.isin, i.ticker
		FROM tinvest_instrument_map im
		JOIN instruments i ON i.id = im.instrument_id
		WHERE im.connection_id = $1 AND im.figi = $2
		ORDER BY im.updated_at DESC
		LIMIT 1`,
		connectionID, figi).Scan(&m.InstrumentID, &m.Type, &m.Currency, &m.ISIN, &m.Ticker)
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
//
// listingCurrency is what the broker's own passport says THIS LISTING is
// denominated in, and it is treated like the three identifiers rather than like
// isin/ticker: an empty one leaves whatever the row already holds. A resolution
// that hit the map has no passport in hand and passes the empty string, which
// must not blank a currency an earlier call learned.
func (s *Store) saveMap(ctx context.Context, connectionID, instrumentID uuid.UUID, ref InstrumentRef, isin, ticker, listingCurrency string) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO tinvest_instrument_map
			(connection_id, instrument_id, figi, instrument_uid, position_uid, asset_uid, isin, ticker, currency)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (connection_id, instrument_uid) DO UPDATE SET
			instrument_id = EXCLUDED.instrument_id,
			figi          = COALESCE(NULLIF(EXCLUDED.figi, ''), tinvest_instrument_map.figi),
			position_uid  = COALESCE(NULLIF(EXCLUDED.position_uid, ''), tinvest_instrument_map.position_uid),
			asset_uid     = COALESCE(NULLIF(EXCLUDED.asset_uid, ''), tinvest_instrument_map.asset_uid),
			isin          = EXCLUDED.isin,
			ticker        = EXCLUDED.ticker,
			currency      = COALESCE(NULLIF(EXCLUDED.currency, ''), tinvest_instrument_map.currency),
			updated_at    = now()
		WHERE tinvest_instrument_map.instrument_id <> EXCLUDED.instrument_id
		   OR (EXCLUDED.figi         <> '' AND tinvest_instrument_map.figi         <> EXCLUDED.figi)
		   OR (EXCLUDED.position_uid <> '' AND tinvest_instrument_map.position_uid <> EXCLUDED.position_uid)
		   OR (EXCLUDED.asset_uid    <> '' AND tinvest_instrument_map.asset_uid    <> EXCLUDED.asset_uid)
		   OR (EXCLUDED.currency     <> '' AND tinvest_instrument_map.currency     <> EXCLUDED.currency)
		   OR tinvest_instrument_map.isin   <> EXCLUDED.isin
		   OR tinvest_instrument_map.ticker <> EXCLUDED.ticker`,
		connectionID, instrumentID, ref.FIGI, ref.InstrumentUID, ref.PositionUID, ref.AssetUID, isin, ticker,
		upperCurrency(listingCurrency))
	if err != nil {
		return fmt.Errorf("tinvest: save instrument map: %w", err)
	}
	return nil
}

// QuotableInstrument is one broker listing this connection has mapped to a
// catalog row, with everything needed to store a price for it: which catalog
// instrument it stands for, which of the broker's identifiers to ask under, and
// what the listing is denominated in.
type QuotableInstrument struct {
	InstrumentUID string
	InstrumentID  uuid.UUID
	// Currency is empty for a mapping written before the currency was
	// recorded (migration 0017). Such a listing is not priced until something
	// fills it in — see (*Store).SetMapCurrency.
	Currency string
}

// QuotableByConnection is every listing this connection could ask a price for,
// ordered by the broker's identifier so a run is reproducible.
//
// One row per (connection, instrument_uid), which is the table's own
// uniqueness, so the SAME catalog instrument legitimately appears several times
// — the broker's identifiers drift, and a paper delisted from one venue and
// quoted on another carries two of them. Both are asked and both are stored;
// which one ends up as the latest quote is decided by the day each price
// belongs to, the same rule that already picks between two refreshes.
func (s *Store) QuotableByConnection(ctx context.Context, connectionID uuid.UUID) ([]QuotableInstrument, error) {
	rows, err := s.db.Query(ctx, `
		SELECT instrument_uid, instrument_id, currency
		FROM tinvest_instrument_map
		WHERE connection_id = $1 AND instrument_uid <> ''
		ORDER BY instrument_uid`, connectionID)
	if err != nil {
		return nil, fmt.Errorf("tinvest: list quotable instruments: %w", err)
	}
	defer rows.Close()
	out := []QuotableInstrument{}
	for rows.Next() {
		var q QuotableInstrument
		if err := rows.Scan(&q.InstrumentUID, &q.InstrumentID, &q.Currency); err != nil {
			return nil, fmt.Errorf("tinvest: list quotable instruments: %w", err)
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tinvest: list quotable instruments: %w", err)
	}
	return out, nil
}

// SetMapCurrency records what a listing is denominated in, for a mapping
// written before that was kept. It is how the rows migration 0017 left empty
// are filled: the quotes worker asks the broker's passport once per such
// listing and remembers the answer, so the next run costs nothing.
func (s *Store) SetMapCurrency(ctx context.Context, connectionID uuid.UUID, instrumentUID, currency string) error {
	if currency == "" {
		return fmt.Errorf("tinvest: set map currency: refusing to record an empty currency for %s", instrumentUID)
	}
	_, err := s.db.Exec(ctx, `
		UPDATE tinvest_instrument_map SET currency = $3, updated_at = now()
		WHERE connection_id = $1 AND instrument_uid = $2`,
		connectionID, instrumentUID, upperCurrency(currency))
	if err != nil {
		return fmt.Errorf("tinvest: set map currency: %w", err)
	}
	return nil
}

// instrumentMap is everything this connection has already learned about the
// broker's instruments, in the two shapes the reconciliation needs: an
// InstrumentIndex saying which catalog instrument each of the broker's
// identifiers stands for, and a label per catalog instrument for saying WHICH
// security a difference is about.
//
// BOTH IDENTIFIERS ARE READ, not the instrument_uid alone, because the
// broker's identifiers drift: the resolver looks its own map up by
// instrument_uid and then by figi for exactly that reason (see
// (*Resolver).lookupMap), and a reconciliation that knew only the first would
// report a drifted position as two differences that are both false. See
// InstrumentIndex.
//
// It is one read of the whole table rather than a lookup per position, because
// the caller compares every position of the account at once and the two
// per-row lookups above would be that many round trips.
//
// The label is the catalog's ticker, or its name when it has no ticker (the
// ticker column is optional and empty for anything that never traded under
// one). Chosen here rather than in SQL so that the one place it is decided is
// readable next to what it is for.
//
// AN EMPTY IDENTIFIER IS NOT INDEXED — neither an empty instrument_uid nor an
// empty figi, though the row's label is kept either way. saveMap's only caller
// never writes a row without an instrument_uid (see Resolve's guard) but does
// write rows without a figi, and a "" key would answer for every broker
// position that arrived without that identifier — resolving them all to one
// instrument. mapByInstrumentUID and mapByFIGI above refuse the same key for
// the same reason.
//
// A FIGI TWO ROWS DISAGREE ABOUT ANSWERS FOR NEITHER, and this is the one
// place where this index deliberately says less than mapByFIGI would. Two
// rows carrying one figi is what a drifted instrument_uid leaves behind, and
// they normally name the same instrument, which is no disagreement at all.
// When they do not, mapByFIGI still returns one (`ORDER BY updated_at DESC
// LIMIT 1`) because a resolution has to end somewhere, while what this index
// answers goes straight onto a screen as "the broker's 100 against your 0" —
// so a wrong confident match here is worse than none, and none means the
// position is shown as a difference, which is what "we could not match this"
// is supposed to look like. Keeping whichever row arrived last would be worse
// than both: rows come back in no particular order, so the answer would
// depend on how the database felt like returning them, and one run would
// match a position where the next reported it, with nothing changed between.
func (s *Store) instrumentMap(ctx context.Context, connectionID uuid.UUID) (InstrumentIndex, map[uuid.UUID]string, error) {
	fail := func(err error) (InstrumentIndex, map[uuid.UUID]string, error) {
		return InstrumentIndex{}, nil, fmt.Errorf("tinvest: read instrument map: %w", err)
	}

	rows, err := s.db.Query(ctx, `
		SELECT im.instrument_uid, im.figi, im.instrument_id, i.ticker, i.name
		FROM tinvest_instrument_map im
		JOIN instruments i ON i.id = im.instrument_id
		WHERE im.connection_id = $1`, connectionID)
	if err != nil {
		return fail(err)
	}
	defer rows.Close()

	index := InstrumentIndex{ByUID: map[string]uuid.UUID{}, ByFIGI: map[string]uuid.UUID{}}
	contested := map[string]bool{}
	labels := map[uuid.UUID]string{}
	for rows.Next() {
		var (
			uid, figi    string
			id           uuid.UUID
			ticker, name string
		)
		if err := rows.Scan(&uid, &figi, &id, &ticker, &name); err != nil {
			return fail(err)
		}
		if uid != "" {
			index.ByUID[uid] = id
		}
		if figi != "" && !contested[figi] {
			if seen, ok := index.ByFIGI[figi]; ok && seen != id {
				delete(index.ByFIGI, figi)
				contested[figi] = true
			} else {
				index.ByFIGI[figi] = id
			}
		}
		if ticker != "" {
			labels[id] = ticker
		} else {
			labels[id] = name
		}
	}
	if err := rows.Err(); err != nil {
		return fail(err)
	}
	return index, labels, nil
}
