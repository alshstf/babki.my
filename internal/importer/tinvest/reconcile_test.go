package tinvest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

// The two narrow interfaces the Reconciler takes must stay the real stores'
// own methods rather than a shape that drifted from them: a signature moving
// under either is a compile error here, not a wiring failure inside task 10's
// worker.
var (
	_ balanceMarker = (*account.Store)(nil)
	_ engineReader  = (*operation.Store)(nil)
)

const (
	portfolioPath = "/tinkoff.public.invest.api.contract.v1.OperationsService/GetPortfolio"
	positionsPath = "/tinkoff.public.invest.api.contract.v1.OperationsService/GetPositions"
)

// -------------------------------------------------------------------------
// test fixtures: the journal side
// -------------------------------------------------------------------------

// aBuy is one purchase in the journal: qty units costing amountMinor (which
// is negative, money leaving) plus feeMinor charged on top.
func aBuy(instrumentID uuid.UUID, qty string, amountMinor, feeMinor int64, currency string) operation.Operation {
	q := decimal.RequireFromString(qty)
	id := instrumentID
	return operation.Operation{
		AccountID:    uuid.Nil,
		InstrumentID: &id,
		Type:         operation.TypeBuy,
		OccurredOn:   time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC),
		Quantity:     &q,
		AmountMinor:  amountMinor,
		Currency:     currency,
		FeeMinor:     feeMinor,
	}
}

// aSell is one disposal: qty units bringing in amountMinor (positive).
func aSell(instrumentID uuid.UUID, qty string, amountMinor, feeMinor int64, currency string) operation.Operation {
	o := aBuy(instrumentID, qty, amountMinor, feeMinor, currency)
	o.Type = operation.TypeSell
	o.OccurredOn = time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC)
	return o
}

// aCashEntry is one journal entry whose whole content is money.
func aCashEntry(t operation.Type, amountMinor int64, currency string) operation.Operation {
	return operation.Operation{
		Type:        t,
		OccurredOn:  time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		AmountMinor: amountMinor,
		Currency:    currency,
	}
}

// aTransferIn is a parcel of shares arriving from another account of the
// owner's, carrying the cost basis that travelled with it.
func aTransferIn(instrumentID uuid.UUID, qty string, basisMinor int64, currency string) operation.Operation {
	o := aBuy(instrumentID, qty, basisMinor, 0, currency)
	o.Type = operation.TypeTransferIn
	o.OccurredOn = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	return o
}

