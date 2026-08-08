package tinvest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// contentKey is what one broker operation IS, as far as the mirror is
// concerned: the instant, the kind, the paper, the currency, the amount and
// the number of units. Everything else the broker sends — its identifiers,
// its wording, its state — is an attribute that may change under a row
// without the row becoming a different operation.
//
// The key exists because the broker's identifiers cannot carry identity. The
// API's own documentation says an operation's id may change over time and is
// not to be relied on as a primary key; the same goes for
// parent_operation_id, and old operations have had their figi and
// instrument_uid rewritten. What is left to recognize an operation by is what
// it says.
//
// THE KEY IS BUILT HERE AND NOWHERE ELSE, and only ever from an OperationItem
// — that is, from what came over the wire. It is then stored in content_key
// and, on every later comparison, read back out of that column as an opaque
// string. Rebuilding it from the mirror's other columns would be a second
// implementation of one value, and worse, a wrong one: this project has
// already been caught by a key assembled from what the database returned not
// matching the key it was asked with, because a date goes in as a date and
// comes back as midnight UTC, and a number goes in one shape and comes back
// in another. It is not a hypothetical here either — occurred_at is a
// timestamptz, which keeps microseconds, and the broker sends nanoseconds, so
// the stored instant is demonstrably not the instant the key was built from
// (see TestSyncMirrorStoresTheInstantLessPreciselyThanTheKeyDoes). Neither
// occurred_at nor payment ever takes part in building a key.
//
// Field by field:
//   - the instant, RFC 3339 with nanoseconds, in UTC. The broker sends UTC,
//     but an item constructed anywhere else must not key differently for the
//     same moment, so it is converted rather than assumed.
//   - the operation type, the broker's enum name verbatim.
//   - the instrument: instrument_uid if there is one, else figi, else
//     nothing. Both are absent on operations that have no paper at all (a
//     top-up, a service fee), and an empty field there is a fact, not a gap.
//   - the currency, upper case — see upperCurrency for why this package
//     normalizes a code the client has already normalized.
//   - the amount, as a decimal string. Built by ADDING units and nano rather
//     than by pasting them together, which is what makes the broker's
//     "negative zero" split (units "-0", nano -200000000, meaning -0.2) read
//     as one number.
//   - the quantity in units.
//
// The separator is "|", which none of these fields can contain: an enum name,
// a uuid, a three-letter code, a decimal and an integer. Two different
// operations therefore cannot collide by re-splitting differently.
func contentKey(it OperationItem) string {
	instrument := it.InstrumentUID
	if instrument == "" {
		instrument = it.FIGI
	}
	return strings.Join([]string{
		it.Date.UTC().Format(time.RFC3339Nano),
		it.Type,
		instrument,
		upperCurrency(it.Payment.Currency),
		it.Payment.Decimal().String(),
		strconv.FormatInt(it.Quantity, 10),
	}, "|")
}

// upperCurrency normalizes a currency code this package is about to key on or
// store.
//
// THE AUTHORITY ON THE CASE OF A CURRENCY CODE IS THE CLIENT, and this
// function is not a second one. wireMoneyValue.parse upper-cases every code
// that comes off the wire, and MoneyValue's own documentation states the
// field is always upper case, so on the live path there is nothing here to
// do. It is a guard for the one path that does not go through the client at
// all: an OperationItem assembled by hand, which is how every test in this
// package builds one and how any future caller assembling an item would.
//
// The guard earns its keep because it is called both where the key is built
// and where the row is written. Without it a hand-built "rub" would key as
// "rub" and sit in a column the schema documents as normalized upper case,
// and the first sync that brought the same operation in through the client
// would compute a different key, mark the stored row disappeared and write a
// second row for one operation.
func upperCurrency(code string) string { return strings.ToUpper(code) }

