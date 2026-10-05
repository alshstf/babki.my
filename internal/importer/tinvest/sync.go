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

// contentKey is what one broker operation is, as far as the mirror is
// concerned: instant, kind, paper, currency, amount and units. Everything else
// (identifiers, wording, state) may change under a row. The broker's ids cannot
// carry identity: its docs say an operation id may change, and old operations
// have had their figi and instrument_uid rewritten.
//
// The key is built here only, only from an OperationItem, stored in content_key
// and read back as an opaque string. Rebuilding it from stored columns would be
// wrong: occurred_at keeps microseconds while the broker sends nanoseconds (see
// TestSyncMirrorStoresTheInstantLessPreciselyThanTheKeyDoes).
//
//   - the instant: RFC 3339 with nanoseconds, converted to UTC.
//   - the type: the enum name as sent.
//   - the instrument: instrument_uid, else figi, else empty (a top-up has none).
//   - the currency, upper case (see upperCurrency).
//   - the amount: units plus nano as a decimal, so "-0" units with -200000000
//     nano read as -0.2.
//   - the quantity in units.
//
// "|" separates them; none of the fields can contain it.
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

// upperCurrency upper-cases a code before it is keyed or stored. The client
// already does this on the live path; this guards hand-built items (tests,
// future callers), which would otherwise key "rub" differently from the same
// operation fetched later, mark the row disappeared and write a second one.
func upperCurrency(code string) string { return strings.ToUpper(code) }

// dedupInPage collapses rows one read of the history repeated, keeping the
// first copy per broker id. Within one read the id is stable, and the docs warn
// of repeated rows (small pages, a revisited cursor). Two lawfully identical
// operations have different ids and both survive. An operation without an id is
// kept as is.
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

// MirrorSyncStats is what one pass over one broker account changed. Read counts
// operations reported after dedupInPage, not the slice length.
type MirrorSyncStats struct {
	Read, Added, Disappeared int
}

// mirrorMatch is a stored row as the comparison sees it: the key as read, and
// nothing a key could be rebuilt from.
type mirrorMatch struct {
	id          uuid.UUID
	contentKey  string
	disappeared bool
}