func rub(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// -------------------------------------------------------------------------
// CompareHoldings: the securities side
// -------------------------------------------------------------------------

func TestCompareHoldingsSaysMatchedWhenBothSidesAgree(t *testing.T) {
	inst := uuid.New()
	res := CompareHoldings(
		[]PortfolioPosition{{InstrumentUID: "uid-sber", Quantity: Quotation{Units: 100}}},
		[]MoneyBalance{{Currency: "RUB", Value: rub("8999.90")}},
		[]operation.Operation{
			aCashEntry(operation.TypeDeposit, 1_000_000, "RUB"),
			aBuy(inst, "100", -100_000, 10, "RUB"),
		},
		map[string]uuid.UUID{"uid-sber": inst},
		map[uuid.UUID]string{inst: "SBER"},
	)

	if res.Status != ReconcileMatched {
		t.Fatalf("status = %q, want %q; mismatches: %+v", res.Status, ReconcileMatched, res.Mismatches)
	}
	if len(res.Mismatches) != 0 {
		t.Errorf("mismatches = %+v, want none", res.Mismatches)
	}
}

// TestABlockedPositionCountsAsItsWholeQuantity pins the first half of the
// asymmetry this whole comparison turns on: the broker's Blocked on a
// SECURITY is a boolean ("halted at the depository"), not a count, and
// Quantity is already the whole position. A comparison that treated the flag
// as a number to add would overstate every halted holding by whatever it
// coerced the flag into — and this owner does hold frozen paper, so a halted
// position is not an exotic case for him.
func TestABlockedPositionCountsAsItsWholeQuantity(t *testing.T) {
	inst := uuid.New()
	res := CompareHoldings(
		[]PortfolioPosition{{InstrumentUID: "uid-frozen", Quantity: Quotation{Units: 10}, Blocked: true}},
		nil,
		// The deposit pays for the purchase exactly, so the cash side of the
		// comparison agrees at zero and anything reported here is about the
		// security.
		[]operation.Operation{
			aCashEntry(operation.TypeDeposit, 10_000, "RUB"),
			aBuy(inst, "10", -10_000, 0, "RUB"),
		},
		map[string]uuid.UUID{"uid-frozen": inst},
		map[uuid.UUID]string{inst: "FXUS"},
	)

	if res.Status != ReconcileMatched {
		t.Fatalf("status = %q, want %q — 10 held against 10 reported, with the halt flag set: %+v",
			res.Status, ReconcileMatched, res.Mismatches)
	}
}

func TestAQuantityMismatchCarriesBothFigures(t *testing.T) {
	inst := uuid.New()
	res := CompareHoldings(
		[]PortfolioPosition{{InstrumentUID: "uid-sber", Quantity: Quotation{Units: 100}}},
		nil,
		[]operation.Operation{
			aCashEntry(operation.TypeDeposit, 90_000, "RUB"),
			aBuy(inst, "90", -90_000, 0, "RUB"),
		},
		map[string]uuid.UUID{"uid-sber": inst},
		map[uuid.UUID]string{inst: "SBER"},
	)

	if res.Status != ReconcileMismatched {
		t.Fatalf("status = %q, want %q", res.Status, ReconcileMismatched)
	}
	if len(res.Mismatches) != 1 {
		t.Fatalf("mismatches = %+v, want exactly one", res.Mismatches)
	}
	m := res.Mismatches[0]
	if m.Kind != MismatchInstrument {
		t.Errorf("kind = %q, want %q", m.Kind, MismatchInstrument)
	}
	if m.InstrumentID == nil || *m.InstrumentID != inst {
		t.Errorf("instrument = %v, want %v", m.InstrumentID, inst)
	}
	if m.Label != "SBER" {
		t.Errorf("label = %q, want %q", m.Label, "SBER")
	}
	if !m.Broker.Equal(decimal.NewFromInt(100)) {
		t.Errorf("broker = %s, want 100", m.Broker)
	}
	if !m.Journal.Equal(decimal.NewFromInt(90)) {
		t.Errorf("journal = %s, want 90", m.Journal)
	}
}

// TestAnUnmappedBrokerPositionIsAMismatch pins decision 2 of the brief: the
// broker's instruments are matched against ours ONLY through the map the
// resolver has already built, and a position that is not in it is a
// difference with an honest label — never a silent skip. It is also the best
// evidence there is that operations went unparsed.
func TestAnUnmappedBrokerPositionIsAMismatch(t *testing.T) {
	res := CompareHoldings(
		[]PortfolioPosition{{InstrumentUID: "uid-unknown", FIGI: "BBG000000000", Quantity: Quotation{Units: 7}}},
		nil,
		nil,
		map[string]uuid.UUID{},
		map[uuid.UUID]string{},
	)

	if res.Status != ReconcileMismatched {
		t.Fatalf("status = %q, want %q", res.Status, ReconcileMismatched)
	}
	if len(res.Mismatches) != 1 {
		t.Fatalf("mismatches = %+v, want exactly one", res.Mismatches)
	}
	m := res.Mismatches[0]
	if m.InstrumentID != nil {
		t.Errorf("instrument = %v, want none — nothing here is ours to name", m.InstrumentID)
	}
	if m.Label != "uid-unknown" {
		t.Errorf("label = %q, want the broker's own identifier %q", m.Label, "uid-unknown")
	}
	if !m.Broker.Equal(decimal.NewFromInt(7)) || !m.Journal.IsZero() {
		t.Errorf("broker/journal = %s/%s, want 7/0", m.Broker, m.Journal)
	}
}

func TestAPositionTheBrokerDoesNotReportIsAMismatch(t *testing.T) {
	inst := uuid.New()
	res := CompareHoldings(
		nil,
		nil,
		[]operation.Operation{
			aCashEntry(operation.TypeDeposit, 5_000, "RUB"),
			aBuy(inst, "5", -5_000, 0, "RUB"),
		},
		map[string]uuid.UUID{"uid-sber": inst},
		map[uuid.UUID]string{inst: "SBER"},
	)

	if len(res.Mismatches) != 1 {
		t.Fatalf("mismatches = %+v, want exactly one", res.Mismatches)
	}
	m := res.Mismatches[0]
	if !m.Broker.IsZero() || !m.Journal.Equal(decimal.NewFromInt(5)) {
		t.Errorf("broker/journal = %s/%s, want 0/5", m.Broker, m.Journal)
	}
	if m.Label != "SBER" {
		t.Errorf("label = %q, want %q", m.Label, "SBER")
	}
}

// TestAClosedPositionIsNotAMismatch: the engine keeps a position that was
// sold out to the last unit, with a quantity of zero. The broker does not
// report such a thing at all, and calling the difference between "nothing"
// and "zero" a difference would put a permanent false alarm on the screen of
// anyone who has ever closed a trade.
func TestAClosedPositionIsNotAMismatch(t *testing.T) {
	inst := uuid.New()
	res := CompareHoldings(
		nil,
		// What the sale brought in over what the purchase cost, which is the
		// 10 ₽ the broker still holds.
		[]MoneyBalance{{Currency: "RUB", Value: rub("10.00")}},
		[]operation.Operation{
			aBuy(inst, "10", -10_000, 0, "RUB"),
			aSell(inst, "10", 11_000, 0, "RUB"),
		},
		map[string]uuid.UUID{"uid-sber": inst},
		map[uuid.UUID]string{inst: "SBER"},
	)

	if res.Status != ReconcileMatched {
		t.Fatalf("status = %q, want %q: %+v", res.Status, ReconcileMatched, res.Mismatches)
	}
}

// TestAnInstrumentWithoutALabelIsNamedByItsID: a label is for a person to
// read, and having none is no reason to withhold the difference itself.
func TestAnInstrumentWithoutALabelIsNamedByItsID(t *testing.T) {
	inst := uuid.New()
	res := CompareHoldings(
		[]PortfolioPosition{{InstrumentUID: "uid-sber", Quantity: Quotation{Units: 1}}},
		nil,
		nil,
		map[string]uuid.UUID{"uid-sber": inst},
		nil,
	)

	if len(res.Mismatches) != 1 {
		t.Fatalf("mismatches = %+v, want exactly one", res.Mismatches)
	}
	if res.Mismatches[0].Label != inst.String() {
		t.Errorf("label = %q, want %q", res.Mismatches[0].Label, inst)
	}
}

// -------------------------------------------------------------------------
// CompareHoldings: the money side
// -------------------------------------------------------------------------

// TestJournalCashIsAmountsMinusFees pins the formula itself with literals.
// It is a NEW computation — this program has never worked a cash balance out
// of the journal before, because a balance on the accounts screen is a manual
// mark and not a derivation — so there is nothing to check it against but the
// numbers themselves: a deposit of 10 000,00 ₽, a purchase costing 1 000,00 ₽
// and 10 kopecks of commission leave 8 999,90 ₽.
func TestJournalCashIsAmountsMinusFees(t *testing.T) {
	inst := uuid.New()
	got := journalCashMinor([]operation.Operation{
		aCashEntry(operation.TypeDeposit, 1_000_000, "RUB"),
		aBuy(inst, "100", -100_000, 10, "RUB"),
	})

	want := decimal.NewFromInt(899_990)
	if !got["RUB"].Equal(want) {
		t.Errorf("RUB = %s, want %s", got["RUB"], want)
	}
	if len(got) != 1 {
		t.Errorf("currencies = %v, want RUB alone", got)
	}
}

// TestJournalCashIgnoresTransfers: a transfer's amount is the cost basis that
// travelled, not money that moved (see portfolio.Operation). Summing it as
// cash would invent a balance nobody has and put a false difference on the
// screen of every account this importer ever moved shares into — which the
// projection does produce, for moves between the owner's own accounts.
func TestJournalCashIgnoresTransfers(t *testing.T) {
	inst := uuid.New()
	got := journalCashMinor([]operation.Operation{
		aCashEntry(operation.TypeDeposit, 1_000_000, "RUB"),
		aTransferIn(inst, "10", 500_000, "RUB"),
	})

	want := decimal.NewFromInt(1_000_000)
	if !got["RUB"].Equal(want) {
		t.Errorf("RUB = %s, want %s — the transfer's basis is not cash", got["RUB"], want)
	}
}

// TestCashCountsFreeAndBlockedTogether pins the other half of the asymmetry:
// on MONEY the broker's two figures are two addends of one balance, and a
// comparison that read only the free part would report a false difference on
// every account with an order standing.
func TestCashCountsFreeAndBlockedTogether(t *testing.T) {
	res := CompareHoldings(
		nil,
		[]MoneyBalance{{Currency: "RUB", Value: rub("8000.00"), Blocked: rub("999.90")}},
		[]operation.Operation{
			aCashEntry(operation.TypeDeposit, 1_000_000, "RUB"),
			aCashEntry(operation.TypeWithdrawal, -100_010, "RUB"),
		},
		nil, nil,
	)

	if res.Status != ReconcileMatched {
		t.Fatalf("status = %q, want %q: %+v", res.Status, ReconcileMatched, res.Mismatches)
	}
}

func TestACurrencyMismatchCarriesBothFigures(t *testing.T) {
	res := CompareHoldings(
		nil,
		[]MoneyBalance{{Currency: "RUB", Value: rub("9000.00")}},
		[]operation.Operation{aCashEntry(operation.TypeDeposit, 899_990, "RUB")},
		nil, nil,
	)

	if len(res.Mismatches) != 1 {
		t.Fatalf("mismatches = %+v, want exactly one", res.Mismatches)
	}
	m := res.Mismatches[0]
	if m.Kind != MismatchCurrency {
		t.Errorf("kind = %q, want %q", m.Kind, MismatchCurrency)
	}
	if m.Label != "RUB" {
		t.Errorf("label = %q, want %q", m.Label, "RUB")
	}
	if m.InstrumentID != nil {
		t.Errorf("instrument = %v, want none on a currency row", m.InstrumentID)
	}
	if !m.Broker.Equal(rub("9000.00")) {
		t.Errorf("broker = %s, want 9000.00", m.Broker)
	}
	// Both sides are stated in whole currency units, the way the broker
	// states its own: 899 990 kopecks is 8 999,90 ₽.
	if !m.Journal.Equal(rub("8999.90")) {
		t.Errorf("journal = %s, want 8999.90", m.Journal)
	}
}

// TestACurrencyOnlyTheJournalKnowsIsAMismatch: a currency the broker did not
// mention is a currency the broker holds none of — its answer is a complete
// statement of its cash — so ours saying otherwise is a difference and not a
// gap to be passed over.
func TestACurrencyOnlyTheJournalKnowsIsAMismatch(t *testing.T) {
	res := CompareHoldings(
		nil,
		[]MoneyBalance{{Currency: "RUB", Value: rub("0")}},
		[]operation.Operation{aCashEntry(operation.TypeDeposit, 4_200, "USD")},
		nil, nil,
	)

	if len(res.Mismatches) != 1 {
		t.Fatalf("mismatches = %+v, want exactly one", res.Mismatches)
	}
	m := res.Mismatches[0]
	if m.Label != "USD" || !m.Broker.IsZero() || !m.Journal.Equal(rub("42.00")) {
		t.Errorf("mismatch = %+v, want USD 0 against 42.00", m)
	}
}

func TestMismatchesComeOutInAStableOrder(t *testing.T) {
	instA, instB := uuid.New(), uuid.New()
	res := CompareHoldings(
		[]PortfolioPosition{
			{InstrumentUID: "uid-b", Quantity: Quotation{Units: 2}},
			{InstrumentUID: "uid-a", Quantity: Quotation{Units: 1}},
		},
		[]MoneyBalance{
			{Currency: "USD", Value: rub("1.00")},
			{Currency: "RUB", Value: rub("1.00")},
		},
		nil,
		map[string]uuid.UUID{"uid-a": instA, "uid-b": instB},
		map[uuid.UUID]string{instA: "AAA", instB: "BBB"},
	)

	var got []string
	for _, m := range res.Mismatches {
		got = append(got, m.Kind+":"+m.Label)
	}
	want := []string{"currency:RUB", "currency:USD", "instrument:AAA", "instrument:BBB"}
	if len(got) != len(want) {
		t.Fatalf("mismatches = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mismatches = %v, want %v", got, want)
		}
	}
}