// dedupInPage collapses the rows one read of the broker's history repeated,
// keeping the first copy of each broker id.
//
// It is deduplication WITHIN a single read, and nothing else — it never looks
// at the mirror. Within one read the broker's identifier is stable, which is
// exactly what makes it usable here and unusable anywhere else: the
// documented failure it treats is a page repeating rows (the API's own docs
// warn of it for small page sizes, and the cursor walk can revisit), where
// the repeat carries the same id it did a moment ago.
//
// It must not be confused with the comparison against the mirror, which is by
// content and by multiplicity. Two legitimately identical operations — one
// paper, one second, one amount — carry DIFFERENT broker ids, so both survive
// this pass, and both go on to become rows of their own.
//
// An operation the broker left unidentified is kept as it is: deduplicating
// by a key that is not there would collapse distinct operations, which is the
// one thing the mirror exists to prevent.
func dedupInPage(items []OperationItem) []OperationItem {
	out := make([]OperationItem, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		if it.ID != "" {
			if seen[it.ID] {
				continue
			}
			seen[it.ID] = true
		}
		out = append(out, it)
	}
	return out
}

// MirrorSyncStats is what one pass over one broker account changed.
//
// Read is the number of operations the broker actually reported, after its
// own repeated rows were collapsed (see dedupInPage) — not the length of the
// slice handed in. A page that repeated a row did not report two operations.
type MirrorSyncStats struct {
	Read, Added, Disappeared int
}

// mirrorMatch is one existing mirror row as the comparison sees it. It
// deliberately carries the content key AS READ, and nothing the key is built
// from: there is no way, from this struct, to rebuild a key from stored
// columns even by accident.
type mirrorMatch struct {
	id          uuid.UUID
	contentKey  string
	disappeared bool
}