// SyncMirror brings one broker account's mirror into agreement with what the
// broker just said and returns what changed.
//
// fetched must be the link's full current history: everything stored and not
// found is marked disappeared, so a window would mark the past as gone. The
// broker rewrites old operations, which is why the comparison sees everything.
//
// Rows match by content key with multiplicity: three fetched against two stored
// adds one; one against three marks two. Identical operations get a row each
// (content_key has no unique index, migration 0014).
//
// Nothing is deleted. A vanished operation gets disappeared_at, once; if it
// returns, the same row is unmarked.
//
// A matched row is rewritten from this fetch: every attribute the key is not
// built from. Its id, filing, first_seen_at, unparsed verdict and key fields stay
// (see mirrorConfirmSQL). Otherwise a corrected commission, or an operation first
// seen in progress, would stay wrong forever.
//
// The connection is locked as the transaction's first statement, so two runs
// serialize. First matters: at READ COMMITTED a mirror read before the wait would
// be stale (TestSyncMirrorTakesTheLockBeforeItReadsTheMirror). The broker is
// called before the transaction opens. All or nothing. now is the run's single
// clock.
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

	// The lock first; nothing reads the mirror before it.
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

	// Live rows first, oldest first: a fetch must not revive a marked row
	// while burying a live one with the same key.
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
		// The whole item: the row keeps its identity and takes the fetch's
		// attributes.
		confirmed = append(confirmed, confirmation{id: match.id, item: it})
	}

	var leftover []mirrorMatch
	for _, m := range existing {
		if consumed[m.id] || m.disappeared {
			continue
		}
		leftover = append(leftover, m)
	}
	rekeyed, leftover, toInsert := reidentify(leftover, toInsert)
	for _, r := range rekeyed {
		confirmed = append(confirmed, confirmation{id: r.id, item: r.item})
	}
	toDisappear := make([]uuid.UUID, 0, len(leftover))
	for _, m := range leftover {
		toDisappear = append(toDisappear, m.id)
	}

	// The transaction makes the run atomic; the order (confirm, mark,
	// insert) lets the rollback test see that the first two really ran before
	// the insert failed.
	if err := confirmMirrorRows(ctx, tx, confirmed, now); err != nil {
		return MirrorSyncStats{}, err
	}
	if err := rekeyMirrorRows(ctx, tx, link.ID, rekeyed); err != nil {
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

// rekeyed is a row the broker rewrote: the stored row, its old key and the
// item that is the same operation under new identifiers.
type rekeyed struct {
	id     uuid.UUID
	oldKey string
	item   OperationItem
}

// reidentify pairs an unmatched row with an unmatched item that differ only in
// the paper's identifiers, which the broker rewrites on old operations: same
// moment, type, payment and quantity, and exactly one of each on the link.
// Anything less certain stays a row gone and a row new.
func reidentify(rows []mirrorMatch, items []OperationItem) ([]rekeyed, []mirrorMatch, []OperationItem) {
	if len(rows) == 0 || len(items) == 0 {
		return nil, rows, items
	}
	rowsBy := map[string][]int{}
	for i, m := range rows {
		rowsBy[keyWithoutPaper(m.contentKey)] = append(rowsBy[keyWithoutPaper(m.contentKey)], i)
	}
	itemsBy := map[string][]int{}
	for i, it := range items {
		k := keyWithoutPaper(contentKey(it))
		itemsBy[k] = append(itemsBy[k], i)
	}
	var pairs []rekeyed
	pairedRow, pairedItem := map[int]bool{}, map[int]bool{}
	for k, ri := range rowsBy {
		ii := itemsBy[k]
		if len(ri) != 1 || len(ii) != 1 {
			continue
		}
		pairs = append(pairs, rekeyed{id: rows[ri[0]].id, oldKey: rows[ri[0]].contentKey, item: items[ii[0]]})
		pairedRow[ri[0]], pairedItem[ii[0]] = true, true
	}
	var restRows []mirrorMatch
	for i, m := range rows {
		if !pairedRow[i] {
			restRows = append(restRows, m)
		}
	}
	var restItems []OperationItem
	for i, it := range items {
		if !pairedItem[i] {
			restItems = append(restItems, it)
		}
	}
	return pairs, restRows, restItems
}

// keyWithoutPaper is a content key minus its third field, the paper.
func keyWithoutPaper(key string) string {
	parts := strings.Split(key, "|")
	if len(parts) < 3 {
		return key
	}
	return strings.Join(append(parts[:2:2], parts[3:]...), "|")
}

// rekeyMirrorRows refiles each rewritten row under its new key and moves its
// explanation with it.
func rekeyMirrorRows(ctx context.Context, tx pgx.Tx, linkID uuid.UUID, rows []rekeyed) error {
	for _, r := range rows {
		newKey := contentKey(r.item)
		if _, err := tx.Exec(ctx, `UPDATE tinvest_operations_mirror SET content_key = $2, instrument_uid = $3 WHERE id = $1`,
			r.id, newKey, r.item.InstrumentUID); err != nil {
			return fmt.Errorf("tinvest: rekey mirror row %s: %w", r.id, err)
		}
		// Unless the new key is already explained: one explanation per row.
		if _, err := tx.Exec(ctx, `UPDATE tinvest_mirror_explanations SET content_key = $3
			WHERE link_id = $1 AND content_key = $2
			  AND NOT EXISTS (SELECT 1 FROM tinvest_mirror_explanations o
			                   WHERE o.link_id = $1 AND o.content_key = $3)`, linkID, r.oldKey, newKey); err != nil {
			return fmt.Errorf("tinvest: move the explanation of mirror row %s: %w", r.id, err)
		}
	}
	return nil
}

// readMirrorMatches reads the link's rows in comparison order: live first,
// then marked, oldest first, by id within one instant.
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

// confirmation pairs a matched row's id with the item it now holds.
type confirmation struct {
	id   uuid.UUID
	item OperationItem
}

// mirrorConfirmSQL rewrites a matched row from its fetch. Left alone: id and
// filing (the journal points at the id), the key's fields (occurred_at, op_type,
// currency, payment, quantity; a change would not match), first_seen_at, and the
// unparsed verdict (the projection's). instrument_uid is a key field too (or figi
// when there is no uid), so figi is set and uid is not.
const mirrorConfirmSQL = `
	UPDATE tinvest_operations_mirror SET
		broker_operation_id = $2, parent_operation_id = $3, state = $4,
		price = $5, commission = $6, commission_currency = $7, accrued_int = $8,
		figi = $9, position_uid = $10, asset_uid = $11, instrument_type = $12,
		description = $13, raw = $14,
		quantity_done = $15, ticker = $16, class_code = $17,
		last_confirmed_at = $18, disappeared_at = NULL
	WHERE id = $1`

// confirmMirrorRows rewrites every matched row in one batch, changed or not:
// comparing each column to skip a write is not worth it for tens of thousands of
// rows an hour. A much larger instance would want that revisited.
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
			it.Description, rawDocument(it.Raw),
			it.QuantityDone, it.Ticker, it.ClassCode, now)
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
		commission_currency, accrued_int, quantity, quantity_done, figi,
		ticker, class_code, instrument_uid, position_uid, asset_uid, instrument_type, description, raw,
		content_key, first_seen_at, last_confirmed_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
		$16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27)`

// insertMirrorRows writes new rows in one batch (a first import is thousands of
// operations; see #73). first_seen_at and last_confirmed_at are the run's
// moment, not the transaction's clock.
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
		it.Quantity, it.QuantityDone, it.FIGI, it.Ticker, it.ClassCode, it.InstrumentUID,
		it.PositionUID, it.AssetUID, it.InstrumentType, it.Description, rawDocument(it.Raw),
		contentKey(it), now, now,
	}
}

// moneyOrNothing maps an optional money field to a nullable column: nil when
// the broker sent nothing (an empty MoneyValue), the amount otherwise. A sent
// value is assumed to carry its currency, per protojson omitting zero fields; if
// not, a zero is stored as nothing, which says the same. An amount without a
// currency is kept: malformed, not absent.
func moneyOrNothing(v MoneyValue) *decimal.Decimal {
	if v.Currency == "" && v.Units == 0 && v.Nano == 0 {
		return nil
	}
	d := v.Decimal()
	return &d
}

// rawDocument is the jsonb value: JSON null for an item with no bytes (not
// from the wire), since an empty value would fail the encoder and the sync.
func rawDocument(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}