// TestAJournalTheEngineRefusesIsNotCheckedRatherThanMatched is the sharpest
// test in this file. When our own side of the comparison cannot be computed
// at all, the answer is "not checked" — the third status, the one that draws
// no tick. Calling it "agrees" would be the exact failure this project has
// been bitten by four times: a true-looking caption over a figure nobody
// established.
func TestAJournalTheEngineRefusesIsNotCheckedRatherThanMatched(t *testing.T) {
	inst := uuid.New()
	journal := []operation.Operation{
		aBuy(inst, "1", -1_000, 0, "RUB"),
		aSell(inst, "10", 11_000, 0, "RUB"), // more than was ever held
	}
	if _, err := portfolio.Compute(journal); err == nil {
		t.Fatal("this journal is supposed to be one the engine refuses; it did not")
	}

	res := CompareHoldings(
		[]PortfolioPosition{{InstrumentUID: "uid-sber", Quantity: Quotation{Units: 1}}},
		nil, journal,
		map[string]uuid.UUID{"uid-sber": inst},
		map[uuid.UUID]string{inst: "SBER"},
	)

	if res.Status != ReconcileNotChecked {
		t.Errorf("status = %q, want %q", res.Status, ReconcileNotChecked)
	}
	if len(res.Mismatches) != 0 {
		t.Errorf("mismatches = %+v, want none: nothing was compared", res.Mismatches)
	}
}

