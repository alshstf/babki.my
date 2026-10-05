package tinvest

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/logtest"
)

// arrivalOf is the one transfer_in the sync wrote: shares from another broker.
func (f *workerFixture) arrivalOf(t *testing.T) operation.Operation {
	t.Helper()
	var found []operation.Operation
	for _, o := range f.journal(t) {
		if o.Type == operation.TypeTransferIn {
			found = append(found, o)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the journal holds %d arrivals, want 1", len(found))
	}
	return found[0]
}

// brokerSendsForty answers with 40 shares arriving from another broker, or
// with however many the test says the broker reports now.
func (f *workerFixture) brokerSends(t *testing.T, quantity string) {
	t.Helper()
	row := opFixture(t, "input_securities.json")
	row = strings.Replace(row, `"quantity": "40"`, `"quantity": "`+quantity+`"`, 1)
	f.broker.answer(rpcOperations, http.StatusOK, operationsPage(row))
	f.broker.answer(rpcInstrumentB, http.StatusOK, string(readFixture(t, "instrument.json")))
}

// The broker passes on no purchases for shares that came from another broker,
// so a projection made from its record alone would take the owner's statement
// off the journal on every sync. It is put back — and on the very row it was
// stated for, so a sync that changed nothing writes nothing.
func TestAPurchaseStatementSurvivesTheNextSync(t *testing.T) {
	f := newWorkerFixture(t)
	f.brokerSends(t, "40")
	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	arrival := f.arrivalOf(t)
	if arrival.AmountMinor != 0 {
		t.Fatalf("the arrival came with a basis of %d, want nought — the broker passes on none", arrival.AmountMinor)
	}

	price := decimal.RequireFromString("250")
	bought := day(t, "2024-11-05")
	if _, err := operation.NewService(f.ops).StatePurchases(f.ctx, f.spaceID, arrival.ID, []operation.StatedPurchase{
		{Quantity: decimal.RequireFromString("40"), Price: &price, AcquiredOn: &bought},
	}); err != nil {
		t.Fatalf("StatePurchases: %v", err)
	}

	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	after := f.arrivalOf(t)
	if after.ID != arrival.ID {
		t.Errorf("the arrival was rewritten (%s -> %s) by a sync that had nothing new to say", arrival.ID, after.ID)
	}
	if after.AmountMinor != 1_000_000 || len(after.TransferLots) != 1 ||
		after.TransferLots[0].AcquiredOn == nil || !after.TransferLots[0].AcquiredOn.Equal(bought) {
		t.Errorf("after the sync: basis %d, pieces %+v — want 1000000 bought on 2024-11-05", after.AmountMinor, after.TransferLots)
	}
}

// A broker that rewrites the row with another quantity has made a new row of
// it, under a new name: the statement made for the old one does not reach it,
// and the shares count as bought for nothing again — which the paper says —
// until the owner states them anew. (The statement is not carried over: the
// same weakness the explanations of broker rows have, #196.)
func TestAPurchaseStatementDoesNotReachARowTheBrokerRewrote(t *testing.T) {
	f := newWorkerFixture(t)
	f.brokerSends(t, "40")
	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	price := decimal.RequireFromString("250")
	if _, err := operation.NewService(f.ops).StatePurchases(f.ctx, f.spaceID, f.arrivalOf(t).ID, []operation.StatedPurchase{
		{Quantity: decimal.RequireFromString("40"), Price: &price},
	}); err != nil {
		t.Fatalf("StatePurchases: %v", err)
	}

	f.brokerSends(t, "30")
	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	after := f.arrivalOf(t)
	if after.AmountMinor != 0 || len(after.TransferLots) != 0 {
		t.Errorf("after the broker rewrote the row: basis %d, pieces %+v — want nought and none", after.AmountMinor, after.TransferLots)
	}
}

// statedReader is a journal reader with a statement on file and nothing else.
type statedReader struct {
	journalReader
	stated map[operation.StatedKey][]operation.ReleasedLot
}

func (r statedReader) StatedPurchases(context.Context, uuid.UUID, []uuid.UUID, string) (
	map[operation.StatedKey][]operation.ReleasedLot, error,
) {
	return r.stated, nil
}

// The guard for a statement that no longer adds up to the row it names: the
// same row, now carrying another quantity. Applying the pieces would put lots
// on the journal for shares that are not there, so they are left off and the
// log says why.
func TestAPurchaseStatementThatNoLongerAddsUpIsLeftOffAndSaidSo(t *testing.T) {
	logs := &logtest.Capture{}
	account, instrument := uuid.New(), uuid.New()
	name := "row:0"
	stated := map[operation.StatedKey][]operation.ReleasedLot{
		{AccountID: account, ExternalID: name}: {{Quantity: decimal.RequireFromString("40"), CostMinor: 1_000_000}},
	}
	r := &Rebuilder{reader: statedReader{stated: stated}, log: slog.New(logs)}

	qty := decimal.RequireFromString("30")
	want := []desired{{op: operation.Operation{
		AccountID: account, InstrumentID: &instrument, Type: operation.TypeTransferIn,
		Quantity: &qty, ExternalID: &name,
	}}}
	if err := r.applyStatedPurchases(t.Context(), uuid.New(), []uuid.UUID{account}, want); err != nil {
		t.Fatalf("applyStatedPurchases: %v", err)
	}
	if want[0].op.AmountMinor != 0 || len(want[0].op.TransferLots) != 0 {
		t.Errorf("basis %d, pieces %+v — want the statement left off", want[0].op.AmountMinor, want[0].op.TransferLots)
	}
	found := false
	for _, rec := range logs.Records() {
		if rec.Level == slog.LevelWarn && strings.Contains(rec.Message, "no longer add up") {
			found = true
		}
	}
	if !found {
		t.Error("nothing in the log says the statement was left off")
	}

	// And one that does add up is applied.
	qty = decimal.RequireFromString("40")
	if err := r.applyStatedPurchases(t.Context(), uuid.New(), []uuid.UUID{account}, want); err != nil {
		t.Fatalf("applyStatedPurchases: %v", err)
	}
	if want[0].op.AmountMinor != 1_000_000 || len(want[0].op.TransferLots) != 1 {
		t.Errorf("basis %d, pieces %+v — want the statement applied", want[0].op.AmountMinor, want[0].op.TransferLots)
	}
}

// Two statements can add up to the same basis on different days, so it is the
// pieces and not the sum that say whether the journal row is the one the owner
// stated. A row whose pieces drifted from the statement — same total, another
// day — is written again by the next sync.
func TestASyncPutsBackStatedPiecesTheJournalNoLongerHolds(t *testing.T) {
	f := newWorkerFixture(t)
	f.brokerSends(t, "40")
	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	price := decimal.RequireFromString("250")
	bought := day(t, "2024-11-05")
	arrival, err := operation.NewService(f.ops).StatePurchases(f.ctx, f.spaceID, f.arrivalOf(t).ID, []operation.StatedPurchase{
		{Quantity: decimal.RequireFromString("40"), Price: &price, AcquiredOn: &bought},
	})
	if err != nil {
		t.Fatalf("StatePurchases: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE operation_transfer_lots SET acquired_on = '2020-01-01' WHERE operation_id = $1`, arrival.ID); err != nil {
		t.Fatalf("make the row drift: %v", err)
	}

	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	after := f.arrivalOf(t)
	if len(after.TransferLots) != 1 || after.TransferLots[0].AcquiredOn == nil || !after.TransferLots[0].AcquiredOn.Equal(bought) {
		t.Errorf("pieces after the sync = %+v, want the stated purchase of 2024-11-05 back", after.TransferLots)
	}
}
