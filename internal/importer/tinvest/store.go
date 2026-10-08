package tinvest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/db"
)

// ErrConnectionNotFound means a sync's connection is gone: the owner deleted
// it while its job was in flight. Named because it comes from a lock
// acquisition, where "nothing to lock" means exactly that.
var ErrConnectionNotFound = errors.New("tinvest: connection not found")

// ErrLinkNotInConnection means SyncMirror got a link of another connection;
// writing would file one broker account's operations under another's.
var ErrLinkNotInConnection = errors.New("tinvest: account link belongs to another connection")

// ErrUnparsedRowsMissing means SetUnparsedVerdicts named rows that are not
// there. The caller read those ids from this table and rows are never deleted, so
// the ids were never the mirror's or the connection went away; marking the rest
// would leave a half-marked projection.
var ErrUnparsedRowsMissing = errors.New("tinvest: some mirror rows named for an unparsed reason are not there")

// ErrLinkOutsideSpace means CreateLink got a connection or account outside the
// link's space, which would put one household's broker operations into another's
// account.
var ErrLinkOutsideSpace = errors.New("tinvest: the connection or the account is not in that space")

// ConnectionStatus is the status column's CHECK in Go.
type ConnectionStatus string

const (
	// StatusActive: the scheduler syncs this connection.
	StatusActive ConnectionStatus = "active"
	// StatusTokenRevoked: the broker rejected the token (see
	// ErrTokenInvalid); only a new token fixes it.
	StatusTokenRevoked ConnectionStatus = "token_revoked"
	// StatusDisabled: switched off by the owner; the mirror stays as is.
	StatusDisabled ConnectionStatus = "disabled"
)

// SyncTrigger is what started a run; a named type so a misspelling fails
// at compile time, not at the CHECK.
type SyncTrigger string

const (
	TriggerSchedule SyncTrigger = "schedule"
	TriggerManual   SyncTrigger = "manual"
	TriggerInitial  SyncTrigger = "initial"
	// TriggerRegistry: the corporate-actions registry changed a journal this
	// connection reconciles (migration 0025).
	TriggerRegistry SyncTrigger = "registry"
)

// RunStatus is where a run stands.
type RunStatus string

const (
	RunRunning RunStatus = "running"
	RunOK      RunStatus = "ok"
	RunFailed  RunStatus = "failed"
)

// ReconcileStatus is the reconciler's verdict on a run.
type ReconcileStatus string

// ReconcileNotChecked is a run's status until the reconciler looks at it;
// not the same as agreeing.
const ReconcileNotChecked ReconcileStatus = "not_checked"

// Connection is one space's read-only broker token: TokenCiphertext is
// secretbox.Box.Seal's output, TokenLast4 the tail the owner is shown.
type Connection struct {
	ID, SpaceID     uuid.UUID
	Status          ConnectionStatus
	TokenCiphertext []byte
	TokenLast4      string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// AccountLink ties one broker account to the babki account that mirrors it.
// BrokerAccountName and Type are labels from when the link was made.
type AccountLink struct {
	ID, ConnectionID, SpaceID, AccountID                  uuid.UUID
	BrokerAccountID, BrokerAccountName, BrokerAccountType string
	OpenedOn                                              *time.Time
}

// MirrorRow is one operation as the broker describes it now, plus what this
// program knows about the row.
//
// Every broker attribute is rewritten by each sync that still finds the operation;
// ID, filing, FirstSeenAt, UnparsedReason and ContentKey with its inputs stay (see
// SyncMirror and mirrorConfirmSQL).
//
// Payment, Price, Commission and AccruedInt are the broker's decimals, unconverted,
// so an amount that does not fit minor units still lands here and becomes a
// visible unparsed row. The nullable ones are pointers: no commission said is not
// zero commission.
//
// Raw is the broker's element as jsonb (normalized by Postgres), refreshed with
// the rest, for a person asking what the broker sent; nothing computes from it.
//
// BrokerOperationID is an attribute, never a key: the broker says it may change.
// Identity is ContentKey, built once from the wire (see contentKey).
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
	// The broker's two counts, order and fill; the mirror keeps both (see
	// OperationItem).
	Quantity     int64
	QuantityDone int64
	// Ticker is what the operation called the paper; for one the broker has
	// forgotten, its ISIN (see Resolver.resolveOne, migration 0019).
	Ticker string
	// ClassCode is the broker's classCode as sent, empty when absent; naming
	// it is tradingMode's job.
	ClassCode string

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
	// DisappearedAt is when the broker stopped returning this operation, nil
	// while it still does. Rows are marked, never deleted, and unmarked if the
	// operation returns.
	DisappearedAt *time.Time
	// UnparsedReason is empty for a row the projection read and otherwise a
	// code saying why not.
	UnparsedReason string
	// UnparsedDetail is the refuser's own words. Empty both for a read row
	// and for a refusal with nothing to add; only UnparsedReason may be
	// computed from (see UnparsedVerdict).
	UnparsedDetail string
	// ExplainedBy is the manual operation the owner entered for this row, or
	// nil. Not a column: attached by the listing queries (attachExplanations).
	// An explained row has its UnparsedReason cleared (see projectAll), so
	// counts agree with the list.
	ExplainedBy *RowExplanation
}