// -------------------------------------------------------------------------
// ReconcileLink
// -------------------------------------------------------------------------

// markedBalance is one call to SetBalance, as the marker saw it.
type markedBalance struct {
	spaceID, accountID uuid.UUID
	asOf               time.Time
	amountMinor        int64
}

type recordingMarker struct {
	marks []markedBalance
	err   error
}

func (m *recordingMarker) SetBalance(_ context.Context, spaceID, accountID uuid.UUID, asOf time.Time, amountMinor int64) error {
	if m.err != nil {
		return m.err
	}
	m.marks = append(m.marks, markedBalance{spaceID, accountID, asOf, amountMinor})
	return nil
}

type fakeJournal struct {
	ops []operation.Operation
	err error
}

func (f fakeJournal) ListForEngine(_ context.Context, _, _ uuid.UUID) ([]operation.Operation, error) {
	return f.ops, f.err
}

// seedMapped puts one instrument in the catalog and maps the broker's uid to
// it, which is the state a reconciliation of a resolved position needs.
func (f fixture) seedMapped(t *testing.T, uid, ticker string) uuid.UUID {
	t.Helper()
	inst, err := instrument.NewStore(f.pool).Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк", Ticker: ticker,
		ISIN: "RU0009029540", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	if err := f.store.saveMap(f.ctx, f.conn.ID, inst.ID,
		InstrumentRef{InstrumentUID: uid, FIGI: "BBG004730N88"}, inst.ISIN, inst.Ticker); err != nil {
		t.Fatalf("seed map: %v", err)
	}
	return inst.ID
}