// SyncMirror brings the mirror of one broker account into agreement with what
// the broker just said, and returns what it changed.
//
// PRECONDITION: fetched MUST BE THE FULL CURRENT HISTORY OF THIS LINK — every
// operation the broker returns for link.BrokerAccountID, from the account's
// opening to now, in one slice. It is not a batch, not a page and not a
// window. Everything the mirror holds for this link and does not find in
// fetched is marked disappeared, so handing this a window of recent
// operations would mark the entire history before that window as gone on the
// first run — the exact opposite of what the mirror is for. There is no
// bounded-fetch mode and none is planned: the broker rewrites old operations
// after the fact, which is why the comparison is by content and why it has to
// see everything to be a comparison at all.
//
// THE COMPARISON IS A MULTISET COMPARISON BY CONTENT. Rows are matched by
// content key WITH MULTIPLICITY: three fetched operations sharing a key
// against two stored rows adds one row, not three and not none; one fetched
// against three stored marks two, not one. Two lawfully identical operations
// — the same paper, the same second, the same amount — are two operations and
// get a row each, which is why content_key carries no unique index (see
// migration 0014).
//
// NOTHING IS EVER DELETED. An operation the broker stops returning gets
// disappeared_at and stays; if the broker returns it again, that mark is
// cleared on the very same row rather than a second row appearing. A row
// already marked is not marked again — the mark records when the broker
// dropped the operation, and that moment does not move.
//
// A MATCHED ROW IS REWRITTEN FROM WHAT THE BROKER SAYS NOW. The content key
// answers "which row is this"; it does not freeze the row's attributes at the
// first sighting. Every field the broker sends that the key is not built from
// — the state, the price, the commission and its currency, the accrued
// interest, the figi, the position and asset uids, the instrument type, the
// description, the parent operation id, the raw document and the broker's own
// operation id — is set to the value in this fetch. What a refresh leaves
// alone is the row's own id and where it is filed (the journal points at the
// id), first_seen_at, the unparsed reason (the projection's verdict, not a
// sync's), and the key together with the fields it was built from, which
// cannot change without the operation becoming a different one. See
// mirrorConfirmSQL, which lists it column by column.
//
// Without this, a broker that corrected a commission would leave the mirror
// wrong for good, and an operation first seen in progress would stay in
// progress for good and never reach the journal at all.
//
// THE WHOLE COMPARISON HAPPENS UNDER A LOCK ON THE CONNECTION, taken as the
// transaction's first statement. Two runs of the same connection therefore
// serialize: without the lock both would compute their difference against the
// same stored state and both would insert the same new rows.
//
// FIRST is load-bearing and not tidiness. Reading the mirror before taking
// the lock leaves the run waiting with a stale answer in hand: at READ
// COMMITTED the read that ran before the wait saw the mirror as it was
// BEFORE the other run committed, and acting on it inserts rows that are
// already there. The statement below must stay above readMirrorMatches; see
// TestSyncMirrorTakesTheLockBeforeItReadsTheMirror, which is what tells the
// two orders apart (the waiting test alone passes under either). The lock covers
// the comparison and the writes and nothing else — fetched is asked for as an
// argument, not read here, so that the caller does its talking to the broker
// before this transaction opens rather than inside it.
//
// The run is all or nothing: either the mirror ends up describing this fetch
// entirely, or it is exactly as it was.
//
// now is the run's clock, passed in rather than read here so that a whole run
// stamps one moment and so that tests can state it.
func (s *Store) SyncMirror(ctx context.Context, connID uuid.UUID, link AccountLink, fetched []OperationItem, now time.Time) (MirrorSyncStats, error) {
	if link.ConnectionID != connID {
		return MirrorSyncStats{}, fmt.Errorf("%w: link %s is under connection %s, not %s",
			ErrLinkNotInConnection, link.ID, link.ConnectionID, connID)
	}
	items := dedupInPage(fetched)

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return MirrorSyncStats{}, fmt.Errorf("tinvest: sync mirror: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The lock, first. Nothing below may read the mirror before it is held.
	var locked uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM tinvest_connections WHERE id = $1 FOR UPDATE`, connID).Scan(&locked)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return MirrorSyncStats{}, fmt.Errorf("%w: %s", ErrConnectionNotFound, connID)
		}
		return MirrorSyncStats{}, fmt.Errorf("tinvest: lock connection: %w", err)
	}

	existing, err := readMirrorMatches(ctx, tx, link.ID)
	if err != nil {
		return MirrorSyncStats{}, err
	}

	// Buckets in the order the query returned them: live rows before marked
	// ones, oldest first within each. Live first is what keeps a fetch from
	// raising a marked row from the dead while burying a live one holding the
	// same key — which, once it happened, would happen again on every run.
	buckets := make(map[string][]*mirrorMatch, len(existing))
	for i := range existing {
		m := &existing[i]
		buckets[m.contentKey] = append(buckets[m.contentKey], m)
	}

	var (
		confirmed []confirmation
		consumed  = make(map[uuid.UUID]bool, len(existing))
		toInsert  []OperationItem
	)
	for _, it := range items {
		key := contentKey(it)
		bucket := buckets[key]
		if len(bucket) == 0 {
			toInsert = append(toInsert, it)
			continue
		}
		match := bucket[0]
		buckets[key] = bucket[1:]
		consumed[match.id] = true
		// The whole item, not just its identifier: the row keeps its identity
		// and takes on this fetch's attributes.
		confirmed = append(confirmed, confirmation{id: match.id, item: it})
	}

	var toDisappear []uuid.UUID
	for _, m := range existing {
		if consumed[m.id] || m.disappeared {
			continue
		}
		toDisappear = append(toDisappear, m.id)
	}

	// What makes the run all-or-nothing is the transaction, in whatever order
	// these three write. The order is nonetheless fixed — confirmations, then
	// marks, then inserts — so that the test which proves it can tell a
	// rollback apart from a run that never got started: with the inserts last,
	// a row the database refuses is refused after the other two have really
	// executed, and what the test then sees restored could only have been
	// restored by rolling them back.
	if err := confirmMirrorRows(ctx, tx, confirmed, now); err != nil {
		return MirrorSyncStats{}, err
	}
	if len(toDisappear) > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE tinvest_operations_mirror SET disappeared_at = $2
			WHERE id = ANY($1)`, toDisappear, now); err != nil {
			return MirrorSyncStats{}, fmt.Errorf("tinvest: mark mirror rows gone: %w", err)
		}
	}
	if len(toInsert) > 0 {
		if err := insertMirrorRows(ctx, tx, connID, link.ID, toInsert, now); err != nil {
			return MirrorSyncStats{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return MirrorSyncStats{}, fmt.Errorf("tinvest: sync mirror: %w", err)
	}
	return MirrorSyncStats{
		Read:        len(items),
		Added:       len(toInsert),
		Disappeared: len(toDisappear),
	}, nil
}

// readMirrorMatches reads the link's rows in the order the comparison
// consumes them: live rows first, then the marked ones, oldest first within
// each group and by id where two rows were first seen in the same instant (a
// whole first import shares one). content_key comes back as the string it was
// written as and is never taken apart.
func readMirrorMatches(ctx context.Context, tx pgx.Tx, linkID uuid.UUID) ([]mirrorMatch, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, content_key, disappeared_at IS NOT NULL
		FROM tinvest_operations_mirror
		WHERE link_id = $1
		ORDER BY (disappeared_at IS NOT NULL), first_seen_at, id`, linkID)
	if err != nil {
		return nil, fmt.Errorf("tinvest: read mirror: %w", err)
	}
	defer rows.Close()
	var out []mirrorMatch
	for rows.Next() {
		var m mirrorMatch
		if err := rows.Scan(&m.id, &m.contentKey, &m.disappeared); err != nil {
			return nil, fmt.Errorf("tinvest: read mirror: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tinvest: read mirror: %w", err)
	}
	return out, nil
}

// confirmation pairs a mirror row that the fetch matched with the item it was
// matched to. Both halves are needed: the id says which row keeps its
// identity, the item says what that row now holds.
type confirmation struct {
	id   uuid.UUID
	item OperationItem
}

// mirrorConfirmSQL rewrites one matched row from the fetch it was matched to.
//
// What it does NOT set is as deliberate as what it does. id, connection_id
// and link_id stay because the journal's operations point at the id and the
// row's filing is not the broker's to change. occurred_at, op_type, currency,
// payment and quantity stay because the content key is built from them: an
// item differing in any one of those does not match this row in the first
// place, so there is nothing here to update. first_seen_at stays because it
// records the first sighting and this is not one. unparsed_reason and
// unparsed_detail stay because together they are the projection's verdict,
// written by SetUnparsedVerdicts, and a sync has no opinion on it.
//
// instrument_uid is absent for a subtler reason than the rest: it is what the
// key was built from — or figi was, when the broker named no uid — so an item
// naming a different instrument computes a different key, does not match this
// row and never reaches this statement. The one corner where a match could
// survive a change to the column is the broker moving one and the same string
// between figi and instrument_uid; a FIGI code and a uuid are not the same
// shape, so it has not been seen, and it is named here rather than left for a
// reader to wonder about. figi IS set, because an operation that names an
// instrument_uid keyed on that and left its figi free to be corrected.
const mirrorConfirmSQL = `
	UPDATE tinvest_operations_mirror SET
		broker_operation_id = $2, parent_operation_id = $3, state = $4,
		price = $5, commission = $6, commission_currency = $7, accrued_int = $8,
		figi = $9, position_uid = $10, asset_uid = $11, instrument_type = $12,
		description = $13, raw = $14,
		last_confirmed_at = $15, disappeared_at = NULL
	WHERE id = $1`

// confirmMirrorRows rewrites every matched row, as ONE batch for the reason
// insertMirrorRows gives.
//
// EVERY CONFIRMED ROW IS REWRITTEN ON EVERY SYNC, whether anything about it
// changed or not: the statement carries no "where something differs" clause,
// because telling apart "the broker sent the same values" from "the broker
// sent different ones" would mean comparing thirteen columns per row to save
// a write. For a personal instance with tens of thousands of operations that
// trade is the right way round and was weighed deliberately — the whole
// history is a few tens of thousands of rows, rewritten hourly in one batch
// inside one transaction. It is a decision, not an oversight; an instance
// with a hundred times that much would want it revisited.
func confirmMirrorRows(ctx context.Context, tx pgx.Tx, confirmed []confirmation, now time.Time) error {
	if len(confirmed) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, c := range confirmed {
		it := c.item
		batch.Queue(mirrorConfirmSQL, c.id, it.ID, it.ParentOperationID, it.State,
			moneyOrNothing(it.Price), moneyOrNothing(it.Commission),
			upperCurrency(it.Commission.Currency), moneyOrNothing(it.AccruedInt),
			it.FIGI, it.PositionUID, it.AssetUID, it.InstrumentType,
			it.Description, rawDocument(it.Raw), now)
	}
	br := tx.SendBatch(ctx, batch)
	for i, c := range confirmed {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return fmt.Errorf("tinvest: confirm mirror row %d (row %s, broker id %q): %w",
				i, c.id, c.item.ID, err)
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("tinvest: confirm mirror rows: %w", err)
	}
	return nil
}

const mirrorInsertSQL = `
	INSERT INTO tinvest_operations_mirror (
		connection_id, link_id, broker_operation_id, parent_operation_id,
		op_type, state, occurred_at, currency, payment, price, commission,
		commission_currency, accrued_int, quantity, figi, instrument_uid,
		position_uid, asset_uid, instrument_type, description, raw,
		content_key, first_seen_at, last_confirmed_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
		$16, $17, $18, $19, $20, $21, $22, $23, $24)`

// insertMirrorRows writes the new rows as ONE batch: a first import of a
// decade-old account is thousands of operations, and one statement at a time
// would be thousands of round trips waiting on each other (the fault #73
// described for transfer breakdowns).
//
// first_seen_at and last_confirmed_at are stated rather than left to the
// column's default, so every row a run writes carries the run's own moment
// instead of the transaction's clock.
func insertMirrorRows(ctx context.Context, tx pgx.Tx, connID, linkID uuid.UUID, items []OperationItem, now time.Time) error {
	batch := &pgx.Batch{}
	for _, it := range items {
		batch.Queue(mirrorInsertSQL, mirrorInsertArgs(connID, linkID, it, now)...)
	}
	br := tx.SendBatch(ctx, batch)
	for i, it := range items {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return fmt.Errorf("tinvest: insert mirror row %d (broker id %q): %w", i, it.ID, err)
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("tinvest: insert mirror rows: %w", err)
	}
	return nil
}

func mirrorInsertArgs(connID, linkID uuid.UUID, it OperationItem, now time.Time) []any {
	return []any{
		connID, linkID, it.ID, it.ParentOperationID,
		it.Type, it.State, it.Date.UTC(), upperCurrency(it.Payment.Currency),
		it.Payment.Decimal(), moneyOrNothing(it.Price), moneyOrNothing(it.Commission),
		upperCurrency(it.Commission.Currency), moneyOrNothing(it.AccruedInt),
		it.Quantity, it.FIGI, it.InstrumentUID,
		it.PositionUID, it.AssetUID, it.InstrumentType, it.Description, rawDocument(it.Raw),
		contentKey(it), now, now,
	}
}

// moneyOrNothing turns one of the broker's optional money fields into what
// goes in a nullable numeric column: nothing at all when the broker sent no
// such field, and the amount otherwise.
//
// "Sent no such field" is read as a wholly empty MoneyValue — no currency and
// no amount — because that is what an absent key unmarshals to. It rests on
// one assumption, stated rather than hidden: that a MoneyValue the gateway
// DOES send carries its currency, so a present-but-zero commission arrives as
// "0 RUB" and not as an empty object. The assumption comes from protojson's
// documented behaviour (zero-valued fields are omitted, so a field rendered
// at all was set) and from the spec's own examples, not from a live response
// carrying a zero commission — this package has never seen one. If it is
// wrong, the cost is bounded and one-directional: a zero amount in an unnamed
// currency would be stored as nothing rather than as zero, which says the
// same thing about the money either way.
//
// The distinction is worth keeping: a broker that charged no commission said
// "0 RUB", and a broker that said nothing about commission said nothing. A
// value with an amount but no currency is stored as an amount rather than
// discarded — it is malformed, but it is not absent.
func moneyOrNothing(v MoneyValue) *decimal.Decimal {
	if v.Currency == "" && v.Units == 0 && v.Nano == 0 {
		return nil
	}
	d := v.Decimal()
	return &d
}

// rawDocument is what goes in the mirror's jsonb column. An item that carries
// no bytes at all did not come off the wire (the client always fills Raw), and
// the JSON literal null says exactly that — there is no broker document for
// this row — where an empty value would be rejected by the encoder and take a
// whole sync down over a field nothing computes from.
func rawDocument(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}
