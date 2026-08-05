package tinvest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// This file is package tinvest (not tinvest_test) so it can reach contentKey
// and dedupInPage, which are unexported on purpose: the content key is the
// mirror's identity and nothing outside this package may build one.

// wireTime parses one of the broker's own timestamps the way the client does.
func wireTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return ts
}

func TestContentKeyHasTheShapeTheMirrorIsMatchedOn(t *testing.T) {
	got := contentKey(OperationItem{
		ID:            "77e0d3e1-0000-4000-8000-000000000001",
		Type:          "OPERATION_TYPE_BUY",
		State:         "OPERATION_STATE_EXECUTED",
		Date:          wireTime(t, "2026-03-14T07:30:15.123456789Z"),
		InstrumentUID: "abc-uid",
		FIGI:          "BBG000B9XRY4",
		Payment:       MoneyValue{Currency: "RUB", Units: -15230, Nano: -500000000},
		Quantity:      10,
		Description:   "Покупка ЦБ",
	})
	const want = "2026-03-14T07:30:15.123456789Z|OPERATION_TYPE_BUY|abc-uid|RUB|-15230.5|10"
	if got != want {
		t.Errorf("contentKey =\n%q\nwant\n%q", got, want)
	}
}

// The broker's own identifiers are precisely what the key must NOT depend on:
// the API's documentation says an operation's id may change over time and is
// not to be relied on as a primary key. If any of them leaked into the key, a
// reissued id would present a known operation as a new one.
func TestContentKeyIgnoresTheBrokersOwnIdentifiers(t *testing.T) {
	base := OperationItem{
		ID:                "id-before",
		ParentOperationID: "parent-before",
		Type:              "OPERATION_TYPE_BUY",
		Date:              wireTime(t, "2026-03-14T07:30:15Z"),
		InstrumentUID:     "abc-uid",
		Payment:           MoneyValue{Currency: "RUB", Units: -15230, Nano: -500000000},
		Quantity:          10,
		Description:       "Покупка ЦБ",
	}
	reissued := base
	reissued.ID = "id-after"
	reissued.ParentOperationID = "parent-after"
	reissued.Description = "Покупка ценных бумаг"
	reissued.State = "OPERATION_STATE_EXECUTED"

	if contentKey(base) != contentKey(reissued) {
		t.Errorf("a reissued id changed the key:\n%q\n%q", contentKey(base), contentKey(reissued))
	}
}

func TestContentKeyFallsBackToFIGIWhenTheBrokerNamedNoInstrumentUID(t *testing.T) {
	got := contentKey(OperationItem{
		ID:       "op-2",
		Type:     "OPERATION_TYPE_DIVIDEND",
		Date:     wireTime(t, "2026-05-20T09:00:00Z"),
		FIGI:     "BBG000B9XRY4",
		Payment:  MoneyValue{Currency: "RUB", Units: 1200, Nano: 0},
		Quantity: 0,
	})
	const want = "2026-05-20T09:00:00Z|OPERATION_TYPE_DIVIDEND|BBG000B9XRY4|RUB|1200|0"
	if got != want {
		t.Errorf("contentKey =\n%q\nwant\n%q", got, want)
	}
}

func TestContentKeyLeavesTheInstrumentEmptyWhenTheBrokerNamedNone(t *testing.T) {
	got := contentKey(OperationItem{
		ID:      "op-3",
		Type:    "OPERATION_TYPE_INPUT",
		Date:    wireTime(t, "2026-01-09T05:00:00Z"),
		Payment: MoneyValue{Currency: "RUB", Units: 50000, Nano: 0},
	})
	const want = "2026-01-09T05:00:00Z|OPERATION_TYPE_INPUT||RUB|50000|0"
	if got != want {
		t.Errorf("contentKey =\n%q\nwant\n%q", got, want)
	}
}

// The broker splits a money value into a signed integer part and a signed
// nano part, and a value between -1 and 0 arrives as units "-0" plus a
// negative nano. The key must read that as one number, not as two.
func TestContentKeySurvivesTheBrokersNegativeZero(t *testing.T) {
	payment, err := wireMoneyValue{Currency: "rub", Units: "-0", Nano: -200000000}.parse()
	if err != nil {
		t.Fatalf("parse the broker's negative zero: %v", err)
	}
	got := contentKey(OperationItem{
		ID:      "op-4",
		Type:    "OPERATION_TYPE_BROKER_FEE",
		Date:    wireTime(t, "2026-02-01T12:00:00Z"),
		Payment: payment,
	})
	const want = "2026-02-01T12:00:00Z|OPERATION_TYPE_BROKER_FEE||RUB|-0.2|0"
	if got != want {
		t.Errorf("contentKey =\n%q\nwant\n%q", got, want)
	}
}

func TestContentKeyIsTheSameInstantWhateverOffsetItArrivedIn(t *testing.T) {
	moscow := contentKey(OperationItem{
		Type:    "OPERATION_TYPE_INPUT",
		Date:    wireTime(t, "2026-03-14T10:30:15+03:00"),
		Payment: MoneyValue{Currency: "RUB", Units: 50000},
	})
	const want = "2026-03-14T07:30:15Z|OPERATION_TYPE_INPUT||RUB|50000|0"
	if moscow != want {
		t.Errorf("contentKey =\n%q\nwant\n%q", moscow, want)
	}
}

// The currency reaches this package lower case ("rub") and the client
// upper-cases it. The key upper-cases it too, so that a change on that path
// could never split one operation into two mirror rows.
func TestContentKeyReadsTheCurrencyInOneCaseOnly(t *testing.T) {
	got := contentKey(OperationItem{
		Type:    "OPERATION_TYPE_INPUT",
		Date:    wireTime(t, "2026-01-09T05:00:00Z"),
		Payment: MoneyValue{Currency: "rub", Units: 50000},
	})
	const want = "2026-01-09T05:00:00Z|OPERATION_TYPE_INPUT||RUB|50000|0"
	if got != want {
		t.Errorf("contentKey =\n%q\nwant\n%q", got, want)
	}
}

func TestContentKeyTellsApartOperationsThatDifferOnlyInAmount(t *testing.T) {
	base := OperationItem{
		Type:          "OPERATION_TYPE_BUY",
		Date:          wireTime(t, "2026-03-14T07:30:15Z"),
		InstrumentUID: "abc-uid",
		Payment:       MoneyValue{Currency: "RUB", Units: -100, Nano: 0},
		Quantity:      1,
	}
	other := base
	other.Payment = MoneyValue{Currency: "RUB", Units: -200, Nano: 0}

	if contentKey(base) == contentKey(other) {
		t.Errorf("−100 and −200 share the key %q", contentKey(base))
	}
}

func TestContentKeyTellsApartOperationsThatDifferOnlyInQuantity(t *testing.T) {
	base := OperationItem{
		Type:          "OPERATION_TYPE_BUY",
		Date:          wireTime(t, "2026-03-14T07:30:15Z"),
		InstrumentUID: "abc-uid",
		Payment:       MoneyValue{Currency: "RUB", Units: -100},
		Quantity:      1,
	}
	other := base
	other.Quantity = 2

	if contentKey(base) == contentKey(other) {
		t.Errorf("1 and 2 shares share the key %q", contentKey(base))
	}
}