// TestReconcileLinkMarksTheBalanceWithTheBrokersOwnRubles pins the owner's
// decision of 2026-08-04: an imported account gets its balance mark from the
// figure the BROKER named — free plus blocked rubles — rather than from any
// sum of ours. Nobody is going to type a mark into an imported account by
// hand, and the accounts screen would otherwise show it empty.
func TestReconcileLinkMarksTheBalanceWithTheBrokersOwnRubles(t *testing.T) {
	f := newFixture(t)
	inst := f.seedMapped(t, "uid-sber", "SBER")

	srv, _ := serve(t, map[string]route{
		portfolioPath: {status: http.StatusOK, body: []byte(
			`{"positions":[{"instrumentUid":"uid-sber","figi":"BBG004730N88","instrumentType":"share",` +
				`"quantity":{"units":"100","nano":0},"blocked":false}]}`)},
		positionsPath: {status: http.StatusOK, body: []byte(
			`{"money":[{"currency":"rub","units":"8000","nano":0}],` +
				`"blocked":[{"currency":"rub","units":"999","nano":900000000}]}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)

	marker := &recordingMarker{}
	r := NewReconciler(f.store, fakeJournal{ops: []operation.Operation{
		aCashEntry(operation.TypeDeposit, 1_000_000, "RUB"),
		aBuy(inst, "100", -100_000, 10, "RUB"),
	}}, marker, nil)
	r.now = func() time.Time { return time.Date(2026, 8, 4, 22, 30, 0, 0, time.UTC) }

	res, err := r.ReconcileLink(f.ctx, c, f.conn, f.link)
	if err != nil {
		t.Fatalf("ReconcileLink: %v", err)
	}
	if res.Status != ReconcileMatched {
		t.Fatalf("status = %q, want %q: %+v", res.Status, ReconcileMatched, res.Mismatches)
	}
	if len(marker.marks) != 1 {
		t.Fatalf("marks = %+v, want exactly one", marker.marks)
	}
	got := marker.marks[0]
	// 8 000,00 ₽ free plus 999,90 ₽ blocked, in kopecks.
	if got.amountMinor != 899_990 {
		t.Errorf("amount = %d, want 899990", got.amountMinor)
	}
	if got.spaceID != f.spaceID || got.accountID != f.accountID {
		t.Errorf("mark filed under %s/%s, want %s/%s", got.spaceID, got.accountID, f.spaceID, f.accountID)
	}
	// 22:30 UTC on the 4th is already the 5th in Moscow, and the broker's day
	// is the Moscow one — the same day this importer files its journal
	// entries under.
	want := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	if !got.asOf.Equal(want) {
		t.Errorf("as_of = %s, want %s", got.asOf, want)
	}
}

// TestReconcileLinkSaysNotCheckedWhenThePortfolioIsUnavailable is the other
// half of the same honesty: a broker that did not answer leaves the run
// saying "not checked", with the error going out and NO balance mark written
// — a mark is the broker's own figure, and there is none.
func TestReconcileLinkSaysNotCheckedWhenThePortfolioIsUnavailable(t *testing.T) {
	f := newFixture(t)

	srv, _ := serve(t, map[string]route{
		portfolioPath: {status: http.StatusInternalServerError, body: []byte(`{"message":"boom"}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)

	marker := &recordingMarker{}
	r := NewReconciler(f.store, fakeJournal{}, marker, nil)

	res, err := r.ReconcileLink(f.ctx, c, f.conn, f.link)
	if err == nil {
		t.Fatal("ReconcileLink returned no error though the broker failed")
	}
	if res.Status != ReconcileNotChecked {
		t.Errorf("status = %q, want %q — a broker that did not answer is not an agreement",
			res.Status, ReconcileNotChecked)
	}
	if len(res.Mismatches) != 0 {
		t.Errorf("mismatches = %+v, want none", res.Mismatches)
	}
	if len(marker.marks) != 0 {
		t.Errorf("marks = %+v, want none: the broker named no figure to mark", marker.marks)
	}
}

func TestReconcileLinkSaysNotCheckedWhenTheCashIsUnavailable(t *testing.T) {
	f := newFixture(t)

	srv, _ := serve(t, map[string]route{
		portfolioPath: {status: http.StatusOK, body: []byte(`{"positions":[]}`)},
		positionsPath: {status: http.StatusInternalServerError, body: []byte(`{"message":"boom"}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)

	marker := &recordingMarker{}
	r := NewReconciler(f.store, fakeJournal{}, marker, nil)

	res, err := r.ReconcileLink(f.ctx, c, f.conn, f.link)
	if err == nil {
		t.Fatal("ReconcileLink returned no error though the broker failed")
	}
	if res.Status != ReconcileNotChecked {
		t.Errorf("status = %q, want %q", res.Status, ReconcileNotChecked)
	}
	if len(marker.marks) != 0 {
		t.Errorf("marks = %+v, want none", marker.marks)
	}
}

// TestReconcileLinkMarksTheBalanceEvenWhenTheSidesDisagree: the mark is what
// the BROKER said about itself, and that figure is no less true because our
// journal has not caught up with it.
func TestReconcileLinkMarksTheBalanceEvenWhenTheSidesDisagree(t *testing.T) {
	f := newFixture(t)

	srv, _ := serve(t, map[string]route{
		portfolioPath: {status: http.StatusOK, body: []byte(
			`{"positions":[{"instrumentUid":"uid-nobody-mapped","instrumentType":"share",` +
				`"quantity":{"units":"3","nano":0},"blocked":false}]}`)},
		positionsPath: {status: http.StatusOK, body: []byte(
			`{"money":[{"currency":"rub","units":"1","nano":0}]}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)

	marker := &recordingMarker{}
	r := NewReconciler(f.store, fakeJournal{}, marker, nil)

	res, err := r.ReconcileLink(f.ctx, c, f.conn, f.link)
	if err != nil {
		t.Fatalf("ReconcileLink: %v", err)
	}
	if res.Status != ReconcileMismatched {
		t.Fatalf("status = %q, want %q", res.Status, ReconcileMismatched)
	}
	if len(marker.marks) != 1 || marker.marks[0].amountMinor != 100 {
		t.Errorf("marks = %+v, want one of 100", marker.marks)
	}
}

func TestReconcileLinkRefusesALinkOfAnotherConnection(t *testing.T) {
	f := newFixture(t)
	other := f.link
	other.ConnectionID = uuid.New()

	r := NewReconciler(f.store, fakeJournal{}, &recordingMarker{}, nil)
	res, err := r.ReconcileLink(f.ctx, NewClient(nil, "", "token", nil), f.conn, other)
	if !errors.Is(err, ErrLinkNotInConnection) {
		t.Fatalf("error = %v, want %v", err, ErrLinkNotInConnection)
	}
	if res.Status != ReconcileNotChecked {
		t.Errorf("status = %q, want %q", res.Status, ReconcileNotChecked)
	}
}

// TestReconcileLinkReadsTheMapOfItsOwnConnection: the map is per connection,
// and a reconciliation that read another connection's rows would resolve the
// broker's positions against instruments this account never held.
func TestReconcileLinkReadsTheMapOfItsOwnConnection(t *testing.T) {
	f := newFixture(t)
	inst := f.seedMapped(t, "uid-sber", "SBER")

	otherConn, err := f.store.CreateConnection(f.ctx, f.spaceID, []byte("x"), "0000")
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	if err := f.store.saveMap(f.ctx, otherConn.ID, inst,
		InstrumentRef{InstrumentUID: "uid-elsewhere"}, "RU0009029540", "SBER"); err != nil {
		t.Fatalf("seed other map: %v", err)
	}

	byUID, labels, err := f.store.instrumentMap(f.ctx, f.conn.ID)
	if err != nil {
		t.Fatalf("instrumentMap: %v", err)
	}
	if _, ok := byUID["uid-elsewhere"]; ok {
		t.Errorf("map = %v, want no row of the other connection", byUID)
	}
	if byUID["uid-sber"] != inst {
		t.Errorf("uid-sber = %v, want %v", byUID["uid-sber"], inst)
	}
	if labels[inst] != "SBER" {
		t.Errorf("label = %q, want %q", labels[inst], "SBER")
	}
}

// TestAnInstrumentWithoutATickerIsLabelledByItsName: the catalog's ticker is
// optional (a fund the owner holds abroad may have none), and the name is
// what a person recognizes it by when it does.
func TestAnInstrumentWithoutATickerIsLabelledByItsName(t *testing.T) {
	f := newFixture(t)
	inst, err := instrument.NewStore(f.pool).Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeCustom, Name: "Замороженный пай", Currency: "USD",
	})
	if err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	if err := f.store.saveMap(f.ctx, f.conn.ID, inst.ID,
		InstrumentRef{InstrumentUID: "uid-frozen"}, "", ""); err != nil {
		t.Fatalf("seed map: %v", err)
	}

	_, labels, err := f.store.instrumentMap(f.ctx, f.conn.ID)
	if err != nil {
		t.Fatalf("instrumentMap: %v", err)
	}
	if labels[inst.ID] != "Замороженный пай" {
		t.Errorf("label = %q, want the instrument's name", labels[inst.ID])
	}
}

// TestAMapRowWithoutAnInstrumentUIDAnswersForNothing: an empty key would be
// the answer for every broker position that arrived without an identifier,
// resolving them all to one instrument. The row is written here by hand
// because the resolver refuses to write one (which is exactly why this guard
// has to be checked rather than assumed).
func TestAMapRowWithoutAnInstrumentUIDAnswersForNothing(t *testing.T) {
	f := newFixture(t)
	inst, err := instrument.NewStore(f.pool).Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Без идентификатора", Ticker: "NOUID2", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO tinvest_instrument_map (connection_id, instrument_id, instrument_uid)
		VALUES ($1, $2, '')`, f.conn.ID, inst.ID); err != nil {
		t.Fatalf("seed map row: %v", err)
	}

	byUID, _, err := f.store.instrumentMap(f.ctx, f.conn.ID)
	if err != nil {
		t.Fatalf("instrumentMap: %v", err)
	}
	if _, ok := byUID[""]; ok {
		t.Errorf("map = %v, want no answer under the empty identifier", byUID)
	}
}

// -------------------------------------------------------------------------
// FinishRun: the verdict reaching the run log
// -------------------------------------------------------------------------

func TestFinishRunWritesTheVerdictAndTheMomentItWasReached(t *testing.T) {
	f := newFixture(t)
	run, err := f.store.StartRun(f.ctx, f.conn.ID, f.link.ID, TriggerManual)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	inst := uuid.New()

	err = f.store.FinishRun(f.ctx, run.ID, RunOutcome{
		Status: RunOK,
		Reconcile: ReconcileResult{
			Status: ReconcileMismatched,
			Mismatches: []ReconcileMismatch{{
				Kind: MismatchInstrument, InstrumentID: &inst, Label: "SBER",
				Broker: decimal.NewFromInt(100), Journal: decimal.NewFromInt(90),
			}},
		},
	})
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	runs, _, err := f.store.RunsByConnection(f.ctx, f.conn.ID, 10, 0)
	if err != nil {
		t.Fatalf("RunsByConnection: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	got := runs[0]
	if got.ReconcileStatus != ReconcileMismatched {
		t.Errorf("reconcile status = %q, want %q", got.ReconcileStatus, ReconcileMismatched)
	}
	if got.ReconciledAt == nil {
		t.Error("reconciled_at is null though the run was checked")
	}
	var back []ReconcileMismatch
	if err := json.Unmarshal(got.ReconcileMismatches, &back); err != nil {
		t.Fatalf("decode mismatches %s: %v", got.ReconcileMismatches, err)
	}
	if len(back) != 1 || back[0].Label != "SBER" || !back[0].Broker.Equal(decimal.NewFromInt(100)) ||
		!back[0].Journal.Equal(decimal.NewFromInt(90)) || back[0].InstrumentID == nil || *back[0].InstrumentID != inst {
		t.Errorf("mismatches read back as %+v, want the one written", back)
	}
}

// TestFinishRunLeavesARunItDidNotCheckSayingSo: a run finished by a caller
// that never reconciled anything keeps "not checked" and says nothing about
// when — the zero value of the new field must not be able to write a verdict.
func TestFinishRunLeavesARunItDidNotCheckSayingSo(t *testing.T) {
	f := newFixture(t)
	run, err := f.store.StartRun(f.ctx, f.conn.ID, f.link.ID, TriggerSchedule)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	if err := f.store.FinishRun(f.ctx, run.ID, RunOutcome{Status: RunOK, ReadCount: 3}); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	runs, _, err := f.store.RunsByConnection(f.ctx, f.conn.ID, 10, 0)
	if err != nil {
		t.Fatalf("RunsByConnection: %v", err)
	}
	got := runs[0]
	if got.ReconcileStatus != ReconcileNotChecked {
		t.Errorf("reconcile status = %q, want %q", got.ReconcileStatus, ReconcileNotChecked)
	}
	if got.ReconciledAt != nil {
		t.Errorf("reconciled_at = %v, want null: nothing was checked", got.ReconciledAt)
	}
	if got.ReconcileMismatches != nil {
		t.Errorf("mismatches = %s, want null: nothing was compared", got.ReconcileMismatches)
	}
}

// TestFinishRunWritesAnEmptyListForAnAgreement: "checked and nothing
// differed" is a list of no differences, which is a different statement from
// the null of "never looked".
func TestFinishRunWritesAnEmptyListForAnAgreement(t *testing.T) {
	f := newFixture(t)
	run, err := f.store.StartRun(f.ctx, f.conn.ID, f.link.ID, TriggerSchedule)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = f.store.FinishRun(f.ctx, run.ID, RunOutcome{
		Status:    RunOK,
		Reconcile: ReconcileResult{Status: ReconcileMatched},
	})
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	runs, _, err := f.store.RunsByConnection(f.ctx, f.conn.ID, 10, 0)
	if err != nil {
		t.Fatalf("RunsByConnection: %v", err)
	}
	if string(runs[0].ReconcileMismatches) != "[]" {
		t.Errorf("mismatches = %s, want []", runs[0].ReconcileMismatches)
	}
	if runs[0].ReconciledAt == nil {
		t.Error("reconciled_at is null though the run was checked")
	}
}

// TestFinishRunRefusesAVerdictItsOwnListContradicts: a run saying it found
// differences with nothing to show, or saying it found none while carrying
// some, is the caption-that-lies shape this project keeps being bitten by.
// The database's CHECK constrains the word alone, so the pairing is checked
// here.
func TestFinishRunRefusesAVerdictItsOwnListContradicts(t *testing.T) {
	f := newFixture(t)
	run, err := f.store.StartRun(f.ctx, f.conn.ID, f.link.ID, TriggerSchedule)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	cases := map[string]ReconcileResult{
		"mismatched with nothing to show": {Status: ReconcileMismatched},
		"matched while carrying a difference": {Status: ReconcileMatched, Mismatches: []ReconcileMismatch{{
			Kind: MismatchCurrency, Label: "RUB", Broker: decimal.NewFromInt(1),
		}}},
		"not checked while carrying a difference": {Status: ReconcileNotChecked, Mismatches: []ReconcileMismatch{{
			Kind: MismatchCurrency, Label: "RUB", Broker: decimal.NewFromInt(1),
		}}},
	}
	for name, rec := range cases {
		t.Run(name, func(t *testing.T) {
			err := f.store.FinishRun(f.ctx, run.ID, RunOutcome{Status: RunOK, Reconcile: rec})
			if !errors.Is(err, ErrReconcileVerdictContradictsItself) {
				t.Errorf("error = %v, want %v", err, ErrReconcileVerdictContradictsItself)
			}
		})
	}
}