// SyncRun is one attempt to refresh the mirror, and the log the reconciler
// writes its verdict onto.
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

// RunOutcome is a finished run's result. Status is RunOK or RunFailed. A zero
// Reconcile means not checked (see FinishRun).
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
// Some methods take no space and check none: they serve the background worker,
// which has a job's arguments and no principal. A request path must establish
// the connection is the caller's (ConnectionByID with the caller's space) first.
//
//   - reads: LinksByConnection, MirrorRowsByLink, UnparsedByConnection,
//     RunsByConnection, LastSuccessfulSyncAt, LastReconcileByLink,
//     connectionForSync, unparsedCountByLink; ListActiveConnections is
//     instance-wide.
//   - writes: UpdateConnectionStatus, SyncMirror, StartRun, FinishRun,
//     SetUnparsedVerdicts.
//
// SetUnparsedVerdicts takes bare row ids and marks any row they name; its ids
// must come from UnparsedByConnection and never from a request.
type Store struct{ db db.Executor }

func NewStore(x db.Executor) *Store { return &Store{db: x} }

// The catalog, the journal and the accounts belong to other modules: this store
// reads them through their stores (plan item 2.3), over its own executor, so a
// read inside a transaction stays in it.
func (s *Store) catalog() *instrument.Store { return instrument.NewStore(s.db) }
func (s *Store) journal() *operation.Store  { return operation.NewStore(s.db) }
func (s *Store) accounts() *account.Store   { return account.NewStore(s.db) }

// catalogOf reads the catalog rows behind ids, failing as what.
func (s *Store) catalogOf(ctx context.Context, what string, ids []uuid.UUID) (map[uuid.UUID]instrument.Instrument, error) {
	found, err := s.catalog().ByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("tinvest: %s: read the catalog: %w", what, err)
	}
	return found, nil
}

// byID orders ids as Postgres orders uuids, byte by byte, so a list built in Go
// keeps the order its query once gave it.
func byID(ids []uuid.UUID) []uuid.UUID {
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return ids
}

// connectionCols and scanConnection keep every connection read on the same
// columns.
const connectionCols = `id, space_id, status, token_ciphertext, token_last4, created_at, updated_at`

func scanConnection(row pgx.Row) (Connection, error) {
	var c Connection
	err := row.Scan(&c.ID, &c.SpaceID, &c.Status, &c.TokenCiphertext, &c.TokenLast4,
		&c.CreatedAt, &c.UpdatedAt)
	return c, err
}

// Every error this package returns names it: driver errors are wrapped as
// "tinvest: <operation>: ..." with %w, so errors.Is still finds pgx.ErrNoRows
// where a doc says it is returned.