func TestDedupInPageKeepsTheFirstOfARepeatedBrokerID(t *testing.T) {
	first := OperationItem{ID: "op-1", Payment: MoneyValue{Currency: "RUB", Units: 1}}
	second := OperationItem{ID: "op-2", Payment: MoneyValue{Currency: "RUB", Units: 2}}
	repeat := OperationItem{ID: "op-1", Payment: MoneyValue{Currency: "RUB", Units: 999}}

	got := dedupInPage([]OperationItem{first, second, repeat})
	if len(got) != 2 {
		t.Fatalf("dedupInPage kept %d of 3, want 2", len(got))
	}
	if got[0].ID != "op-1" || got[1].ID != "op-2" {
		t.Errorf("dedupInPage kept %q, %q; want op-1, op-2", got[0].ID, got[1].ID)
	}
	if got[0].Payment.Units != 1 {
		t.Errorf("the repeat won: units = %d, want 1 (the first copy)", got[0].Payment.Units)
	}
}

// An operation the broker left unidentified cannot be deduplicated by
// identifier, and collapsing two of them would silently drop a real
// operation — the one thing the mirror exists to prevent.
func TestDedupInPageKeepsOperationsTheBrokerLeftUnidentified(t *testing.T) {
	got := dedupInPage([]OperationItem{
		{ID: "", Payment: MoneyValue{Currency: "RUB", Units: 1}},
		{ID: "", Payment: MoneyValue{Currency: "RUB", Units: 2}},
	})
	if len(got) != 2 {
		t.Fatalf("dedupInPage kept %d of 2 unidentified operations, want both", len(got))
	}
}

func TestDedupInPageLeavesADistinctPageAlone(t *testing.T) {
	in := []OperationItem{{ID: "op-1"}, {ID: "op-2"}, {ID: "op-3"}}
	got := dedupInPage(in)
	if len(got) != 3 {
		t.Fatalf("dedupInPage kept %d of 3, want 3", len(got))
	}
	for i := range in {
		if got[i].ID != in[i].ID {
			t.Errorf("position %d = %q, want %q", i, got[i].ID, in[i].ID)
		}
	}
}

// ---------------------------------------------------------------------------
// SyncMirror. Everything below needs a database.
// ---------------------------------------------------------------------------

// op builds an OperationItem the way the client hands one over: raw bytes
// included, since the mirror keeps them.
func op(id, opType, instrumentUID string, at time.Time, currency string, units int64, nano int32, quantity int64) OperationItem {
	return OperationItem{
		ID:            id,
		Type:          opType,
		State:         "OPERATION_STATE_EXECUTED",
		Date:          at,
		InstrumentUID: instrumentUID,
		Payment:       MoneyValue{Currency: currency, Units: units, Nano: nano},
		Quantity:      quantity,
		Raw:           json.RawMessage(`{"id":"` + id + `"}`),
	}
}

// rowsByPayment indexes the link's mirror rows by the decimal string of their
// payment, which the tests below use to name a row without depending on the
// order they come back in.
func rowsByPayment(t *testing.T, f fixture) map[string]MirrorRow {
	t.Helper()
	rows, err := f.store.MirrorRowsByLink(f.ctx, f.link.ID)
	if err != nil {
		t.Fatalf("MirrorRowsByLink: %v", err)
	}
	out := make(map[string]MirrorRow, len(rows))
	for _, r := range rows {
		out[r.Payment.String()] = r
	}
	if len(out) != len(rows) {
		t.Fatalf("two mirror rows share a payment; this helper cannot name them apart")
	}
	return out
}

func mirrorCount(t *testing.T, f fixture) int {
	t.Helper()
	rows, err := f.store.MirrorRowsByLink(f.ctx, f.link.ID)
	if err != nil {
		t.Fatalf("MirrorRowsByLink: %v", err)
	}
	return len(rows)
}

func TestSyncMirrorFirstRunStoresEveryOperation(t *testing.T) {
	f := newFixture(t)
	now := wireTime(t, "2026-03-16T00:00:00Z")

	stats, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{
		op("op-1", "OPERATION_TYPE_BUY", "uid-1", wireTime(t, "2026-03-14T07:30:15Z"), "RUB", -15230, -500000000, 10),
		op("op-2", "OPERATION_TYPE_SELL", "uid-1", wireTime(t, "2026-03-15T10:00:00Z"), "RUB", 16000, 0, 10),
	}, now)
	if err != nil {
		t.Fatalf("SyncMirror: %v", err)
	}
	if want := (MirrorSyncStats{Read: 2, Added: 2, Disappeared: 0}); stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}

	rows, err := f.store.MirrorRowsByLink(f.ctx, f.link.ID)
	if err != nil {
		t.Fatalf("MirrorRowsByLink: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("mirror holds %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if r.DisappearedAt != nil {
			t.Errorf("%s arrived already marked gone", r.BrokerOperationID)
		}
		if !r.FirstSeenAt.Equal(now) || !r.LastConfirmedAt.Equal(now) {
			t.Errorf("%s: first seen %s, confirmed %s; want both %s",
				r.BrokerOperationID, r.FirstSeenAt, r.LastConfirmedAt, now)
		}
		if r.UnparsedReason != "" {
			t.Errorf("%s arrived with unparsed reason %q", r.BrokerOperationID, r.UnparsedReason)
		}
	}
}

