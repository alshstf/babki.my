package tinvest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

// The Reconciler's narrow interfaces must stay the real stores' methods; a
// drifted signature fails to compile here.
var (
	_ balanceMarker = (*account.Store)(nil)
	_ engineReader  = (*operation.Store)(nil)
)

const (
	portfolioPath = "/tinkoff.public.invest.api.contract.v1.OperationsService/GetPortfolio"
	positionsPath = "/tinkoff.public.invest.api.contract.v1.OperationsService/GetPositions"
	// The check asks the broker what an unmatched position is (see
	// Reconciler.matchByISIN); a stub that does not answer leaves it
	// unmatched, which is usually the case under test anyway.
	instrumentByPath = "/tinkoff.public.invest.api.contract.v1.InstrumentsService/GetInstrumentBy"
)

// Fixtures: the journal side.

// instrumentWithISIN creates a catalog row carrying an ISIN, which is what the
// cross-venue pairing matches on.
func (f fixture) instrumentWithISIN(t *testing.T, ticker, isin string) instrument.Instrument {
	t.Helper()
	inst, err := instrument.NewStore(f.pool).Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: ticker, Ticker: ticker, ISIN: isin, Currency: "USD",
	})
	if err != nil {
		t.Fatalf("create instrument %s: %v", ticker, err)
	}
	return inst
}

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

// byUID is the index of a connection with no drift: everything by
// instrument_uid, the figi side empty.
func byUID(m map[string]uuid.UUID) InstrumentIndex {
	return InstrumentIndex{ByUID: m, ByFIGI: map[string]uuid.UUID{}}
}

// CompareHoldings: securities.

func TestCompareHoldingsSaysMatchedWhenBothSidesAgree(t *testing.T) {
	inst := uuid.New()
	res := CompareHoldings(
		[]PortfolioPosition{{InstrumentUID: "uid-sber", Quantity: Quotation{Units: 100}}},
		[]MoneyBalance{{Currency: "RUB", Value: rub("8999.90")}},
		[]operation.Operation{
			aCashEntry(operation.TypeDeposit, 1_000_000, "RUB"),
			aBuy(inst, "100", -100_000, 10, "RUB"),
		},
		byUID(map[string]uuid.UUID{"uid-sber": inst}),
		map[uuid.UUID]string{inst: "SBER"},
	)

	if res.Status != ReconcileMatched {
		t.Fatalf("status = %q, want %q; mismatches: %+v", res.Status, ReconcileMatched, res.Mismatches)
	}
	if len(res.Mismatches) != 0 {
		t.Errorf("mismatches = %+v, want none", res.Mismatches)
	}
}

// One paper on two listings (AAPL, AAPL-RM) is one holding, compared once
// against the journal's sum (#135).
func TestOnePaperHeldOnTwoListingsIsOneHolding(t *testing.T) {
	inst := uuid.New()
	index := byUID(map[string]uuid.UUID{"uid-aapl": inst, "uid-aapl-rm": inst})
	broker := []PortfolioPosition{
		{InstrumentUID: "uid-aapl", Quantity: Quotation{Units: 10}},
		{InstrumentUID: "uid-aapl-rm", Quantity: Quotation{Units: 5}},
	}
	labels := map[uuid.UUID]string{inst: "AAPL"}

	agree := CompareHoldings(broker, nil, []operation.Operation{
		aCashEntry(operation.TypeDeposit, 15_000, "RUB"),
		aBuy(inst, "15", -15_000, 0, "RUB"),
	}, index, labels)
	if agree.Status != ReconcileMatched {
		t.Fatalf("status = %q, want %q — 10 + 5 at the broker against 15 in the journal: %+v",
			agree.Status, ReconcileMatched, agree.Mismatches)
	}

	differ := CompareHoldings(broker, nil, []operation.Operation{
		aCashEntry(operation.TypeDeposit, 12_000, "RUB"),
		aBuy(inst, "12", -12_000, 0, "RUB"),
	}, index, labels)
	if len(differ.Mismatches) != 1 {
		t.Fatalf("mismatches = %+v, want exactly one row for the one paper", differ.Mismatches)
	}
	m := differ.Mismatches[0]
	if !m.Broker.Equal(rub("15")) || !m.Journal.Equal(rub("12")) {
		t.Errorf("row says broker %s against journal %s, want 15 against 12 — the broker's side is the sum of its listings",
			m.Broker, m.Journal)
	}
}