// CreateConnection files one connection. The caller states the status: the
// column defaults to active, and Service.CreateConnection creates it disabled
// until its accounts and links exist.
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

// ConnectionByID reads one connection of the caller's space; pgx.ErrNoRows
// also for one in another space, so a stranger learns nothing.
func (s *Store) ConnectionByID(ctx context.Context, spaceID, id uuid.UUID) (Connection, error) {
	c, err := scanConnection(s.db.QueryRow(ctx,
		`SELECT `+connectionCols+` FROM tinvest_connections WHERE id = $1 AND space_id = $2`, id, spaceID))
	if err != nil {
		return Connection{}, fmt.Errorf("tinvest: read connection: %w", err)
	}
	return c, nil
}

// connectionForSync reads a connection by id alone, for the sync worker, whose
// job carries only the id; the space is read from the row. It returns any status,
// so the worker can tell "switched off" from "gone" (pgx.ErrNoRows).
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

// ListActiveConnections returns every active connection of the instance, for
// the hourly scheduler. Never from a request path.
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

// UpdateConnectionToken replaces the secret and nothing else; whether the new
// token works is for a call to the broker to say, not for the owner's paste.
// pgx.ErrNoRows when not the caller's.
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

// UpdateConnectionStatus is the worker's write when the broker reports a dead
// token; it checks no space (see Store).
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

// DeleteConnection removes the connection and, by the foreign keys of migration
// 0014, its links, mirror, instrument map and run log. "Mirror rows are never
// deleted" is about syncs; with the authorization withdrawn nothing is left to
// mirror. The babki accounts stay. pgx.ErrNoRows when not the caller's.
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

// CreateLink files one broker account against one babki account; link.ID is
// ignored. The connection and the account must both be in link.SpaceID, which the
// foreign keys cannot check: ErrLinkOutsideSpace otherwise.
//
// The account is asked of its own store first. That is as good as asking in the
// insert: an account never changes space and is never deleted on its own, only
// with its space, which takes the connection too. The connection is checked in
// the insert, so one deleted in between cannot surface as a confusing
// foreign-key error.
func (s *Store) CreateLink(ctx context.Context, link AccountLink) (AccountLink, error) {
	outside := fmt.Errorf("%w: connection %s, account %s, space %s",
		ErrLinkOutsideSpace, link.ConnectionID, link.AccountID, link.SpaceID)
	if _, err := s.accounts().ByID(ctx, link.SpaceID, link.AccountID); errors.Is(err, pgx.ErrNoRows) {
		return AccountLink{}, outside
	} else if err != nil {
		return AccountLink{}, fmt.Errorf("tinvest: create account link: read the account: %w", err)
	}
	l, err := scanLink(s.db.QueryRow(ctx, `
		INSERT INTO tinvest_account_links (connection_id, space_id, account_id,
			broker_account_id, broker_account_name, broker_account_type, opened_on)
		SELECT $1::uuid, $2::uuid, $3::uuid, $4::text, $5::text, $6::text, $7::date
		WHERE EXISTS (SELECT 1 FROM tinvest_connections WHERE id = $1 AND space_id = $2)
		RETURNING `+linkCols,
		link.ConnectionID, link.SpaceID, link.AccountID, link.BrokerAccountID,
		link.BrokerAccountName, link.BrokerAccountType, link.OpenedOn))
	if errors.Is(err, pgx.ErrNoRows) {
		// Only the WHERE above can stop the insert.
		return AccountLink{}, outside
	}
	if err != nil {
		return AccountLink{}, fmt.Errorf("tinvest: create account link: %w", err)
	}
	return l, nil
}