// The mirror is what the broker said, so this checks the values rather than
// the count: every column that a projection will later read back.
func TestSyncMirrorStoresWhatTheBrokerSaid(t *testing.T) {
	f := newFixture(t)
	now := wireTime(t, "2026-03-16T00:00:00Z")

	item := OperationItem{
		ID:                "op-1",
		ParentOperationID: "op-parent",
		Type:              "OPERATION_TYPE_BUY",
		State:             "OPERATION_STATE_EXECUTED",
		Date:              wireTime(t, "2026-03-14T07:30:15.123456789Z"),
		InstrumentUID:     "uid-1",
		FIGI:              "BBG000B9XRY4",
		PositionUID:       "pos-1",
		AssetUID:          "asset-1",
		InstrumentType:    "share",
		Payment:           MoneyValue{Currency: "RUB", Units: -15230, Nano: -500000000},
		Price:             MoneyValue{Currency: "RUB", Units: 1523, Nano: 50000000},
		Commission:        MoneyValue{Currency: "USD", Units: -7, Nano: -600000000},
		AccruedInt:        MoneyValue{Currency: "RUB", Units: 3, Nano: 250000000},
		Quantity:          10,
		Description:       "Покупка ЦБ",
		Raw:               json.RawMessage(`{"id":"op-1","name":"Сбер"}`),
	}
	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{item}, now); err != nil {
		t.Fatalf("SyncMirror: %v", err)
	}

	rows, err := f.store.MirrorRowsByLink(f.ctx, f.link.ID)
	if err != nil {
		t.Fatalf("MirrorRowsByLink: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("mirror holds %d rows, want 1", len(rows))
	}
	r := rows[0]

	if r.ConnectionID != f.conn.ID || r.LinkID != f.link.ID {
		t.Errorf("row filed under connection %s / link %s, want %s / %s",
			r.ConnectionID, r.LinkID, f.conn.ID, f.link.ID)
	}
	if r.BrokerOperationID != "op-1" || r.ParentOperationID != "op-parent" {
		t.Errorf("ids = %q / %q, want op-1 / op-parent", r.BrokerOperationID, r.ParentOperationID)
	}
	if r.OpType != "OPERATION_TYPE_BUY" || r.State != "OPERATION_STATE_EXECUTED" {
		t.Errorf("type/state = %q / %q", r.OpType, r.State)
	}
	// Microseconds, not nanoseconds: see
	// TestSyncMirrorStoresTheInstantLessPreciselyThanTheKeyDoes.
	if want := wireTime(t, "2026-03-14T07:30:15.123456Z"); !r.OccurredAt.Equal(want) {
		t.Errorf("occurred at %s, want %s", r.OccurredAt, want)
	}
	if r.Currency != "RUB" {
		t.Errorf("currency = %q, want RUB", r.Currency)
	}
	if got := r.Payment.String(); got != "-15230.5" {
		t.Errorf("payment = %s, want -15230.5", got)
	}
	if r.Price == nil || r.Price.String() != "1523.05" {
		t.Errorf("price = %v, want 1523.05", r.Price)
	}
	if r.Commission == nil || r.Commission.String() != "-7.6" {
		t.Errorf("commission = %v, want -7.6", r.Commission)
	}
	if r.CommissionCurrency != "USD" {
		t.Errorf("commission currency = %q, want USD", r.CommissionCurrency)
	}
	if r.AccruedInt == nil || r.AccruedInt.String() != "3.25" {
		t.Errorf("accrued interest = %v, want 3.25", r.AccruedInt)
	}
	if r.Quantity != 10 {
		t.Errorf("quantity = %d, want 10", r.Quantity)
	}
	if r.FIGI != "BBG000B9XRY4" || r.InstrumentUID != "uid-1" ||
		r.PositionUID != "pos-1" || r.AssetUID != "asset-1" || r.InstrumentType != "share" {
		t.Errorf("instrument fields = %+v", r)
	}
	if r.Description != "Покупка ЦБ" {
		t.Errorf("description = %q", r.Description)
	}
	if r.ContentKey != contentKey(item) {
		t.Errorf("content key = %q, want %q", r.ContentKey, contentKey(item))
	}

	// raw is stored as jsonb, so what comes back is the broker's document,
	// not its bytes: compare the document.
	var back map[string]string
	if err := json.Unmarshal(r.Raw, &back); err != nil {
		t.Fatalf("raw did not come back as a document: %v (%s)", err, r.Raw)
	}
	if back["id"] != "op-1" || back["name"] != "Сбер" {
		t.Errorf("raw = %v, want the broker's own element", back)
	}
}

// THE REASON THE KEY IS NEVER REBUILT FROM THE STORED COLUMNS, demonstrated
// rather than asserted: PostgreSQL keeps a timestamptz to the microsecond,
// so the broker's nanoseconds do not survive the round trip — while the key,
// built once from the wire, keeps every one of them. Rebuilding a key from
// occurred_at would produce a string that matches nothing the mirror holds,
// and every operation would look new on every single run.
func TestSyncMirrorStoresTheInstantLessPreciselyThanTheKeyDoes(t *testing.T) {
	f := newFixture(t)
	now := wireTime(t, "2026-03-16T00:00:00Z")
	item := op("op-1", "OPERATION_TYPE_BUY", "uid-1",
		wireTime(t, "2026-03-14T07:30:15.123456789Z"), "RUB", -100, 0, 1)

	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{item}, now); err != nil {
		t.Fatalf("SyncMirror: %v", err)
	}
	rows, err := f.store.MirrorRowsByLink(f.ctx, f.link.ID)
	if err != nil {
		t.Fatalf("MirrorRowsByLink: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("mirror holds %d rows, want 1", len(rows))
	}

	if got := rows[0].OccurredAt.UTC().Format(time.RFC3339Nano); got != "2026-03-14T07:30:15.123456Z" {
		t.Errorf("stored instant = %s, want 2026-03-14T07:30:15.123456Z", got)
	}
	if !strings.HasPrefix(rows[0].ContentKey, "2026-03-14T07:30:15.123456789Z|") {
		t.Errorf("the key lost the nanoseconds too: %q", rows[0].ContentKey)
	}

	// And the proof that this matters: a second run of the very same
	// operation adds nothing, because the comparison uses the stored key and
	// not the stored instant.
	stats, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{item},
		wireTime(t, "2026-03-16T01:00:00Z"))
	if err != nil {
		t.Fatalf("second SyncMirror: %v", err)
	}
	if want := (MirrorSyncStats{Read: 1, Added: 0, Disappeared: 0}); stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}
}

// A money value the broker did not send at all is stored as nothing, not as
// zero: "the broker charged no commission" and "the broker said nothing about
// commission" are different statements and the mirror must not merge them.
func TestSyncMirrorKeepsAbsentMoneyApartFromZeroMoney(t *testing.T) {
	f := newFixture(t)
	now := wireTime(t, "2026-03-16T00:00:00Z")

	absent := op("op-absent", "OPERATION_TYPE_INPUT", "", wireTime(t, "2026-01-09T05:00:00Z"), "RUB", 50000, 0, 0)
	zero := op("op-zero", "OPERATION_TYPE_INPUT", "", wireTime(t, "2026-01-10T05:00:00Z"), "RUB", 60000, 0, 0)
	zero.Commission = MoneyValue{Currency: "RUB", Units: 0, Nano: 0}

	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{absent, zero}, now); err != nil {
		t.Fatalf("SyncMirror: %v", err)
	}
	byPayment := rowsByPayment(t, f)

	if got := byPayment["50000"]; got.Commission != nil {
		t.Errorf("an unsent commission became %v", got.Commission)
	}
	if got := byPayment["50000"]; got.CommissionCurrency != "" {
		t.Errorf("an unsent commission got the currency %q", got.CommissionCurrency)
	}
	if got := byPayment["60000"]; got.Commission == nil || !got.Commission.IsZero() {
		t.Errorf("a commission of zero rubles became %v, want 0", got.Commission)
	}
	if got := byPayment["60000"]; got.CommissionCurrency != "RUB" {
		t.Errorf("a commission of zero rubles got the currency %q, want RUB", got.CommissionCurrency)
	}
}