// A security's Blocked is a flag (halted at the depository), not a count, and
// Quantity is the whole position; the owner does hold halted paper.
func TestABlockedPositionCountsAsItsWholeQuantity(t *testing.T) {
	inst := uuid.New()
	res := CompareHoldings(
		[]PortfolioPosition{{InstrumentUID: "uid-frozen", Quantity: Quotation{Units: 10}, Blocked: true}},
		nil,
		// The deposit pays for the purchase exactly, so cash agrees and anything
		// reported is about the security.
		[]operation.Operation{
			aCashEntry(operation.TypeDeposit, 10_000, "RUB"),
			aBuy(inst, "10", -10_000, 0, "RUB"),
		},
		byUID(map[string]uuid.UUID{"uid-frozen": inst}),
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
		byUID(map[string]uuid.UUID{"uid-sber": inst}),
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

// A supported security missing from the index is a difference of its own
// kind, MismatchUnknownSecurity, never a silent skip. On the owner's account
// these are the funds his TECH and TSPX were converted into.
func TestAnUnmappedBrokerPositionIsAMismatch(t *testing.T) {
	res := CompareHoldings(
		[]PortfolioPosition{{
			InstrumentUID: "uid-unknown", FIGI: "BBG000000000", InstrumentType: "share",
			Ticker: "TSLA", Quantity: Quotation{Units: 7},
		}},
		nil,
		nil,
		byUID(map[string]uuid.UUID{}),
		map[uuid.UUID]string{},
	)

	if res.Status != ReconcileMismatched {
		t.Fatalf("status = %q, want %q", res.Status, ReconcileMismatched)
	}
	if len(res.Mismatches) != 1 {
		t.Fatalf("mismatches = %+v, want exactly one", res.Mismatches)
	}
	m := res.Mismatches[0]
	switch m.Kind {
	case MismatchInstrument:
		t.Errorf("kind = %q — that is what a paper BOTH sides know but count differently gets, and it sends a "+
			"reader looking for operations that may not be missing at all", m.Kind)
	case MismatchUnsupported:
		t.Errorf("kind = %q — a share is not outside what this program accounts for; that value is for futures "+
			"and options, which no re-import will ever bring in", m.Kind)
	case MismatchUnknownSecurity:
	default:
		t.Errorf("kind = %q, want %q", m.Kind, MismatchUnknownSecurity)
	}
	if m.InstrumentID != nil {
		t.Errorf("instrument = %v, want none — nothing here is ours to name", m.InstrumentID)
	}
	if m.Label != "TSLA" {
		t.Errorf("label = %q, want the broker's own ticker %q", m.Label, "TSLA")
	}
	if !m.Broker.Equal(decimal.NewFromInt(7)) || !m.Journal.IsZero() {
		t.Errorf("broker/journal = %s/%s, want 7/0", m.Broker, m.Journal)
	}
}

// Cash appears in the position list as type "currency"
// (testdata/portfolio_cash_only.json: a live sandbox account with 50 000 ₽ and
// no trades). Compared as a security it would be a permanent phantom position
// and the account could never agree.
func TestCashIsNotAPhantomSecurity(t *testing.T) {
	res := CompareHoldings(
		[]PortfolioPosition{{
			InstrumentUID:  "a92e2e25-a698-45cc-a781-167cf465257c",
			FIGI:           "RUB000UTSTOM",
			InstrumentType: "currency",
			Ticker:         "RUB000UTSTOM",
			Quantity:       Quotation{Units: 50_000},
		}},
		[]MoneyBalance{{Currency: "RUB", Value: rub("50000")}},
		[]operation.Operation{aCashEntry(operation.TypeDeposit, 5_000_000, "RUB")},
		byUID(map[string]uuid.UUID{}),
		nil,
	)

	if res.Status != ReconcileMatched {
		t.Fatalf("status = %q, want %q — the rubles are the account's cash, which the money half of this "+
			"comparison already checked and found right: %+v", res.Status, ReconcileMatched, res.Mismatches)
	}
}

// The same cash is compared by the money side; the skip above is a
// handover, not a hole.
func TestCashIsStillComparedAsCash(t *testing.T) {
	res := CompareHoldings(
		[]PortfolioPosition{{
			InstrumentUID: "a92e2e25", FIGI: "RUB000UTSTOM", InstrumentType: "currency",
			Ticker: "RUB000UTSTOM", Quantity: Quotation{Units: 50_000},
		}},
		[]MoneyBalance{{Currency: "RUB", Value: rub("50000")}},
		[]operation.Operation{aCashEntry(operation.TypeDeposit, 4_000_000, "RUB")},
		byUID(map[string]uuid.UUID{}),
		nil,
	)

	if len(res.Mismatches) != 1 {
		t.Fatalf("mismatches = %+v, want exactly one", res.Mismatches)
	}
	m := res.Mismatches[0]
	if m.Kind != MismatchCurrency || m.Label != "RUB" {
		t.Errorf("mismatch = %+v, want the currency row for RUB", m)
	}
	if !m.Broker.Equal(rub("50000")) || !m.Journal.Equal(rub("40000")) {
		t.Errorf("broker/journal = %s/%s, want 50000/40000", m.Broker, m.Journal)
	}
}

// A future is MismatchUnsupported, not MismatchInstrument: held, but outside
// what this program accounts for, so the owner should not hunt for missing
// operations. Any type outside brokerInstrumentTypes behaves the same.
func TestAnAssetThisProgramCannotHoldIsItsOwnKindOfDifference(t *testing.T) {
	res := CompareHoldings(
		[]PortfolioPosition{{
			InstrumentUID:  "uid-futures",
			FIGI:           "FUTSBRF06250",
			InstrumentType: "futures",
			Ticker:         "SBRF-6.25",
			Quantity:       Quotation{Units: 4},
		}},
		nil, nil,
		byUID(map[string]uuid.UUID{}),
		nil,
	)

	if len(res.Mismatches) != 1 {
		t.Fatalf("mismatches = %+v, want exactly one", res.Mismatches)
	}
	m := res.Mismatches[0]
	if m.Kind != MismatchUnsupported {
		t.Errorf("kind = %q, want %q", m.Kind, MismatchUnsupported)
	}
	if m.Label != "SBRF-6.25" {
		t.Errorf("label = %q, want the ticker a person recognizes", m.Label)
	}
	if !m.Broker.Equal(decimal.NewFromInt(4)) || !m.Journal.IsZero() {
		t.Errorf("broker/journal = %s/%s, want 4/0", m.Broker, m.Journal)
	}
	if res.Status != ReconcileMismatched {
		t.Errorf("status = %q, want %q — an asset the program cannot hold is still a difference",
			res.Status, ReconcileMismatched)
	}
}

// brokerLabel's fallbacks in order; an instrument_uid is a bare UUID.
func TestBrokerLabelPrefersWhatAPersonReads(t *testing.T) {
	cases := []struct {
		name string
		p    PortfolioPosition
		want string
	}{
		{
			"ticker wins",
			PortfolioPosition{Ticker: "SBER", FIGI: "BBG004730N88", InstrumentUID: "uid", InstrumentType: "share"},
			"SBER",
		},
		{
			"figi when there is no ticker",
			PortfolioPosition{FIGI: "BBG004730N88", InstrumentUID: "uid", InstrumentType: "share"},
			"BBG004730N88",
		},
		{
			"the uid when there is no figi either",
			PortfolioPosition{InstrumentUID: "uid", InstrumentType: "share"},
			"uid",
		},
		{
			"the type when the broker identified nothing",
			PortfolioPosition{InstrumentType: "option"},
			"option",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := brokerLabel(c.p); got != c.want {
				t.Errorf("brokerLabel = %q, want %q", got, c.want)
			}
		})
	}
}

// A position whose instrument_uid drifted is found by figi, as the resolver
// matched its operations; knowing only the uid would show two false lines.
func TestAPositionWhoseUIDDriftedIsFoundByItsFIGI(t *testing.T) {
	inst := uuid.New()
	res := CompareHoldings(
		[]PortfolioPosition{{
			InstrumentUID:  "uid-sber-NEW",
			FIGI:           "BBG004730N88",
			InstrumentType: "share",
			Ticker:         "SBER",
			Quantity:       Quotation{Units: 100},
		}},
		nil,
		[]operation.Operation{
			aCashEntry(operation.TypeDeposit, 100_000, "RUB"),
			aBuy(inst, "100", -100_000, 0, "RUB"),
		},
		InstrumentIndex{
			ByUID:  map[string]uuid.UUID{"uid-sber-OLD": inst},
			ByFIGI: map[string]uuid.UUID{"BBG004730N88": inst},
		},
		map[uuid.UUID]string{inst: "SBER"},
	)

	if res.Status != ReconcileMatched {
		t.Fatalf("status = %q, want %q — 100 against 100, matched by figi after the uid drifted: %+v",
			res.Status, ReconcileMatched, res.Mismatches)
	}
}

// uid before figi, as the resolver does; both hit different instruments
// here, so the winner is visible.
func TestTheUIDIsTriedBeforeTheFIGI(t *testing.T) {
	byTheUID, byTheFIGI := uuid.New(), uuid.New()
	res := CompareHoldings(
		[]PortfolioPosition{{
			InstrumentUID: "uid", FIGI: "figi", InstrumentType: "share", Ticker: "SBER",
			Quantity: Quotation{Units: 1},
		}},
		nil, nil,
		InstrumentIndex{
			ByUID:  map[string]uuid.UUID{"uid": byTheUID},
			ByFIGI: map[string]uuid.UUID{"figi": byTheFIGI},
		},
		map[uuid.UUID]string{byTheUID: "BY-UID", byTheFIGI: "BY-FIGI"},
	)

	if len(res.Mismatches) != 1 {
		t.Fatalf("mismatches = %+v, want exactly one", res.Mismatches)
	}
	if got := res.Mismatches[0].InstrumentID; got == nil || *got != byTheUID {
		t.Errorf("instrument = %v, want the one found by instrument_uid (%v)", got, byTheUID)
	}
}

// An empty identifier matches nothing.
func TestAPositionWithNoIdentifiersMatchesNothing(t *testing.T) {
	inst := uuid.New()
	_, ok := InstrumentIndex{
		ByUID:  map[string]uuid.UUID{"": inst},
		ByFIGI: map[string]uuid.UUID{"": inst},
	}.lookup(PortfolioPosition{InstrumentType: "share", Quantity: Quotation{Units: 1}})

	if ok {
		t.Error("a position carrying no identifier at all was matched to an instrument")
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
		byUID(map[string]uuid.UUID{"uid-sber": inst}),
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

// A sold-out position at zero in the engine is no difference: the broker
// does not report it.
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
		byUID(map[string]uuid.UUID{"uid-sber": inst}),
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
		byUID(map[string]uuid.UUID{"uid-sber": inst}),
		nil,
	)

	if len(res.Mismatches) != 1 {
		t.Fatalf("mismatches = %+v, want exactly one", res.Mismatches)
	}
	if res.Mismatches[0].Label != inst.String() {
		t.Errorf("label = %q, want %q", res.Mismatches[0].Label, inst)
	}
}

// CompareHoldings: money.

// The cash formula with literals, since nothing else computes it: a 10 000,00
// ₽ deposit and a 1 000,00 ₽ purchase with 0,10 commission leave 8 999,90 ₽.
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

// A transfer's amount is a basis, not cash; counting it would invent a
// balance on every account shares moved into.
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

// Free and blocked money are two addends of one balance.
func TestCashCountsFreeAndBlockedTogether(t *testing.T) {
	res := CompareHoldings(
		nil,
		[]MoneyBalance{{Currency: "RUB", Value: rub("8000.00"), Blocked: rub("999.90")}},
		[]operation.Operation{
			aCashEntry(operation.TypeDeposit, 1_000_000, "RUB"),
			aCashEntry(operation.TypeWithdrawal, -100_010, "RUB"),
		},
		InstrumentIndex{}, nil,
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
		InstrumentIndex{}, nil,
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

// A currency the broker does not mention is one it holds none of.
func TestACurrencyOnlyTheJournalKnowsIsAMismatch(t *testing.T) {
	res := CompareHoldings(
		nil,
		[]MoneyBalance{{Currency: "RUB", Value: rub("0")}},
		[]operation.Operation{aCashEntry(operation.TypeDeposit, 4_200, "USD")},
		InstrumentIndex{}, nil,
	)

	if len(res.Mismatches) != 1 {
		t.Fatalf("mismatches = %+v, want exactly one", res.Mismatches)
	}
	m := res.Mismatches[0]
	if m.Label != "USD" || !m.Broker.IsZero() || !m.Journal.Equal(rub("42.00")) {
		t.Errorf("mismatch = %+v, want USD 0 against 42.00", m)
	}
}

// CCC and DDD, held in the journal and not reported, make the unstable half
// (iterating the engine's map) actually run; without them only the broker's
// slice order would be tested.
func TestMismatchesComeOutInAStableOrder(t *testing.T) {
	instA, instB := uuid.New(), uuid.New()
	instC, instD := uuid.New(), uuid.New()
	res := CompareHoldings(
		[]PortfolioPosition{
			{InstrumentUID: "uid-b", Quantity: Quotation{Units: 2}},
			{InstrumentUID: "uid-a", Quantity: Quotation{Units: 1}},
		},
		[]MoneyBalance{
			{Currency: "USD", Value: rub("1.00")},
			{Currency: "RUB", Value: rub("1.00")},
		},
		[]operation.Operation{
			aBuy(instC, "3", -300, 0, "RUB"),
			aBuy(instD, "4", -400, 0, "RUB"),
		},
		byUID(map[string]uuid.UUID{"uid-a": instA, "uid-b": instB}),
		map[uuid.UUID]string{instA: "AAA", instB: "BBB", instC: "CCC", instD: "DDD"},
	)

	var got []string
	for _, m := range res.Mismatches {
		got = append(got, m.Kind+":"+m.Label)
	}
	want := []string{
		"currency:RUB", "currency:USD",
		"instrument:AAA", "instrument:BBB", "instrument:CCC", "instrument:DDD",
	}
	if len(got) != len(want) {
		t.Fatalf("mismatches = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mismatches = %v, want %v", got, want)
		}
	}
}

// A journal the engine refuses is "not checked", never "agrees".
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
		byUID(map[string]uuid.UUID{"uid-sber": inst}),
		map[uuid.UUID]string{inst: "SBER"},
	)

	if res.Status != ReconcileNotChecked {
		t.Errorf("status = %q, want %q", res.Status, ReconcileNotChecked)
	}
	if len(res.Mismatches) != 0 {
		t.Errorf("mismatches = %+v, want none: nothing was compared", res.Mismatches)
	}
}