// ConnectionsOfAccounts names each connection reconciling any of these
// accounts once, in stable order. Not space-scoped: the caller is the
// corporate-actions registry, whose facts are instance-wide, passing accounts it
// found through the journals.
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
	ticker, class_code, instrument_uid, position_uid, asset_uid, instrument_type, description, raw,
	content_key, first_seen_at, last_confirmed_at, disappeared_at, unparsed_reason,
	unparsed_detail`

func scanMirrorRow(row pgx.Row) (MirrorRow, error) {
	var m MirrorRow
	err := row.Scan(&m.ID, &m.ConnectionID, &m.LinkID, &m.BrokerOperationID,
		&m.ParentOperationID, &m.OpType, &m.State, &m.OccurredAt, &m.Currency,
		&m.Payment, &m.Price, &m.Commission, &m.CommissionCurrency, &m.AccruedInt,
		&m.Quantity, &m.QuantityDone, &m.FIGI, &m.Ticker, &m.ClassCode,
		&m.InstrumentUID, &m.PositionUID, &m.AssetUID,
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

// MirrorRowsByLink returns everything mirrored for one broker account, in
// first-seen order, including disappeared rows: the projection reads them all.
func (s *Store) MirrorRowsByLink(ctx context.Context, linkID uuid.UUID) ([]MirrorRow, error) {
	return s.listMirrorRows(ctx, "list mirror rows",
		`SELECT `+mirrorCols+` FROM tinvest_operations_mirror
		 WHERE link_id = $1 ORDER BY first_seen_at, id`, linkID)
}

// UnparsedByConnection lists, newest first and a page at a time, the
// connection's rows the projection could not read and the rows the owner
// explained by hand (ExplainedBy set): those carry no reason, and this is the only
// screen where the owner can see or take back the explanation. Disappeared rows
// stay, with DisappearedAt. hasMore is fetched, not inferred (#86).
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

// sealer opens a token with any known key and seals it with the current one.
type sealer interface {
	Open(sealed []byte) ([]byte, error)
	Seal(plaintext []byte) []byte
}

// ResealTokens re-encrypts every token with the current key so an old key can
// be dropped; all or none. Returns how many.
func (s *Store) ResealTokens(ctx context.Context, box sealer) (int, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT id, token_ciphertext FROM tinvest_connections FOR UPDATE`)
	if err != nil {
		return 0, fmt.Errorf("tinvest: read tokens: %w", err)
	}
	type sealed struct {
		id    uuid.UUID
		token []byte
	}
	var all []sealed
	for rows.Next() {
		var c sealed
		if err := rows.Scan(&c.id, &c.token); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, c := range all {
		plain, err := box.Open(c.token)
		if err != nil {
			return 0, fmt.Errorf("tinvest: connection %s: no key opens its token: %w", c.id, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE tinvest_connections SET token_ciphertext = $2 WHERE id = $1`,
			c.id, box.Seal(plain)); err != nil {
			return 0, fmt.Errorf("tinvest: reseal connection %s: %w", c.id, err)
		}
	}
	return len(all), tx.Commit(ctx)
}

// UnmappedHeldInstrument is a catalog row this space's journal names and this
// connection has no listing for.
type UnmappedHeldInstrument struct {
	InstrumentID uuid.UUID
	ISIN         string
	Ticker       string
	Type         string
	Currency     string
}

// UnmappedHeldInstruments lists papers this space has history in that this
// connection's instrument map does not know: holdings entered by hand, since the
// map is built from imported operations. On the owner's account these are the
// only two holdings left unpriced. Ordered by catalog id; the caller bounds it,
// since each row is a broker search.
func (s *Store) UnmappedHeldInstruments(ctx context.Context, spaceID, connID uuid.UUID) ([]UnmappedHeldInstrument, error) {
	const what = "list unmapped held instruments"
	held, err := s.journal().FirstDaysInSpace(ctx, spaceID)
	if err != nil {
		return nil, fmt.Errorf("tinvest: %s: read the journal: %w", what, err)
	}
	mapped, err := s.mappedInstruments(ctx, connID)
	if err != nil {
		return nil, fmt.Errorf("tinvest: %s: %w", what, err)
	}
	var ids []uuid.UUID
	for id := range held {
		if !mapped[id] {
			ids = append(ids, id)
		}
	}
	papers, err := s.catalogOf(ctx, what, ids)
	if err != nil {
		return nil, err
	}
	out := []UnmappedHeldInstrument{}
	for _, id := range byID(ids) {
		if i, ok := papers[id]; ok {
			out = append(out, UnmappedHeldInstrument{
				InstrumentID: id, ISIN: i.ISIN, Ticker: i.Ticker, Type: string(i.Type), Currency: i.Currency,
			})
		}
	}
	return out, nil
}

// mappedInstruments is every catalog instrument this connection's map knows.
func (s *Store) mappedInstruments(ctx context.Context, connID uuid.UUID) (map[uuid.UUID]bool, error) {
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT instrument_id FROM tinvest_instrument_map WHERE connection_id = $1`, connID)
	if err != nil {
		return nil, fmt.Errorf("read the instrument map: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("read the instrument map: %w", err)
	}
	out := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// CurrencyTradesUnparsedByLink counts, per link, the currency trades this
// program does not import (ReasonCurrencyTrade). Each leaves the cash comparison
// off by its sum in both currencies for good, so the reconciliation shows it
// beside the money differences (not the securities ones). Counted at read time,
// since a rebuild can change it.
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

// unparsedCountByLink counts one broker account's unreadable rows for its run.
// The rebuild's own figure is per connection, and a connection-wide number filed
// under one account would be read as that account's.
func (s *Store) unparsedCountByLink(ctx context.Context, linkID uuid.UUID) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM tinvest_operations_mirror
		WHERE link_id = $1 AND unparsed_reason <> ''`, linkID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("tinvest: count the unparsed rows of a link: %w", err)
	}
	return n, nil
}

// UnparsedVerdict is one pass's decision about one row: Reason, the closed code
// the interface chooses its sentence from, and Detail, the refuser's prose for a
// person reading the row, never branched on. The zero value is a row that was
// read; a refusal always has a Reason and may have no Detail.
type UnparsedVerdict struct {
	Reason string
	Detail string
}

// SetUnparsedVerdicts records each named row's verdict, or clears it for the
// zero value, in one statement inside a transaction discarded unless every row
// was there (ErrUnparsedRowsMissing). Reason and Detail move together, so a stale
// detail cannot sit under a new code.
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

// StartRun opens the log entry before the attempt; a crash leaves it "running"
// for good, which is visible.
func (s *Store) StartRun(ctx context.Context, connID, linkID uuid.UUID, trigger SyncTrigger) (SyncRun, error) {
	r, err := scanRun(s.db.QueryRow(ctx, `
		INSERT INTO tinvest_sync_runs (connection_id, link_id, trigger, status)
		VALUES ($1, $2, $3, $4) RETURNING `+runCols, connID, linkID, trigger, RunRunning))
	if err != nil {
		return SyncRun{}, fmt.Errorf("tinvest: start sync run: %w", err)
	}
	return r, nil
}

// FinishRun closes the log entry with the reconciliation's verdict. A zero
// verdict is written as not_checked with null reconciled_at and a null list:
// "never looked" differs from "looked and found nothing", an empty list.
// reconciled_at is the statement's clock, as finished_at. pgx.ErrNoRows for no
// such run; ErrReconcileVerdictContradictsItself when verdict and list disagree.
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

// ErrReconcileVerdictContradictsItself means a verdict its own list denies:
// differences with nothing listed, or any other verdict with differences listed.
// Refused rather than repaired: either half could be the true one.
var ErrReconcileVerdictContradictsItself = errors.New("tinvest: the reconcile verdict and its list of differences disagree")

// reconcileColumns turns a verdict into the stored status word and jsonb
// list (null when nothing was checked).
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
	// Never nil: agreement is an empty list, null means unchecked.
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

// RunsByConnection returns the run log, newest first, a page at a time;
// hasMore is fetched.
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

// LastSuccessfulSyncAt returns when the connection's last successful run
// started, or nil, derived from the run log (migration 0014). It is for showing
// the owner and nothing else: a start, not a finish, and per connection, so with
// several links it means at least one synced. Never a lower bound for fetching
// history: SyncMirror marks everything not fetched as disappeared.
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

// LastReconcileByLink returns, per link, the newest run of that link that
// actually checked against the broker; an absent link is "not checked". Per link
// because a verdict is about one account: a connection-wide newest verdict would
// let an agreeing account hide a differing one. Not simply the newest run either:
// failed and uncomputed runs finish not_checked and must not erase an earlier
// check. Takes no space (see Store).
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

// mapMatch is one instrument-map hit with the catalog's type, currency, isin
// and ticker: what Resolve answers with, without asking the broker, which is the
// point of checking the map first.
type mapMatch struct {
	InstrumentID uuid.UUID
	Type         instrument.Type
	Currency     string
	ISIN, Ticker string
}

// mapByInstrumentUID and mapByFIGI are lookupMap's two lookups, in that order;
// both return pgx.ErrNoRows on a miss. instrument_id cascades on delete, so a
// match always has its catalog row.
func (s *Store) mapByInstrumentUID(ctx context.Context, connectionID uuid.UUID, instrumentUID string) (mapMatch, error) {
	if instrumentUID == "" {
		// An empty uid would match whatever an earlier empty write left; refused
		// before the query.
		return mapMatch{}, fmt.Errorf("tinvest: instrument map by instrument_uid: %w", pgx.ErrNoRows)
	}
	return s.mapHit(ctx, "instrument map by instrument_uid", `
		SELECT instrument_id FROM tinvest_instrument_map
		WHERE connection_id = $1 AND instrument_uid = $2`, connectionID, instrumentUID)
}

// mapByFIGI is the fallback when the operation's instrument_uid is unknown
// here (older operations, or a drifted uid). The figi index is not unique, so the
// most recently updated row wins.
func (s *Store) mapByFIGI(ctx context.Context, connectionID uuid.UUID, figi string) (mapMatch, error) {
	if figi == "" {
		// Every row with no figi shares "", which would match an unrelated
		// instrument.
		return mapMatch{}, fmt.Errorf("tinvest: instrument map by figi: %w", pgx.ErrNoRows)
	}
	return s.mapHit(ctx, "instrument map by figi", `
		SELECT instrument_id FROM tinvest_instrument_map
		WHERE connection_id = $1 AND figi = $2
		ORDER BY updated_at DESC
		LIMIT 1`, connectionID, figi)
}

// mapHit reads the one instrument a map query names, then its catalog row.
func (s *Store) mapHit(ctx context.Context, what, sql string, args ...any) (mapMatch, error) {
	var id uuid.UUID
	if err := s.db.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
		return mapMatch{}, fmt.Errorf("tinvest: %s: %w", what, err)
	}
	i, err := s.catalog().ByID(ctx, id)
	if err != nil {
		return mapMatch{}, fmt.Errorf("tinvest: %s: read the catalog: %w", what, err)
	}
	return mapMatch{InstrumentID: id, Type: i.Type, Currency: i.Currency, ISIN: i.ISIN, Ticker: i.Ticker}, nil
}

// saveMap records instrumentID for ref.InstrumentUID with the other identifiers
// and the catalog's isin and ticker. Called on every resolution, map hits
// included, so a drift in figi, position_uid or asset_uid alone is captured.
//
// An empty identifier never erases a stored one (COALESCE(NULLIF(...))): ref comes
// from one operation, the row from all of them, and erasing the figi would break
// the drift fallback (mapByFIGI). listingCurrency is treated the same way, since a
// map hit has no passport and passes "". isin and ticker are assigned outright:
// they are the catalog's current columns, so empty is true.
//
// Nothing is written when nothing changed: a long history resolves one instrument
// on every operation. All columns are NOT NULL, so <> suffices. Callers never pass
// an empty InstrumentUID (Resolve's guard).
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

// QuotableInstrument is a mapped broker listing with what storing its price
// needs: the catalog instrument, the broker id to ask under, the listing's
// currency.
type QuotableInstrument struct {
	InstrumentUID string
	InstrumentID  uuid.UUID
	// Currency is empty for a mapping older than migration 0017; such a listing
	// is not priced until SetMapCurrency fills it.
	Currency string
	// Bond: the catalog row is a bond, whose price is a percentage of its face.
	Bond bool
}

// QuotableByConnection is every listing this connection can price, ordered by
// broker id. One row per (connection, instrument_uid), so one catalog instrument
// may appear twice (drifted ids, another venue); both are priced, and the price's
// own day decides the latest quote.
func (s *Store) QuotableByConnection(ctx context.Context, connectionID uuid.UUID) ([]QuotableInstrument, error) {
	const what = "list quotable instruments"
	rows, err := s.db.Query(ctx, `
		SELECT instrument_uid, instrument_id, currency
		FROM tinvest_instrument_map
		WHERE connection_id = $1 AND instrument_uid <> ''
		ORDER BY instrument_uid`, connectionID)
	if err != nil {
		return nil, fmt.Errorf("tinvest: %s: %w", what, err)
	}
	listings, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (QuotableInstrument, error) {
		var q QuotableInstrument
		return q, row.Scan(&q.InstrumentUID, &q.InstrumentID, &q.Currency)
	})
	if err != nil {
		return nil, fmt.Errorf("tinvest: %s: %w", what, err)
	}
	ids := make([]uuid.UUID, len(listings))
	for i, q := range listings {
		ids[i] = q.InstrumentID
	}
	papers, err := s.catalogOf(ctx, what, ids)
	if err != nil {
		return nil, err
	}
	out := []QuotableInstrument{}
	for _, q := range listings {
		if i, ok := papers[q.InstrumentID]; ok {
			q.Bond = i.Type == instrument.TypeBond
			out = append(out, q)
		}
	}
	return out, nil
}

// SetMapCurrency fills a listing's currency for mappings migration 0017 left
// empty; the quotes worker asks the passport once and remembers.
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

// instrumentMap is what this connection knows about the broker's instruments,
// in the reconciliation's two shapes: an InstrumentIndex from broker identifiers
// to catalog instruments, and a label per instrument (ticker, else name). Both
// instrument_uid and figi are indexed because identifiers drift (see
// Resolver.lookupMap). One read of the table, not a lookup per position.
//
// An empty identifier is not indexed, or "" would match every position lacking
// it. A figi two rows map to different instruments answers for neither: the index
// feeds a screen, where a confident wrong match is worse than reporting the
// position as unmatched, and picking by row order would vary between runs.
func (s *Store) instrumentMap(ctx context.Context, connectionID uuid.UUID) (InstrumentIndex, map[uuid.UUID]string, error) {
	fail := func(err error) (InstrumentIndex, map[uuid.UUID]string, error) {
		return InstrumentIndex{}, nil, fmt.Errorf("tinvest: read instrument map: %w", err)
	}

	type entry struct {
		uid, figi string
		id        uuid.UUID
	}
	rows, err := s.db.Query(ctx, `
		SELECT instrument_uid, figi, instrument_id
		FROM tinvest_instrument_map
		WHERE connection_id = $1`, connectionID)
	if err != nil {
		return fail(err)
	}
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (entry, error) {
		var e entry
		return e, row.Scan(&e.uid, &e.figi, &e.id)
	})
	if err != nil {
		return fail(err)
	}
	ids := make([]uuid.UUID, len(entries))
	for i, e := range entries {
		ids[i] = e.id
	}
	papers, err := s.catalog().ByIDs(ctx, ids)
	if err != nil {
		return fail(err)
	}

	index := InstrumentIndex{ByUID: map[string]uuid.UUID{}, ByFIGI: map[string]uuid.UUID{}}
	contested := map[string]bool{}
	labels := map[uuid.UUID]string{}
	for _, e := range entries {
		paper, ok := papers[e.id]
		if !ok {
			continue
		}
		uid, figi, id, ticker, name := e.uid, e.figi, e.id, paper.Ticker, paper.Name
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
	return index, labels, nil
}