func TestSyncMirrorRepeatedRunAddsNothingAndOnlyMovesTheConfirmation(t *testing.T) {
	f := newFixture(t)
	first := wireTime(t, "2026-03-16T00:00:00Z")
	second := wireTime(t, "2026-03-16T01:00:00Z")
	items := []OperationItem{
		op("op-1", "OPERATION_TYPE_BUY", "uid-1", wireTime(t, "2026-03-14T07:30:15Z"), "RUB", -15230, -500000000, 10),
		op("op-2", "OPERATION_TYPE_SELL", "uid-1", wireTime(t, "2026-03-15T10:00:00Z"), "RUB", 16000, 0, 10),
	}

	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, items, first); err != nil {
		t.Fatalf("first SyncMirror: %v", err)
	}
	stats, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, items, second)
	if err != nil {
		t.Fatalf("second SyncMirror: %v", err)
	}
	if want := (MirrorSyncStats{Read: 2, Added: 0, Disappeared: 0}); stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}
	if n := mirrorCount(t, f); n != 2 {
		t.Fatalf("mirror holds %d rows after an identical second run, want 2", n)
	}
	for _, r := range rowsByPayment(t, f) {
		if !r.FirstSeenAt.Equal(first) {
			t.Errorf("%s: first seen %s, want %s (it was first seen on the first run)", r.BrokerOperationID, r.FirstSeenAt, first)
		}
		if !r.LastConfirmedAt.Equal(second) {
			t.Errorf("%s: confirmed %s, want %s", r.BrokerOperationID, r.LastConfirmedAt, second)
		}
	}
}

// The broker's own documentation says an operation's id may change. When it
// does, the operation is the same operation.
func TestSyncMirrorReissuedBrokerIDDoesNotCreateASecondRow(t *testing.T) {
	f := newFixture(t)
	first := wireTime(t, "2026-03-16T00:00:00Z")
	second := wireTime(t, "2026-03-16T01:00:00Z")
	at := wireTime(t, "2026-03-14T07:30:15Z")

	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link,
		[]OperationItem{op("id-before", "OPERATION_TYPE_BUY", "uid-1", at, "RUB", -15230, -500000000, 10)}, first); err != nil {
		t.Fatalf("first SyncMirror: %v", err)
	}
	before := rowsByPayment(t, f)["-15230.5"]

	stats, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link,
		[]OperationItem{op("id-after", "OPERATION_TYPE_BUY", "uid-1", at, "RUB", -15230, -500000000, 10)}, second)
	if err != nil {
		t.Fatalf("second SyncMirror: %v", err)
	}
	if want := (MirrorSyncStats{Read: 1, Added: 0, Disappeared: 0}); stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}
	if n := mirrorCount(t, f); n != 1 {
		t.Fatalf("mirror holds %d rows after the id was reissued, want 1", n)
	}
	after := rowsByPayment(t, f)["-15230.5"]
	if after.ID != before.ID {
		t.Errorf("the row was replaced (%s -> %s) instead of being kept", before.ID, after.ID)
	}
	if after.BrokerOperationID != "id-after" {
		t.Errorf("broker id = %q, want id-after (the identifier is an attribute and follows the broker)", after.BrokerOperationID)
	}
}

// A mirror row carries the LAST thing the broker said about the operation,
// not the first. The content key answers "which row is this"; everything
// outside the key is what the broker says about it now, and a broker that
// corrects a commission must not leave the mirror wrong for good.
//
// Every attribute a confirmation may refresh is corrected here at once, so
// that dropping any single one of them from the confirming statement shows up
// as a failure. What must survive is checked too: the row's own id (the
// journal points at it), the moment it was first seen, the content key, and
// the unparsed reason, which belongs to the projection and not to this.
func TestSyncMirrorRefreshesEveryAttributeTheBrokerCorrected(t *testing.T) {
	f := newFixture(t)
	first := wireTime(t, "2026-03-16T00:00:00Z")
	second := wireTime(t, "2026-03-16T01:00:00Z")

	before := OperationItem{
		ID:                "op-before",
		ParentOperationID: "parent-before",
		Type:              "OPERATION_TYPE_BUY",
		State:             "OPERATION_STATE_PROGRESS",
		Date:              wireTime(t, "2026-03-14T07:30:15Z"),
		InstrumentUID:     "uid-1",
		FIGI:              "BBG000B9XRY4",
		PositionUID:       "pos-before",
		AssetUID:          "asset-before",
		InstrumentType:    "share",
		Payment:           MoneyValue{Currency: "RUB", Units: -15230, Nano: -500000000},
		Price:             MoneyValue{Currency: "RUB", Units: 1523, Nano: 50000000},
		Commission:        MoneyValue{Currency: "RUB", Units: -7, Nano: -600000000},
		AccruedInt:        MoneyValue{Currency: "RUB", Units: 3, Nano: 250000000},
		Quantity:          10,
		Description:       "Покупка ЦБ",
		Raw:               json.RawMessage(`{"id":"op-before"}`),
	}
	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{before}, first); err != nil {
		t.Fatalf("first SyncMirror: %v", err)
	}
	original := rowsByPayment(t, f)["-15230.5"]
	if err := f.store.SetUnparsedReasons(f.ctx, map[uuid.UUID]string{original.ID: "unknown_type"}); err != nil {
		t.Fatalf("SetUnparsedReasons: %v", err)
	}

	// Everything outside the content key, corrected at once. The key's own
	// fields — the instant, the operation type, the instrument uid, the
	// currency, the payment and the quantity — are left as they were, because
	// changing one of them makes it a different operation, and this test would
	// then be about something else. FIGI is free to move because this
	// operation names an instrument uid, which is what the key took.
	after := before
	after.ID = "op-after"
	after.ParentOperationID = "parent-after"
	after.State = "OPERATION_STATE_EXECUTED"
	after.FIGI = "BBG000B9XRY5"
	after.PositionUID = "pos-after"
	after.AssetUID = "asset-after"
	after.InstrumentType = "bond"
	after.Price = MoneyValue{Currency: "RUB", Units: 1600, Nano: 250000000}
	after.Commission = MoneyValue{Currency: "USD", Units: -9, Nano: -900000000}
	after.AccruedInt = MoneyValue{Currency: "RUB", Units: 4, Nano: 750000000}
	after.Description = "Покупка ценных бумаг"
	after.Raw = json.RawMessage(`{"id":"op-after"}`)

	stats, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{after}, second)
	if err != nil {
		t.Fatalf("second SyncMirror: %v", err)
	}
	if want := (MirrorSyncStats{Read: 1, Added: 0, Disappeared: 0}); stats != want {
		t.Fatalf("stats = %+v, want %+v — a corrected attribute is the same operation", stats, want)
	}
	if n := mirrorCount(t, f); n != 1 {
		t.Fatalf("mirror holds %d rows, want 1", n)
	}
	row := rowsByPayment(t, f)["-15230.5"]

	if row.BrokerOperationID != "op-after" {
		t.Errorf("broker id = %q, want op-after", row.BrokerOperationID)
	}
	if row.ParentOperationID != "parent-after" {
		t.Errorf("parent id = %q, want parent-after", row.ParentOperationID)
	}
	if row.State != "OPERATION_STATE_EXECUTED" {
		t.Errorf("state = %q, want OPERATION_STATE_EXECUTED", row.State)
	}
	if row.FIGI != "BBG000B9XRY5" {
		t.Errorf("figi = %q, want BBG000B9XRY5", row.FIGI)
	}
	if row.PositionUID != "pos-after" {
		t.Errorf("position uid = %q, want pos-after", row.PositionUID)
	}
	if row.AssetUID != "asset-after" {
		t.Errorf("asset uid = %q, want asset-after", row.AssetUID)
	}
	if row.InstrumentType != "bond" {
		t.Errorf("instrument type = %q, want bond", row.InstrumentType)
	}
	if row.Price == nil || row.Price.String() != "1600.25" {
		t.Errorf("price = %v, want 1600.25", row.Price)
	}
	if row.Commission == nil || row.Commission.String() != "-9.9" {
		t.Errorf("commission = %v, want -9.9", row.Commission)
	}
	if row.CommissionCurrency != "USD" {
		t.Errorf("commission currency = %q, want USD", row.CommissionCurrency)
	}
	if row.AccruedInt == nil || row.AccruedInt.String() != "4.75" {
		t.Errorf("accrued interest = %v, want 4.75", row.AccruedInt)
	}
	if row.Description != "Покупка ценных бумаг" {
		t.Errorf("description = %q, want Покупка ценных бумаг", row.Description)
	}
	var raw map[string]string
	if err := json.Unmarshal(row.Raw, &raw); err != nil {
		t.Fatalf("raw did not come back as a document: %v (%s)", err, row.Raw)
	}
	if raw["id"] != "op-after" {
		t.Errorf("raw = %v, want the document the broker sent this time", raw)
	}
	if !row.LastConfirmedAt.Equal(second) {
		t.Errorf("confirmed %s, want %s", row.LastConfirmedAt, second)
	}

	// And what a refresh may not touch.
	if row.ID != original.ID {
		t.Errorf("the row's id changed (%s -> %s); the journal points at it", original.ID, row.ID)
	}
	if !row.FirstSeenAt.Equal(first) {
		t.Errorf("first seen %s, want %s — it was first seen on the first run", row.FirstSeenAt, first)
	}
	if row.ContentKey != original.ContentKey {
		t.Errorf("content key = %q, want %q", row.ContentKey, original.ContentKey)
	}
	if row.UnparsedReason != "unknown_type" {
		t.Errorf("unparsed reason = %q, want unknown_type — a confirmation does not decide it", row.UnparsedReason)
	}
	if row.InstrumentUID != "uid-1" {
		t.Errorf("instrument uid = %q, want uid-1", row.InstrumentUID)
	}
	if row.Currency != "RUB" || row.Payment.String() != "-15230.5" || row.Quantity != 10 {
		t.Errorf("a field of the key moved: %q / %s / %d", row.Currency, row.Payment, row.Quantity)
	}
}