// ReconcileLink.

// markedBalance is one call to SetBalance, as the marker saw it.
type markedBalance struct {
	spaceID, accountID uuid.UUID
	asOf               time.Time
	amountMinor        int64
}

// recordingMarker stands in for *account.Store: it answers what currency the
// account is kept in and records the marks written to it.
type recordingMarker struct {
	// currency is what the account is kept in — the thing that decides
	// whether the broker's rubles may be filed as its mark at all.
	currency string
	marks    []markedBalance
	// err is what SetBalance refuses with; readErr is what ByID refuses with,
	// which is this program's own database failing before any mark is written.
	err, readErr error
}

// newMarker is a marker over a rouble account, as a link must name.
func newMarker() *recordingMarker { return &recordingMarker{currency: "RUB"} }

func (m *recordingMarker) ByID(_ context.Context, spaceID, id uuid.UUID) (account.WithBalance, error) {
	if m.readErr != nil {
		return account.WithBalance{}, m.readErr
	}
	return account.WithBalance{
		Account: account.Account{ID: id, SpaceID: spaceID, Currency: m.currency},
	}, nil
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
		InstrumentRef{InstrumentUID: uid, FIGI: "BBG004730N88"}, inst.ISIN, inst.Ticker, "RUB"); err != nil {
		t.Fatalf("seed map: %v", err)
	}
	return inst.ID
}