// The broker returns an operation before it has settled and again once it
// has. If the mirror kept the state it first saw, that operation would read
// "in progress" for good and never reach the journal at all, since the
// projection takes only executed ones.
func TestSyncMirrorFollowsAnOperationOutOfProgressIntoExecuted(t *testing.T) {
	f := newFixture(t)
	first := wireTime(t, "2026-03-16T00:00:00Z")
	second := wireTime(t, "2026-03-16T01:00:00Z")

	inProgress := op("op-1", "OPERATION_TYPE_BUY", "uid-1", wireTime(t, "2026-03-14T07:30:15Z"), "RUB", -15230, -500000000, 10)
	inProgress.State = "OPERATION_STATE_PROGRESS"
	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{inProgress}, first); err != nil {
		t.Fatalf("first SyncMirror: %v", err)
	}
	if got := rowsByPayment(t, f)["-15230.5"]; got.State != "OPERATION_STATE_PROGRESS" {
		t.Fatalf("the operation was mirrored as %q, so its settling cannot be tested", got.State)
	}

	executed := inProgress
	executed.State = "OPERATION_STATE_EXECUTED"
	stats, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{executed}, second)
	if err != nil {
		t.Fatalf("second SyncMirror: %v", err)
	}
	if want := (MirrorSyncStats{Read: 1, Added: 0, Disappeared: 0}); stats != want {
		t.Fatalf("stats = %+v, want %+v — settling is not a new operation", stats, want)
	}
	if got := rowsByPayment(t, f)["-15230.5"]; got.State != "OPERATION_STATE_EXECUTED" {
		t.Errorf("state = %q, want OPERATION_STATE_EXECUTED", got.State)
	}
}

// raw promises what the broker actually sent. The broker's identifier is
// refreshed on every confirmation, so leaving raw at the first document seen
// would put the two side by side saying different things about one row.
func TestSyncMirrorRefreshesRawSoItCannotContradictTheIDBesideIt(t *testing.T) {
	f := newFixture(t)
	first := wireTime(t, "2026-03-16T00:00:00Z")
	second := wireTime(t, "2026-03-16T01:00:00Z")
	at := wireTime(t, "2026-03-14T07:30:15Z")

	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link,
		[]OperationItem{op("id-before", "OPERATION_TYPE_BUY", "uid-1", at, "RUB", -100, 0, 1)}, first); err != nil {
		t.Fatalf("first SyncMirror: %v", err)
	}
	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link,
		[]OperationItem{op("id-after", "OPERATION_TYPE_BUY", "uid-1", at, "RUB", -100, 0, 1)}, second); err != nil {
		t.Fatalf("second SyncMirror: %v", err)
	}

	row := rowsByPayment(t, f)["-100"]
	var raw map[string]string
	if err := json.Unmarshal(row.Raw, &raw); err != nil {
		t.Fatalf("raw did not come back as a document: %v (%s)", err, row.Raw)
	}
	if row.BrokerOperationID != "id-after" {
		t.Fatalf("broker id = %q, want id-after", row.BrokerOperationID)
	}
	if raw["id"] != "id-after" {
		t.Errorf("raw says the operation is %q while the column beside it says %q",
			raw["id"], row.BrokerOperationID)
	}
}

func TestSyncMirrorMarksAMissingOperationWithoutDeletingIt(t *testing.T) {
	f := newFixture(t)
	first := wireTime(t, "2026-03-16T00:00:00Z")
	second := wireTime(t, "2026-03-16T01:00:00Z")
	kept := op("op-1", "OPERATION_TYPE_BUY", "uid-1", wireTime(t, "2026-03-14T07:30:15Z"), "RUB", -15230, -500000000, 10)
	lost := op("op-2", "OPERATION_TYPE_SELL", "uid-1", wireTime(t, "2026-03-15T10:00:00Z"), "RUB", 16000, 0, 10)

	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{kept, lost}, first); err != nil {
		t.Fatalf("first SyncMirror: %v", err)
	}
	stats, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{kept}, second)
	if err != nil {
		t.Fatalf("second SyncMirror: %v", err)
	}
	if want := (MirrorSyncStats{Read: 1, Added: 0, Disappeared: 1}); stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}
	if n := mirrorCount(t, f); n != 2 {
		t.Fatalf("mirror holds %d rows, want 2 — a row that stopped being returned is marked, never removed", n)
	}
	byPayment := rowsByPayment(t, f)
	if got := byPayment["16000"]; got.DisappearedAt == nil || !got.DisappearedAt.Equal(second) {
		t.Errorf("the missing operation is marked %v, want %s", got.DisappearedAt, second)
	}
	if got := byPayment["-15230.5"]; got.DisappearedAt != nil {
		t.Errorf("the operation the broker still returns was marked gone at %s", got.DisappearedAt)
	}
}