// The balance mark is the broker's figure for the whole account, securities
// at its prices plus cash, which the journal's valuation is checked against (Р-2,
// 2026-10-02).
func TestReconcileLinkMarksTheBalanceWithTheBrokersTotal(t *testing.T) {
	f := newFixture(t)
	inst := f.seedMapped(t, "uid-sber", "SBER")

	srv, _ := serve(t, map[string]route{
		portfolioPath: {status: http.StatusOK, body: []byte(
			`{"positions":[{"instrumentUid":"uid-sber","figi":"BBG004730N88","instrumentType":"share",` +
				`"quantity":{"units":"100","nano":0},"blocked":false}],` +
				`"totalAmountPortfolio":{"currency":"rub","units":"36499","nano":900000000}}`)},
		positionsPath: {status: http.StatusOK, body: []byte(
			`{"money":[{"currency":"rub","units":"8000","nano":0}],` +
				`"blocked":[{"currency":"rub","units":"999","nano":900000000}]}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)

	marker := newMarker()
	r := NewReconciler(f.store, fakeJournal{ops: []operation.Operation{
		aCashEntry(operation.TypeDeposit, 1_000_000, "RUB"),
		aBuy(inst, "100", -100_000, 10, "RUB"),
	}}, marker, instrument.NewStore(f.pool), nil, nil)
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
	// The broker's own total, 36 499,90 ₽ — not its 8 999,90 ₽ of rubles.
	if got.amountMinor != 3_649_990 {
		t.Errorf("amount = %d, want 3649990 — the whole account as the broker values it", got.amountMinor)
	}
	if got.spaceID != f.spaceID || got.accountID != f.accountID {
		t.Errorf("mark filed under %s/%s, want %s/%s", got.spaceID, got.accountID, f.spaceID, f.accountID)
	}
	// 22:30 UTC on the 4th is the 5th in Moscow, the broker's day.
	want := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	if !got.asOf.Equal(want) {
		t.Errorf("as_of = %s, want %s", got.asOf, want)
	}
}

// End to end from a live sandbox response: an account topped up with 50 000 ₽
// and nothing bought must agree, its one "currency" position being the
// cash.
func TestReconcileLinkAgreesWithAnAccountThatOnlyHoldsCash(t *testing.T) {
	f := newFixture(t)

	srv, _ := serve(t, map[string]route{
		portfolioPath: {status: http.StatusOK, body: readFixture(t, "portfolio_cash_only.json")},
		positionsPath: {status: http.StatusOK, body: []byte(
			`{"money":[{"currency":"rub","units":"50000","nano":0}]}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)

	marker := newMarker()
	r := NewReconciler(f.store, fakeJournal{ops: []operation.Operation{
		aCashEntry(operation.TypeDeposit, 5_000_000, "RUB"),
	}}, marker, instrument.NewStore(f.pool), nil, nil)

	res, err := r.ReconcileLink(f.ctx, c, f.conn, f.link)
	if err != nil {
		t.Fatalf("ReconcileLink: %v", err)
	}
	if res.Status != ReconcileMatched {
		t.Fatalf("status = %q, want %q: %+v", res.Status, ReconcileMatched, res.Mismatches)
	}
	if len(marker.marks) != 1 || marker.marks[0].amountMinor != 5_000_000 {
		t.Errorf("marks = %+v, want one of 5000000", marker.marks)
	}
}

// A broker that did not answer: "not checked", the error returned, no mark.
func TestReconcileLinkSaysNotCheckedWhenThePortfolioIsUnavailable(t *testing.T) {
	f := newFixture(t)

	srv, _ := serve(t, map[string]route{
		portfolioPath: {status: http.StatusInternalServerError, body: []byte(`{"message":"boom"}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)

	marker := newMarker()
	r := NewReconciler(f.store, fakeJournal{}, marker, instrument.NewStore(f.pool), nil, nil)

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

	marker := newMarker()
	r := NewReconciler(f.store, fakeJournal{}, marker, instrument.NewStore(f.pool), nil, nil)

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

// The mark is the broker's statement, written even when the sides
// disagree.
func TestReconcileLinkMarksTheBalanceEvenWhenTheSidesDisagree(t *testing.T) {
	f := newFixture(t)

	srv, _ := serve(t, map[string]route{
		portfolioPath: {status: http.StatusOK, body: []byte(
			`{"positions":[{"instrumentUid":"uid-nobody-mapped","instrumentType":"share",` +
				`"quantity":{"units":"3","nano":0},"blocked":false}],` +
				`"totalAmountPortfolio":{"currency":"rub","units":"1","nano":0}}`)},
		positionsPath: {status: http.StatusOK, body: []byte(
			`{"money":[{"currency":"rub","units":"1","nano":0}]}`)},
		// The broker knows nothing about it either: the position stays
		// unmatched, which is what this test's difference is made of.
		instrumentByPath: {status: http.StatusNotFound, body: []byte(
			`{"code":5,"message":"Instrument not found","description":"50002"}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)

	marker := newMarker()
	r := NewReconciler(f.store, fakeJournal{}, marker, instrument.NewStore(f.pool), nil, nil)

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

	r := NewReconciler(f.store, fakeJournal{}, newMarker(), instrument.NewStore(f.pool), nil, nil)
	res, err := r.ReconcileLink(f.ctx, NewClient(nil, "", "token", nil), f.conn, other)
	if !errors.Is(err, ErrLinkNotInConnection) {
		t.Fatalf("error = %v, want %v", err, ErrLinkNotInConnection)
	}
	if res.Status != ReconcileNotChecked {
		t.Errorf("status = %q, want %q", res.Status, ReconcileNotChecked)
	}
}

// A link and connection in different spaces would check one household's
// broker account against another's account; the client has no server, so
// reaching one would already be the failure.
func TestReconcileLinkRefusesALinkOfAnotherSpace(t *testing.T) {
	f := newFixture(t)
	other := f.link
	other.SpaceID = uuid.New()

	marker := newMarker()
	r := NewReconciler(f.store, fakeJournal{}, marker, instrument.NewStore(f.pool), nil, nil)
	res, err := r.ReconcileLink(f.ctx, NewClient(nil, "", "token", nil), f.conn, other)
	if !errors.Is(err, ErrLinkOutsideSpace) {
		t.Fatalf("error = %v, want %v", err, ErrLinkOutsideSpace)
	}
	if res.Status != ReconcileNotChecked {
		t.Errorf("status = %q, want %q", res.Status, ReconcileNotChecked)
	}
	if len(marker.marks) != 0 {
		t.Errorf("marks = %+v, want none", marker.marks)
	}
}

// When our own database refused a read, no mark is written and the run is
// "not checked". Unlike a journal the engine refuses, which still gets the
// mark: here the journal was never read.
func TestReconcileLinkMarksNothingWhenOurOwnJournalCannotBeRead(t *testing.T) {
	f := newFixture(t)

	srv, _ := serve(t, map[string]route{
		portfolioPath: {status: http.StatusOK, body: []byte(`{"positions":[]}`)},
		positionsPath: {status: http.StatusOK, body: []byte(
			`{"money":[{"currency":"rub","units":"7","nano":0}]}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)

	dbDown := errors.New("connection refused")
	marker := newMarker()
	r := NewReconciler(f.store, fakeJournal{err: dbDown}, marker, instrument.NewStore(f.pool), nil, nil)

	res, err := r.ReconcileLink(f.ctx, c, f.conn, f.link)
	if !errors.Is(err, dbDown) {
		t.Fatalf("error = %v, want the database's own refusal", err)
	}
	if res.Status != ReconcileNotChecked {
		t.Errorf("status = %q, want %q", res.Status, ReconcileNotChecked)
	}
	if len(marker.marks) != 0 {
		t.Errorf("marks = %+v, want none: nothing was compared and nothing is marked", marker.marks)
	}
}

// No total from the broker leaves the previous mark; the check still
// counts.
func TestReconcileLinkLeavesTheMarkWhenTheBrokerNamesNoTotal(t *testing.T) {
	f := newFixture(t)
	srv, _ := serve(t, map[string]route{
		portfolioPath: {status: http.StatusOK, body: []byte(`{"positions":[]}`)},
		positionsPath: {status: http.StatusOK, body: []byte(`{"money":[]}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)
	marker := newMarker()
	r := NewReconciler(f.store, fakeJournal{}, marker, instrument.NewStore(f.pool), nil, nil)

	res, err := r.ReconcileLink(f.ctx, c, f.conn, f.link)
	if err != nil {
		t.Fatalf("ReconcileLink: %v", err)
	}
	if res.Status != ReconcileMatched {
		t.Errorf("status = %q, want %q", res.Status, ReconcileMatched)
	}
	if len(marker.marks) != 0 {
		t.Errorf("marks = %+v, want none: the broker named no total", marker.marks)
	}
}

// A total stated in another currency than the rubles asked for is refused
// rather than filed under a ruble account.
func TestReconcileLinkRefusesATotalInAnotherCurrency(t *testing.T) {
	f := newFixture(t)
	srv, _ := serve(t, map[string]route{
		portfolioPath: {status: http.StatusOK, body: []byte(
			`{"positions":[],"totalAmountPortfolio":{"currency":"usd","units":"100","nano":0}}`)},
		positionsPath: {status: http.StatusOK, body: []byte(`{"money":[]}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)
	marker := newMarker()
	r := NewReconciler(f.store, fakeJournal{}, marker, instrument.NewStore(f.pool), nil, nil)

	if _, err := r.ReconcileLink(f.ctx, c, f.conn, f.link); !errors.Is(err, ErrBalanceMarkRefused) {
		t.Fatalf("error = %v, want %v", err, ErrBalanceMarkRefused)
	}
	if len(marker.marks) != 0 {
		t.Errorf("marks = %+v, want none: 100 dollars under a ruble account is a wrong number", marker.marks)
	}
}

// A non-rouble account is refused loudly and not marked: the mark has no
// currency of its own, and the figure is in roubles.
func TestReconcileLinkRefusesToMarkANonRubleAccount(t *testing.T) {
	f := newFixture(t)

	srv, _ := serve(t, map[string]route{
		portfolioPath: {status: http.StatusOK, body: []byte(`{"positions":[]}`)},
		positionsPath: {status: http.StatusOK, body: []byte(
			`{"money":[{"currency":"rub","units":"8000","nano":0}]}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)

	marker := newMarker()
	marker.currency = "USD"
	r := NewReconciler(f.store, fakeJournal{}, marker, instrument.NewStore(f.pool), nil, nil)

	_, err := r.ReconcileLink(f.ctx, c, f.conn, f.link)
	if !errors.Is(err, ErrAccountNotInRubles) {
		t.Fatalf("error = %v, want %v", err, ErrAccountNotInRubles)
	}
	if len(marker.marks) != 0 {
		t.Errorf("marks = %+v, want none: 8 000 ₽ under a dollar account is a wrong number", marker.marks)
	}
}

// A sum finer than a kopeck is refused as a balance mark failure, not a
// projection one: name the action that failed.
func TestABalanceMarkFinerThanAKopeckIsRefusedForWhatItIs(t *testing.T) {
	f := newFixture(t)

	srv, _ := serve(t, map[string]route{
		// 8 000,005 ₽ — half a kopeck, which no whole number of kopecks holds.
		portfolioPath: {status: http.StatusOK, body: []byte(
			`{"positions":[],"totalAmountPortfolio":{"currency":"rub","units":"8000","nano":5000000}}`)},
		positionsPath: {status: http.StatusOK, body: []byte(
			`{"money":[{"currency":"rub","units":"8000","nano":5000000}]}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)

	marker := newMarker()
	r := NewReconciler(f.store, fakeJournal{}, marker, instrument.NewStore(f.pool), nil, nil)

	_, err := r.ReconcileLink(f.ctx, c, f.conn, f.link)
	if !errors.Is(err, ErrBalanceMarkRefused) {
		t.Fatalf("error = %v, want %v", err, ErrBalanceMarkRefused)
	}
	if strings.Contains(err.Error(), "not projected") {
		t.Errorf("error = %q, but nothing was being projected: a balance mark was being written", err)
	}
	if !strings.Contains(err.Error(), "finer than a minor unit") {
		t.Errorf("error = %q, want it to keep the reason the sum could not be stored", err)
	}
	if len(marker.marks) != 0 {
		t.Errorf("marks = %+v, want none", marker.marks)
	}
}

// Our journal not computing and the mark failing are both returned.
func TestBothRefusalsSurviveWhenBothHappened(t *testing.T) {
	f := newFixture(t)
	inst := f.seedMapped(t, "uid-sber", "SBER")

	srv, _ := serve(t, map[string]route{
		portfolioPath: {status: http.StatusOK, body: []byte(
			`{"positions":[],"totalAmountPortfolio":{"currency":"rub","units":"1","nano":0}}`)},
		positionsPath: {status: http.StatusOK, body: []byte(
			`{"money":[{"currency":"rub","units":"1","nano":0}]}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)

	// More sold than was ever held: the engine refuses this journal outright.
	journal := []operation.Operation{
		aBuy(inst, "1", -1_000, 0, "RUB"),
		aSell(inst, "10", 11_000, 0, "RUB"),
	}
	if _, err := portfolio.Compute(journal); err == nil {
		t.Fatal("this journal is supposed to be one the engine refuses; it did not")
	}

	markFailed := errors.New("the balance table is locked")
	marker := newMarker()
	marker.err = markFailed
	r := NewReconciler(f.store, fakeJournal{ops: journal}, marker, instrument.NewStore(f.pool), nil, nil)

	res, err := r.ReconcileLink(f.ctx, c, f.conn, f.link)
	if res.Status != ReconcileNotChecked {
		t.Errorf("status = %q, want %q", res.Status, ReconcileNotChecked)
	}
	if !errors.Is(err, markFailed) {
		t.Errorf("error = %v, want it to carry the failure to write the mark", err)
	}
	if !strings.Contains(err.Error(), "does not compute") {
		t.Errorf("error = %q, want it to carry the engine's refusal as well — it is why nothing was checked", err)
	}
}

// The map read is the link's own connection's.
func TestReconcileLinkReadsTheMapOfItsOwnConnection(t *testing.T) {
	f := newFixture(t)
	inst := f.seedMapped(t, "uid-sber", "SBER")

	otherConn, err := f.store.CreateConnection(f.ctx, f.spaceID, []byte("x"), "0000", StatusActive)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	if err := f.store.saveMap(f.ctx, otherConn.ID, inst,
		InstrumentRef{InstrumentUID: "uid-elsewhere"}, "RU0009029540", "SBER", "RUB"); err != nil {
		t.Fatalf("seed other map: %v", err)
	}

	index, labels, err := f.store.instrumentMap(f.ctx, f.conn.ID)
	if err != nil {
		t.Fatalf("instrumentMap: %v", err)
	}
	if _, ok := index.ByUID["uid-elsewhere"]; ok {
		t.Errorf("map = %v, want no row of the other connection", index.ByUID)
	}
	if index.ByUID["uid-sber"] != inst {
		t.Errorf("uid-sber = %v, want %v", index.ByUID["uid-sber"], inst)
	}
	if labels[inst] != "SBER" {
		t.Errorf("label = %q, want %q", labels[inst], "SBER")
	}
}

// The index reads the figi column too, so a drifted uid matches here as
// in the resolver.
func TestTheInstrumentIndexCarriesTheFIGIToo(t *testing.T) {
	f := newFixture(t)
	inst := f.seedMapped(t, "uid-sber", "SBER")

	index, _, err := f.store.instrumentMap(f.ctx, f.conn.ID)
	if err != nil {
		t.Fatalf("instrumentMap: %v", err)
	}
	if index.ByFIGI["BBG004730N88"] != inst {
		t.Errorf("figi BBG004730N88 = %v, want %v", index.ByFIGI["BBG004730N88"], inst)
	}
}

// A figi two rows give different instruments answers for neither: the
// position shows as a difference, rather than a match that varies with row
// order.
func TestAFIGITwoRowsDisagreeAboutAnswersForNeither(t *testing.T) {
	f := newFixture(t)
	first := f.seedMapped(t, "uid-first", "SBER")

	other, err := instrument.NewStore(f.pool).Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Другая бумага", Ticker: "OTHER",
		ISIN: "RU0009029541", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	if err := f.store.saveMap(f.ctx, f.conn.ID, other.ID,
		InstrumentRef{InstrumentUID: "uid-second", FIGI: "BBG004730N88"}, other.ISIN, other.Ticker, "RUB"); err != nil {
		t.Fatalf("seed second map row: %v", err)
	}

	index, _, err := f.store.instrumentMap(f.ctx, f.conn.ID)
	if err != nil {
		t.Fatalf("instrumentMap: %v", err)
	}
	if got, ok := index.ByFIGI["BBG004730N88"]; ok {
		t.Errorf("figi BBG004730N88 = %v, want no answer at all: two rows claim it (%v and %v)",
			got, first, other.ID)
	}
	// The instrument_uid side is untouched — each row still answers under its
	// own, which is the identifier the uniqueness of the table is built on.
	if index.ByUID["uid-first"] != first || index.ByUID["uid-second"] != other.ID {
		t.Errorf("byUID = %v, want both rows under their own instrument_uid", index.ByUID)
	}
}

// An instrument without a ticker is labelled by its name.
func TestAnInstrumentWithoutATickerIsLabelledByItsName(t *testing.T) {
	f := newFixture(t)
	inst, err := instrument.NewStore(f.pool).Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeCustom, Name: "Замороженный пай", Currency: "USD",
	})
	if err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	if err := f.store.saveMap(f.ctx, f.conn.ID, inst.ID,
		InstrumentRef{InstrumentUID: "uid-frozen"}, "", "", "RUB"); err != nil {
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

// A map row without instrument_uid answers for nothing; written by hand
// because the resolver never writes one.
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

	index, _, err := f.store.instrumentMap(f.ctx, f.conn.ID)
	if err != nil {
		t.Fatalf("instrumentMap: %v", err)
	}
	if _, ok := index.ByUID[""]; ok {
		t.Errorf("map = %v, want no answer under the empty identifier", index.ByUID)
	}
}

// FinishRun: the verdict in the run log.

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

// A run nobody reconciled stays "not checked" with no time.
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

// Agreement is an empty list, not the null of "never looked".
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

// A verdict its own list contradicts is refused; the CHECK covers only the
// word.
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

// The owner's screen: a share moved venues, the portfolio says AMZN-RM, the
// journal AMZN. Seven papers showed twice with agreeing quantities; paired by
// ISIN they agree.
func TestReconcileMatchesOnePaperListedOnTwoVenues(t *testing.T) {
	f := newFixture(t)

	// Ours, mapped under the listing the history named.
	inst := f.instrumentWithISIN(t, "AMZN", "US0231351067")
	if err := f.store.saveMap(f.ctx, f.conn.ID, inst.ID,
		InstrumentRef{InstrumentUID: "uid-amzn"}, inst.ISIN, inst.Ticker, "USD"); err != nil {
		t.Fatalf("saveMap: %v", err)
	}

	srv, _ := serve(t, map[string]route{
		// The broker reports the OTHER listing, which the map knows nothing of.
		portfolioPath: {status: http.StatusOK, body: []byte(
			`{"positions":[{"instrumentUid":"uid-amzn-rm","instrumentType":"share",` +
				`"quantity":{"units":"20","nano":0},"blocked":false}]}`)},
		positionsPath: {status: http.StatusOK, body: []byte(`{"money":[]}`)},
		// Asked what that listing is, the broker names the same ISIN.
		instrumentByPath: {status: http.StatusOK, body: []byte(
			`{"instrument":{"uid":"uid-amzn-rm","ticker":"AMZN-RM","name":"Amazon",` +
				`"isin":"US0231351067","currency":"usd","instrumentType":"share"}}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)

	r := NewReconciler(f.store, fakeJournal{ops: []operation.Operation{
		aBuy(inst.ID, "20", -200_000, 20, "USD"),
	}}, newMarker(), instrument.NewStore(f.pool), nil, nil)

	res, err := r.ReconcileLink(f.ctx, c, f.conn, f.link)
	if err != nil {
		t.Fatalf("ReconcileLink: %v", err)
	}
	// Securities only: this fixture's cash differs on purpose.
	if got := securitiesMismatches(res); len(got) != 0 {
		t.Fatalf("got %+v, want none: one paper on two venues is one holding", got)
	}
}

// securitiesMismatches is the part of a verdict that is about papers.
func securitiesMismatches(res ReconcileResult) []ReconcileMismatch {
	out := []ReconcileMismatch{}
	for _, m := range res.Mismatches {
		if m.Kind != MismatchCurrency {
			out = append(out, m)
		}
	}
	return out
}

// After pairing, a real difference is one line: the owner's Amazon, 1 in the
// journal against 20 at the broker (the unreported 20:1 split of June 2022).
func TestReconcileStillReportsARealDifferenceAcrossVenues(t *testing.T) {
	f := newFixture(t)

	inst := f.instrumentWithISIN(t, "AMZN2", "US0231351067")
	if err := f.store.saveMap(f.ctx, f.conn.ID, inst.ID,
		InstrumentRef{InstrumentUID: "uid-amzn"}, inst.ISIN, inst.Ticker, "USD"); err != nil {
		t.Fatalf("saveMap: %v", err)
	}

	srv, _ := serve(t, map[string]route{
		portfolioPath: {status: http.StatusOK, body: []byte(
			`{"positions":[{"instrumentUid":"uid-amzn-rm","instrumentType":"share",` +
				`"quantity":{"units":"20","nano":0},"blocked":false}]}`)},
		positionsPath: {status: http.StatusOK, body: []byte(`{"money":[]}`)},
		instrumentByPath: {status: http.StatusOK, body: []byte(
			`{"instrument":{"uid":"uid-amzn-rm","ticker":"AMZN-RM","name":"Amazon",` +
				`"isin":"US0231351067","currency":"usd","instrumentType":"share"}}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)

	r := NewReconciler(f.store, fakeJournal{ops: []operation.Operation{
		aBuy(inst.ID, "1", -200_000, 1, "USD"),
	}}, newMarker(), instrument.NewStore(f.pool), nil, nil)

	res, err := r.ReconcileLink(f.ctx, c, f.conn, f.link)
	if err != nil {
		t.Fatalf("ReconcileLink: %v", err)
	}
	got := securitiesMismatches(res)
	if len(got) != 1 {
		t.Fatalf("got %d differences about papers, want exactly 1: %+v", len(got), got)
	}
	m := got[0]
	if m.Broker.String() != "20" || m.Journal.String() != "1" {
		t.Errorf("difference = broker %s / journal %s, want 20 and 1 on one line", m.Broker, m.Journal)
	}
	if m.InstrumentID == nil || *m.InstrumentID != inst.ID {
		t.Errorf("the line names %v, want our own catalog row %s", m.InstrumentID, inst.ID)
	}
}

// An unknown security carries the broker's passport (ISIN, name, upper-case
// currency, translated type) instead of a bare ticker like «TECH2». The owner's
// live case: the fund his TECH was converted into.
func TestReconcileUnknownSecurityCarriesTheBrokersPassport(t *testing.T) {
	f := newFixture(t)

	srv, _ := serve(t, map[string]route{
		portfolioPath: {status: http.StatusOK, body: []byte(
			`{"positions":[{"instrumentUid":"uid-tech2","instrumentType":"etf",` +
				`"quantity":{"units":"60795","nano":0},"blocked":true}]}`)},
		positionsPath: {status: http.StatusOK, body: []byte(`{"money":[]}`)},
		// Asked what the position is, the broker answers — lowercase currency,
		// the way the wire really spells it.
		instrumentByPath: {status: http.StatusOK, body: []byte(
			`{"instrument":{"uid":"uid-tech2","ticker":"TECH2",` +
				`"name":"Заблокированные активы Тинькофф Технологии",` +
				`"isin":"RU000A1071G8","currency":"rub","instrumentType":"etf"}}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)

	r := NewReconciler(f.store, fakeJournal{}, newMarker(), instrument.NewStore(f.pool), nil, nil)
	res, err := r.ReconcileLink(f.ctx, c, f.conn, f.link)
	if err != nil {
		t.Fatalf("ReconcileLink: %v", err)
	}

	got := securitiesMismatches(res)
	if len(got) != 1 {
		t.Fatalf("got %d differences about papers, want exactly 1: %+v", len(got), got)
	}
	m := got[0]
	if m.Kind != MismatchUnknownSecurity {
		t.Fatalf("kind = %q, want %q", m.Kind, MismatchUnknownSecurity)
	}
	if m.BrokerISIN == nil || *m.BrokerISIN != "RU000A1071G8" {
		t.Errorf("broker_isin = %v, want RU000A1071G8", m.BrokerISIN)
	}
	if m.BrokerName == nil || *m.BrokerName != "Заблокированные активы Тинькофф Технологии" {
		t.Errorf("broker_name = %v, want the passport's name", m.BrokerName)
	}
	// Upper case, as CreateInstrumentRequest requires.
	if m.BrokerCurrency == nil || *m.BrokerCurrency != "RUB" {
		t.Errorf("broker_currency = %v, want RUB", m.BrokerCurrency)
	}
	if m.BrokerType == nil || *m.BrokerType != string(instrument.TypeETF) {
		t.Errorf("broker_type = %v, want %q — our own type word, translated by the importer's table",
			m.BrokerType, instrument.TypeETF)
	}
}

// A future gets no passport, though one was fetched: it is not something
// a catalog row can be made for.
func TestReconcileUnsupportedAssetCarriesNoPassport(t *testing.T) {
	f := newFixture(t)

	srv, _ := serve(t, map[string]route{
		portfolioPath: {status: http.StatusOK, body: []byte(
			`{"positions":[{"instrumentUid":"uid-fut","instrumentType":"futures",` +
				`"quantity":{"units":"3","nano":0}}]}`)},
		positionsPath: {status: http.StatusOK, body: []byte(`{"money":[]}`)},
		instrumentByPath: {status: http.StatusOK, body: []byte(
			`{"instrument":{"uid":"uid-fut","ticker":"SiH6","name":"Фьючерс на доллар",` +
				`"isin":"RU000FUT00001","currency":"rub","instrumentType":"futures"}}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)

	r := NewReconciler(f.store, fakeJournal{}, newMarker(), instrument.NewStore(f.pool), nil, nil)
	res, err := r.ReconcileLink(f.ctx, c, f.conn, f.link)
	if err != nil {
		t.Fatalf("ReconcileLink: %v", err)
	}

	got := securitiesMismatches(res)
	if len(got) != 1 {
		t.Fatalf("got %d differences about papers, want exactly 1: %+v", len(got), got)
	}
	m := got[0]
	if m.Kind != MismatchUnsupported {
		t.Fatalf("kind = %q, want %q", m.Kind, MismatchUnsupported)
	}
	if m.BrokerISIN != nil || m.BrokerName != nil || m.BrokerCurrency != nil || m.BrokerType != nil {
		t.Errorf("an unsupported asset carries a passport: isin=%v name=%v currency=%v type=%v — the fields belong to an unknown-security row alone",
			m.BrokerISIN, m.BrokerName, m.BrokerCurrency, m.BrokerType)
	}
}

// When the broker will not say (404, a forgotten paper like the owner's
// TCS Group receipts), ISIN, name and currency are nil; the type still
// comes from the position.
func TestReconcileUnknownSecurityWithoutPassportSaysSo(t *testing.T) {
	f := newFixture(t)

	srv, _ := serve(t, map[string]route{
		portfolioPath: {status: http.StatusOK, body: []byte(
			`{"positions":[{"instrumentUid":"uid-forgotten","instrumentType":"share",` +
				`"quantity":{"units":"3","nano":0},"blocked":false}]}`)},
		positionsPath: {status: http.StatusOK, body: []byte(`{"money":[]}`)},
		instrumentByPath: {status: http.StatusNotFound, body: []byte(
			`{"code":5,"message":"Instrument not found","description":"50002"}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)

	r := NewReconciler(f.store, fakeJournal{}, newMarker(), instrument.NewStore(f.pool), nil, nil)
	res, err := r.ReconcileLink(f.ctx, c, f.conn, f.link)
	if err != nil {
		t.Fatalf("ReconcileLink: %v", err)
	}

	got := securitiesMismatches(res)
	if len(got) != 1 {
		t.Fatalf("got %d differences about papers, want exactly 1: %+v", len(got), got)
	}
	m := got[0]
	if m.BrokerISIN != nil || m.BrokerName != nil || m.BrokerCurrency != nil {
		t.Errorf("passport fields = %v/%v/%v, want all nil: the broker answered 404 and inventing "+
			"an empty passport would let a reader take blank for known",
			m.BrokerISIN, m.BrokerName, m.BrokerCurrency)
	}
	if m.BrokerType == nil || *m.BrokerType != string(instrument.TypeShare) {
		t.Errorf("broker_type = %v, want %q even without a passport — the position's own type is what "+
			"made this row an unknown security", m.BrokerType, instrument.TypeShare)
	}
}