func TestSyncMirrorClearsTheMarkWhenTheBrokerReturnsTheOperation(t *testing.T) {
	f := newFixture(t)
	first := wireTime(t, "2026-03-16T00:00:00Z")
	second := wireTime(t, "2026-03-16T01:00:00Z")
	third := wireTime(t, "2026-03-16T02:00:00Z")
	item := op("op-1", "OPERATION_TYPE_BUY", "uid-1", wireTime(t, "2026-03-14T07:30:15Z"), "RUB", -15230, -500000000, 10)

	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{item}, first); err != nil {
		t.Fatalf("first SyncMirror: %v", err)
	}
	original := rowsByPayment(t, f)["-15230.5"].ID

	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, nil, second); err != nil {
		t.Fatalf("second SyncMirror: %v", err)
	}
	if got := rowsByPayment(t, f)["-15230.5"]; got.DisappearedAt == nil {
		t.Fatalf("the operation was not marked gone, so the return cannot be tested")
	}

	stats, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{item}, third)
	if err != nil {
		t.Fatalf("third SyncMirror: %v", err)
	}
	if want := (MirrorSyncStats{Read: 1, Added: 0, Disappeared: 0}); stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}
	if n := mirrorCount(t, f); n != 1 {
		t.Fatalf("mirror holds %d rows after the operation came back, want 1", n)
	}
	back := rowsByPayment(t, f)["-15230.5"]
	if back.ID != original {
		t.Errorf("the return created a new row (%s -> %s) instead of clearing the mark", original, back.ID)
	}
	if back.DisappearedAt != nil {
		t.Errorf("the mark was left on at %s", back.DisappearedAt)
	}
	if !back.LastConfirmedAt.Equal(third) {
		t.Errorf("confirmed %s, want %s", back.LastConfirmedAt, third)
	}
}

// A run that finds an operation still missing must not restamp it: the mark
// says when the broker stopped returning it, and that moment does not move.
func TestSyncMirrorDoesNotRestampAnAlreadyMissingOperation(t *testing.T) {
	f := newFixture(t)
	first := wireTime(t, "2026-03-16T00:00:00Z")
	second := wireTime(t, "2026-03-16T01:00:00Z")
	third := wireTime(t, "2026-03-16T02:00:00Z")
	item := op("op-1", "OPERATION_TYPE_BUY", "uid-1", wireTime(t, "2026-03-14T07:30:15Z"), "RUB", -15230, -500000000, 10)

	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{item}, first); err != nil {
		t.Fatalf("first SyncMirror: %v", err)
	}
	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, nil, second); err != nil {
		t.Fatalf("second SyncMirror: %v", err)
	}
	stats, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, nil, third)
	if err != nil {
		t.Fatalf("third SyncMirror: %v", err)
	}
	if want := (MirrorSyncStats{Read: 0, Added: 0, Disappeared: 0}); stats != want {
		t.Errorf("stats = %+v, want %+v — it was already gone before this run", stats, want)
	}
	if got := rowsByPayment(t, f)["-15230.5"]; got.DisappearedAt == nil || !got.DisappearedAt.Equal(second) {
		t.Errorf("the mark moved to %v, want it left at %s", got.DisappearedAt, second)
	}
}

// Two top-ups of the same size in the same second are two operations. The
// mirror is a multiset, so both get a row of their own.
func TestSyncMirrorGivesTwoIdenticalOperationsARowEach(t *testing.T) {
	f := newFixture(t)
	now := wireTime(t, "2026-03-16T00:00:00Z")
	at := wireTime(t, "2026-01-09T05:00:00Z")

	stats, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{
		op("op-1", "OPERATION_TYPE_INPUT", "", at, "RUB", 50000, 0, 0),
		op("op-2", "OPERATION_TYPE_INPUT", "", at, "RUB", 50000, 0, 0),
	}, now)
	if err != nil {
		t.Fatalf("SyncMirror: %v", err)
	}
	if want := (MirrorSyncStats{Read: 2, Added: 2, Disappeared: 0}); stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}
	if n := mirrorCount(t, f); n != 2 {
		t.Fatalf("mirror holds %d rows, want 2 — two lawful twins are two operations", n)
	}
}

// The multiset is compared with multiplicity: what is added is the
// difference, not the whole bucket and not nothing.
func TestSyncMirrorAddsOnlyTheDifferenceInMultiplicity(t *testing.T) {
	f := newFixture(t)
	first := wireTime(t, "2026-03-16T00:00:00Z")
	second := wireTime(t, "2026-03-16T01:00:00Z")
	third := wireTime(t, "2026-03-16T02:00:00Z")
	at := wireTime(t, "2026-01-09T05:00:00Z")
	twin := func(id string) OperationItem {
		return op(id, "OPERATION_TYPE_INPUT", "", at, "RUB", 50000, 0, 0)
	}

	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link,
		[]OperationItem{twin("op-1"), twin("op-2")}, first); err != nil {
		t.Fatalf("first SyncMirror: %v", err)
	}

	// Three in the fetch against two in the mirror: one is added, not three
	// and not none.
	stats, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link,
		[]OperationItem{twin("op-1"), twin("op-2"), twin("op-3")}, second)
	if err != nil {
		t.Fatalf("second SyncMirror: %v", err)
	}
	if want := (MirrorSyncStats{Read: 3, Added: 1, Disappeared: 0}); stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}
	if n := mirrorCount(t, f); n != 3 {
		t.Fatalf("mirror holds %d rows, want 3", n)
	}

	// One in the fetch against three in the mirror: two are marked, not one
	// and not three.
	stats, err = f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{twin("op-1")}, third)
	if err != nil {
		t.Fatalf("third SyncMirror: %v", err)
	}
	if want := (MirrorSyncStats{Read: 1, Added: 0, Disappeared: 2}); stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}
	rows, err := f.store.MirrorRowsByLink(f.ctx, f.link.ID)
	if err != nil {
		t.Fatalf("MirrorRowsByLink: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("mirror holds %d rows, want 3 — nothing is ever removed", len(rows))
	}
	live := 0
	for _, r := range rows {
		if r.DisappearedAt == nil {
			live++
		}
	}
	if live != 1 {
		t.Errorf("%d rows are still live, want 1", live)
	}
}

// THE MUTATION ANCHOR for contentKey. Strip the payment out of the key and
// the second run below stops seeing a correction: the −200 operation would
// be matched against the stored −100 row and reported as a confirmation
// (Added 0, Disappeared 0) instead of as one row gone and one row new.
func TestSyncMirrorKeepsTwoOperationsThatDifferOnlyInAmountApart(t *testing.T) {
	f := newFixture(t)
	first := wireTime(t, "2026-03-16T00:00:00Z")
	second := wireTime(t, "2026-03-16T01:00:00Z")
	third := wireTime(t, "2026-03-16T02:00:00Z")
	at := wireTime(t, "2026-03-14T07:30:15Z")
	hundred := op("op-100", "OPERATION_TYPE_BUY", "uid-1", at, "RUB", -100, 0, 1)
	twoHundred := op("op-200", "OPERATION_TYPE_BUY", "uid-1", at, "RUB", -200, 0, 1)

	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{hundred}, first); err != nil {
		t.Fatalf("first SyncMirror: %v", err)
	}

	stats, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{twoHundred}, second)
	if err != nil {
		t.Fatalf("second SyncMirror: %v", err)
	}
	if want := (MirrorSyncStats{Read: 1, Added: 1, Disappeared: 1}); stats != want {
		t.Fatalf("stats = %+v, want %+v — a different amount is a different operation", stats, want)
	}
	byPayment := rowsByPayment(t, f)
	if len(byPayment) != 2 {
		t.Fatalf("mirror holds %d rows, want 2 (−100 and −200)", len(byPayment))
	}
	if got := byPayment["-100"]; got.DisappearedAt == nil || got.BrokerOperationID != "op-100" {
		t.Errorf("the −100 row: marked %v, broker id %q; want marked and op-100", got.DisappearedAt, got.BrokerOperationID)
	}
	if got := byPayment["-200"]; got.DisappearedAt != nil || got.BrokerOperationID != "op-200" {
		t.Errorf("the −200 row: marked %v, broker id %q; want live and op-200", got.DisappearedAt, got.BrokerOperationID)
	}

	// And when the broker returns both, both live: two rows, not one.
	stats, err = f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{hundred, twoHundred}, third)
	if err != nil {
		t.Fatalf("third SyncMirror: %v", err)
	}
	if want := (MirrorSyncStats{Read: 2, Added: 0, Disappeared: 0}); stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}
	byPayment = rowsByPayment(t, f)
	if len(byPayment) != 2 {
		t.Fatalf("mirror holds %d rows, want 2", len(byPayment))
	}
	for amount, r := range byPayment {
		if r.DisappearedAt != nil {
			t.Errorf("%s is still marked gone at %s", amount, r.DisappearedAt)
		}
	}
}

// The broker's own docs warn that a page can repeat rows. Within one read the
// identifier is stable, so a repeat is caught by it — and a repeat is not a
// second operation.
func TestSyncMirrorCollapsesTheBrokersOwnDuplicateRow(t *testing.T) {
	f := newFixture(t)
	now := wireTime(t, "2026-03-16T00:00:00Z")
	at := wireTime(t, "2026-01-09T05:00:00Z")

	stats, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{
		op("op-1", "OPERATION_TYPE_INPUT", "", at, "RUB", 50000, 0, 0),
		op("op-2", "OPERATION_TYPE_INPUT", "", at, "RUB", 60000, 0, 0),
		op("op-1", "OPERATION_TYPE_INPUT", "", at, "RUB", 50000, 0, 0),
	}, now)
	if err != nil {
		t.Fatalf("SyncMirror: %v", err)
	}
	if want := (MirrorSyncStats{Read: 2, Added: 2, Disappeared: 0}); stats != want {
		t.Errorf("stats = %+v, want %+v — the repeated row is not an operation", stats, want)
	}
	if n := mirrorCount(t, f); n != 2 {
		t.Fatalf("mirror holds %d rows, want 2", n)
	}
}

// When a bucket holds both a live row and a marked one, the fetch confirms
// the live one. Matching the marked one instead would raise a row from the
// dead and bury a live one on every single run, forever.
func TestSyncMirrorConfirmsTheLiveRowBeforeTheMarkedOne(t *testing.T) {
	f := newFixture(t)
	now := wireTime(t, "2026-03-16T02:00:00Z")
	marked := wireTime(t, "2026-03-16T01:00:00Z")
	item := op("op-1", "OPERATION_TYPE_INPUT", "", wireTime(t, "2026-01-09T05:00:00Z"), "RUB", 50000, 0, 0)
	key := contentKey(item)

	// Two rows on one key, and the MARKED one is the older: nothing this
	// package writes can arrange that (a bucket is consumed oldest first),
	// so it is arranged directly.
	old := f.insertMirrorRow(t, key, wireTime(t, "2026-03-16T00:00:00Z"), &marked)
	young := f.insertMirrorRow(t, key, wireTime(t, "2026-03-16T00:30:00Z"), nil)

	stats, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{item}, now)
	if err != nil {
		t.Fatalf("SyncMirror: %v", err)
	}
	if want := (MirrorSyncStats{Read: 1, Added: 0, Disappeared: 0}); stats != want {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}

	rows, err := f.store.MirrorRowsByLink(f.ctx, f.link.ID)
	if err != nil {
		t.Fatalf("MirrorRowsByLink: %v", err)
	}
	byID := map[uuid.UUID]MirrorRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if got := byID[young]; got.DisappearedAt != nil || !got.LastConfirmedAt.Equal(now) {
		t.Errorf("the live row: marked %v, confirmed %s; want live and confirmed %s",
			got.DisappearedAt, got.LastConfirmedAt, now)
	}
	if got := byID[old]; got.DisappearedAt == nil || !got.DisappearedAt.Equal(marked) {
		t.Errorf("the marked row came back to life: marked %v, want it left at %s", got.DisappearedAt, marked)
	}
}

// A sync speaks for one link. Another link's rows are not part of the
// comparison and must not be marked gone by it.
func TestSyncMirrorLeavesAnotherLinksRowsAlone(t *testing.T) {
	f := newFixture(t)
	other := f.secondLink(t)
	first := wireTime(t, "2026-03-16T00:00:00Z")
	second := wireTime(t, "2026-03-16T01:00:00Z")
	item := op("op-1", "OPERATION_TYPE_BUY", "uid-1", wireTime(t, "2026-03-14T07:30:15Z"), "RUB", -15230, -500000000, 10)

	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, other, []OperationItem{item}, first); err != nil {
		t.Fatalf("SyncMirror on the other link: %v", err)
	}
	stats, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, nil, second)
	if err != nil {
		t.Fatalf("SyncMirror on the empty link: %v", err)
	}
	if want := (MirrorSyncStats{}); stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}

	rows, err := f.store.MirrorRowsByLink(f.ctx, other.ID)
	if err != nil {
		t.Fatalf("MirrorRowsByLink: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the other link holds %d rows, want 1", len(rows))
	}
	if rows[0].DisappearedAt != nil {
		t.Errorf("the other link's row was marked gone at %s by a sync that never read it", rows[0].DisappearedAt)
	}
}

// A run is all or nothing. The failing row below is rejected by PostgreSQL
// itself — its description carries a byte sequence that is not valid UTF-8 —
// so every statement of the run that came before it really did execute on
// the server, and what the assertions see is a rollback rather than a run
// that never started.
func TestSyncMirrorRollsBackEverythingWhenOneRowCannotBeWritten(t *testing.T) {
	f := newFixture(t)
	first := wireTime(t, "2026-03-16T00:00:00Z")
	second := wireTime(t, "2026-03-16T01:00:00Z")
	kept := op("op-1", "OPERATION_TYPE_BUY", "uid-1", wireTime(t, "2026-03-14T07:30:15Z"), "RUB", -15230, -500000000, 10)
	lost := op("op-2", "OPERATION_TYPE_SELL", "uid-1", wireTime(t, "2026-03-15T10:00:00Z"), "RUB", 16000, 0, 10)

	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{kept, lost}, first); err != nil {
		t.Fatalf("first SyncMirror: %v", err)
	}

	broken := op("op-3", "OPERATION_TYPE_BUY", "uid-1", wireTime(t, "2026-03-16T00:30:00Z"), "RUB", -900, 0, 1)
	broken.Description = "\xff\xfe"

	// kept is confirmed, lost disappears, broken is inserted — and the
	// insert is refused.
	_, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, []OperationItem{kept, broken}, second)
	if err == nil {
		t.Fatalf("SyncMirror accepted a row PostgreSQL cannot store")
	}
	// The refusal has to come from the server for this test to mean what it
	// says: only then did the confirmation and the mark really execute before
	// it, and only then is what the assertions below see a rollback.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("the row was refused before it reached PostgreSQL (%v); this test would then prove nothing", err)
	}

	if n := mirrorCount(t, f); n != 2 {
		t.Fatalf("mirror holds %d rows after the failed run, want 2", n)
	}
	byPayment := rowsByPayment(t, f)
	if got := byPayment["-15230.5"]; !got.LastConfirmedAt.Equal(first) {
		t.Errorf("the confirmation stuck at %s, want it rolled back to %s", got.LastConfirmedAt, first)
	}
	if got := byPayment["16000"]; got.DisappearedAt != nil {
		t.Errorf("the mark stuck at %s, want it rolled back", got.DisappearedAt)
	}
}

// The comparison and the writes sit behind a lock on the connection row,
// taken as the transaction's first statement. Two runs of one connection have
// to serialize on it: without it both would compute their difference against
// the same stored state and both would insert the same new rows.
//
// The lock is held HERE, by the test itself, so that the waiting is observed
// rather than raced for.
//
// FOR NO KEY UPDATE, and not the FOR UPDATE the sync takes, is what makes
// this test mean anything. Inserting a mirror row makes PostgreSQL check the
// row's foreign key back to the connection, and that check takes FOR KEY
// SHARE on this very row — which FOR UPDATE blocks. Held that way, the sync
// would wait here whether or not it locked anything itself, and the test
// would pass with the lock taken out. FOR NO KEY UPDATE conflicts with FOR
// UPDATE and not with FOR KEY SHARE, so what is being waited on is the sync's
// own lock and nothing else. (Checked: with the FOR UPDATE removed from
// SyncMirror this test goes red, and it did not before this note was written.)
func TestSyncMirrorWaitsForAnotherRunOfTheSameConnection(t *testing.T) {
	f := newFixture(t)
	now := wireTime(t, "2026-03-16T00:00:00Z")
	item := op("op-1", "OPERATION_TYPE_BUY", "uid-1", wireTime(t, "2026-03-14T07:30:15Z"), "RUB", -100, 0, 1)

	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	var locked uuid.UUID
	if err := tx.QueryRow(f.ctx,
		`SELECT id FROM tinvest_connections WHERE id = $1 FOR NO KEY UPDATE`, f.conn.ID).Scan(&locked); err != nil {
		t.Fatalf("take the connection lock: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := f.store.SyncMirror(context.Background(), f.conn.ID, f.link, []OperationItem{item}, now)
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("SyncMirror ran to the end while another transaction held the connection (err %v)", err)
	case <-time.After(300 * time.Millisecond):
	}

	if err := tx.Commit(f.ctx); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SyncMirror once the lock was released: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("SyncMirror never proceeded after the lock was released")
	}
	if n := mirrorCount(t, f); n != 1 {
		t.Errorf("mirror holds %d rows, want 1", n)
	}
}

// waitUntilBlocked waits until n backends of this test's own database are
// stopped on a lock. Every test database is private to its test, so nothing
// else can be counted here by mistake.
//
// It exists so that "both runs have reached the gate" is observed rather than
// assumed: a sleep long enough on this machine today is too short on a loaded
// one tomorrow, and a run that had not yet reached the gate would let the
// wrong order through green.
func (f fixture) waitUntilBlocked(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var blocked int
		if err := f.pool.QueryRow(f.ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&blocked); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if blocked >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d runs are waiting on the lock", blocked, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// THE LOCK IS TAKEN BEFORE THE MIRROR IS READ, and this is the test that
// tells that order apart from the other one. The test above proves only that
// a second run WAITS, which it does either way — move the lock down past the
// read and it stays green. What the order decides is whether a run that
// waited then acts on what it read BEFORE it waited.
//
// PostgreSQL reads at READ COMMITTED here, so each statement takes its own
// snapshot. With the read first, both runs see the empty mirror while the
// gate is shut and each then inserts the operation it believes is new: two
// rows for one operation, on every pair of runs that overlap. With the lock
// first, neither has read anything while it waits, so the second run reads
// the mirror the first one left behind and confirms that row instead.
//
// The gate is a lock the test itself holds, so that both runs are demonstrably
// inside SyncMirror and stopped at the same statement before either is let on.
// FOR NO KEY UPDATE for the reason the test above spells out: it conflicts
// with the sync's FOR UPDATE and not with the FOR KEY SHARE that an insert's
// foreign-key check takes.
func TestSyncMirrorTakesTheLockBeforeItReadsTheMirror(t *testing.T) {
	f := newFixture(t)
	item := op("op-1", "OPERATION_TYPE_BUY", "uid-1", wireTime(t, "2026-03-14T07:30:15Z"), "RUB", -100, 0, 1)

	gate, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = gate.Rollback(f.ctx) }()
	var locked uuid.UUID
	if err := gate.QueryRow(f.ctx,
		`SELECT id FROM tinvest_connections WHERE id = $1 FOR NO KEY UPDATE`, f.conn.ID).Scan(&locked); err != nil {
		t.Fatalf("shut the gate: %v", err)
	}

	done := make(chan error, 2)
	for _, now := range []time.Time{
		wireTime(t, "2026-03-16T00:00:00Z"),
		wireTime(t, "2026-03-16T00:00:01Z"),
	} {
		go func(now time.Time) {
			_, err := f.store.SyncMirror(context.Background(), f.conn.ID, f.link, []OperationItem{item}, now)
			done <- err
		}(now)
	}
	f.waitUntilBlocked(t, 2)

	if err := gate.Commit(f.ctx); err != nil {
		t.Fatalf("open the gate: %v", err)
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("SyncMirror once the gate opened: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("a run never finished after the gate opened")
		}
	}

	if n := mirrorCount(t, f); n != 1 {
		t.Errorf("mirror holds %d rows after two simultaneous runs of one operation, want 1", n)
	}
}

func TestSyncMirrorRefusesALinkFromAnotherConnection(t *testing.T) {
	f := newFixture(t)
	stranger := f.link
	stranger.ConnectionID = uuid.New()

	_, err := f.store.SyncMirror(f.ctx, f.conn.ID, stranger, nil, wireTime(t, "2026-03-16T00:00:00Z"))
	if !errors.Is(err, ErrLinkNotInConnection) {
		t.Errorf("SyncMirror error = %v, want ErrLinkNotInConnection", err)
	}
}

func TestSyncMirrorRefusesAConnectionThatIsNotThere(t *testing.T) {
	f := newFixture(t)
	gone := uuid.New()
	link := f.link
	link.ConnectionID = gone

	_, err := f.store.SyncMirror(f.ctx, gone, link, nil, wireTime(t, "2026-03-16T00:00:00Z"))
	if !errors.Is(err, ErrConnectionNotFound) {
		t.Errorf("SyncMirror error = %v, want ErrConnectionNotFound", err)
	}
}
