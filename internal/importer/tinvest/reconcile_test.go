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
	// The check asks the broker what an unmatched position IS, so that one
	// paper listed on two venues is not reported as two differences (see
	// Reconciler.matchByISIN). A test whose stub does not answer it is a test
	// whose broker position stays unmatched — which for most of these is the
	// case under test anyway.
	instrumentByPath = "/tinkoff.public.invest.api.contract.v1.InstrumentsService/GetInstrumentBy"
)

// -------------------------------------------------------------------------
// test fixtures: the journal side
// -------------------------------------------------------------------------

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

// byUID is the instrument index of a connection where nothing has drifted:
// every instrument found under the broker's instrument_uid, the figi side
// present and empty. Tests about drift build the index by hand instead.
func byUID(m map[string]uuid.UUID) InstrumentIndex {
	return InstrumentIndex{ByUID: m, ByFIGI: map[string]uuid.UUID{}}
}

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

// TestAnUnmappedBrokerPositionIsAMismatch pins decision 2 of the brief: the
// broker's instruments are matched against ours ONLY through the index the
// resolver has already built, and a SECURITY THIS PROGRAM ACCOUNTS FOR that is
// not in it is a difference with an honest label — never a silent skip.
//
// THE KIND IS ITS OWN, and that is the point of checking it here. Such a row
// used to be reported as MismatchInstrument, word for word what a paper both
// sides know but count differently gets — and the only thing separating them on
// screen was that the label was not a ticker from this catalog, which is a thing
// to NOTICE rather than a thing to be told. On the owner's own account these are
// the funds his TECH and TSPX were converted into, and the question they raise
// is what happened to that paper rather than which operations are missing.
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

// TestCashIsNotAPhantomSecurity is the sharpest test of the securities side.
// THE BROKER'S LIST OF POSITIONS IS NOT A LIST OF SECURITIES — the account's
// own cash stands in it, as a position of type "currency" (see
// testdata/portfolio_cash_only.json, a live sandbox account topped up with
// 50 000 ₽ and never traded, whose portfolio came back holding exactly that
// one position). Compared as a security it resolves to nothing of ours, so
// every account would carry a permanent phantom position under an unreadable
// label, could never reach "agrees", and would show the owner the very
// complaint this whole comparison was written to answer.
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

// TestCashIsStillComparedAsCash: passing a currency position over is a
// division of labour and not a blind spot. The same 50 000 ₽ the securities
// side ignores is compared by the money side, and disagreeing about it is a
// difference — otherwise the skip above would be a hole rather than a
// handover.
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

// TestAnAssetThisProgramCannotHoldIsItsOwnKindOfDifference: a future is not a
// share whose operations went missing. The owner really does hold it and this
// program really does not account for it, so passing it over would be silence
// where the comparison promises honesty — but calling it MismatchInstrument
// would send him looking for operations that are not missing. It gets its own
// kind so the screen can say the other sentence.
//
// The exact word "futures" is not what is being pinned here and was not
// checked against the live API: what decides the answer is that the type is
// not in brokerInstrumentTypes, and any word outside that table behaves the
// same way.
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

// TestBrokerLabelPrefersWhatAPersonReads: the fallbacks, in order. A row about
// a position that is not ours is the one row on this screen with no name of
// OURS behind it, so the broker's own naming is all there is — and an
// instrument_uid is a bare UUID.
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

// TestAPositionWhoseUIDDriftedIsFoundByItsFIGI pins the second identifier.
// The broker's instrument_uid on old operations has been seen to change,
// which is why the resolver looks its map up by instrument_uid and then by
// figi — so the operations behind this position were matched by figi and the
// journal is in perfect order. A comparison that knew only the first
// identifier would answer with TWO false lines: a phantom under the new uid
// and a "the broker has none of it" under our own instrument.
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

// TestTheUIDIsTriedBeforeTheFIGI: the order is the resolver's own
// ((*Resolver).lookupMap), so a position and the operations behind it resolve
// to the same instrument. Both identifiers hit here, and they name different
// instruments, so which one wins is visible.
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

// TestAPositionWithNoIdentifiersMatchesNothing: an entry under "" in either
// map would answer for every position that arrived without that identifier,
// resolving them all to a single instrument.
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

// TestACurrencyOnlyTheJournalKnowsIsAMismatch: a currency the broker did not
// mention is a currency the broker holds none of — its answer is a complete
// statement of its cash — so ours saying otherwise is a difference and not a
// gap to be passed over.
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

// TestMismatchesComeOutInAStableOrder must exercise the half of the walk that
// is unstable to mean anything: the differences found by iterating the
// ENGINE'S POSITIONS, which come out of a Go map whose iteration order is
// randomized on purpose. CCC and DDD are there for that — two instruments the
// journal holds and the broker does not report — because with the journal
// side empty the engine's map is empty too, that loop never runs, and the
// test would pin the order of the broker's slice alone, which was never in
// doubt.
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

// -------------------------------------------------------------------------
// ReconcileLink
// -------------------------------------------------------------------------

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

// newMarker is a marker over an account kept in rubles, which is what a link
// of this importer is required to name. Tests about the requirement set
// currency themselves.
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

	marker := newMarker()
	r := NewReconciler(f.store, fakeJournal{ops: []operation.Operation{
		aCashEntry(operation.TypeDeposit, 1_000_000, "RUB"),
		aBuy(inst, "100", -100_000, 10, "RUB"),
	}}, marker, instrument.NewStore(f.pool), nil)
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

// TestReconcileLinkAgreesWithAnAccountThatOnlyHoldsCash runs the owner's
// simplest possible case end to end, through the client, from the response a
// LIVE sandbox account actually returned: opened, topped up with 50 000 ₽,
// nothing bought. The portfolio comes back holding one position — the rubles
// themselves, of type "currency" — and the run must say the two sides agree.
//
// Before the securities side learned to tell cash from paper this returned
// "differs" with a phantom position labelled by a UUID, on an account whose
// whole history is one deposit. That is the complaint the whole import was
// written to answer, reproduced by the import itself.
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
	}}, marker, instrument.NewStore(f.pool), nil)

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

	marker := newMarker()
	r := NewReconciler(f.store, fakeJournal{}, marker, instrument.NewStore(f.pool), nil)

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
	r := NewReconciler(f.store, fakeJournal{}, marker, instrument.NewStore(f.pool), nil)

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
		// The broker knows nothing about it either: the position stays
		// unmatched, which is what this test's difference is made of.
		instrumentByPath: {status: http.StatusNotFound, body: []byte(
			`{"code":5,"message":"Instrument not found","description":"50002"}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)

	marker := newMarker()
	r := NewReconciler(f.store, fakeJournal{}, marker, instrument.NewStore(f.pool), nil)

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

	r := NewReconciler(f.store, fakeJournal{}, newMarker(), instrument.NewStore(f.pool), nil)
	res, err := r.ReconcileLink(f.ctx, NewClient(nil, "", "token", nil), f.conn, other)
	if !errors.Is(err, ErrLinkNotInConnection) {
		t.Fatalf("error = %v, want %v", err, ErrLinkNotInConnection)
	}
	if res.Status != ReconcileNotChecked {
		t.Errorf("status = %q, want %q", res.Status, ReconcileNotChecked)
	}
}

// TestReconcileLinkRefusesALinkOfAnotherSpace: the link names the space its
// journal is read from and its balance mark is filed under, so a link and a
// connection that disagree about it would check one household's broker
// account against another household's account. The client here has no server
// behind it — reaching one would already be the failure.
func TestReconcileLinkRefusesALinkOfAnotherSpace(t *testing.T) {
	f := newFixture(t)
	other := f.link
	other.SpaceID = uuid.New()

	marker := newMarker()
	r := NewReconciler(f.store, fakeJournal{}, marker, instrument.NewStore(f.pool), nil)
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

// TestReconcileLinkMarksNothingWhenOurOwnJournalCannotBeRead pins the last
// clause of ReconcileLink's promise about the mark: it is not written when
// this program's own database refused a read on the way. A broker figure
// nobody could compare anything against is still no reason to fail silently,
// so the run says "not checked" and the previous mark is left standing.
//
// This is NOT the same case as a journal the engine refuses (which does get a
// mark, since the broker's statement is true regardless): here the journal was
// never read at all.
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
	r := NewReconciler(f.store, fakeJournal{err: dbDown}, marker, instrument.NewStore(f.pool), nil)

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

// TestReconcileLinkRefusesToMarkANonRubleAccount pins the precondition this
// program used to only write down. The mark is a bare int64 whose currency is
// the account's own, and the figure being filed is the broker's RUBLES — so
// putting it on an account kept in anything else would file one currency's
// number under another's name, and nothing on the screen would say so. The
// refusal is loud and the mark is not written.
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
	r := NewReconciler(f.store, fakeJournal{}, marker, instrument.NewStore(f.pool), nil)

	_, err := r.ReconcileLink(f.ctx, c, f.conn, f.link)
	if !errors.Is(err, ErrAccountNotInRubles) {
		t.Fatalf("error = %v, want %v", err, ErrAccountNotInRubles)
	}
	if len(marker.marks) != 0 {
		t.Errorf("marks = %+v, want none: 8 000 ₽ under a dollar account is a wrong number", marker.marks)
	}
}

// TestABalanceMarkFinerThanAKopeckIsRefusedForWhatItIs: the substance of the
// refusal — a sum finer than a minor unit, which this program will not round
// into place — is the projection's, but the ACTION that failed is not.
// Nothing was being projected here; a balance mark was being written, and
// that is what the refusal has to say. A caption naming the wrong action is
// the failure this project has been bitten by four times.
func TestABalanceMarkFinerThanAKopeckIsRefusedForWhatItIs(t *testing.T) {
	f := newFixture(t)

	srv, _ := serve(t, map[string]route{
		portfolioPath: {status: http.StatusOK, body: []byte(`{"positions":[]}`)},
		// 8 000,005 ₽ — half a kopeck, which no whole number of kopecks holds.
		positionsPath: {status: http.StatusOK, body: []byte(
			`{"money":[{"currency":"rub","units":"8000","nano":5000000}]}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)

	marker := newMarker()
	r := NewReconciler(f.store, fakeJournal{}, marker, instrument.NewStore(f.pool), nil)

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

// TestBothRefusalsSurviveWhenBothHappened: our journal not computing and the
// mark failing to be written are two independent accidents with two different
// remedies. Returning only the second would leave whoever has to act on this
// looking at half of what went wrong.
func TestBothRefusalsSurviveWhenBothHappened(t *testing.T) {
	f := newFixture(t)
	inst := f.seedMapped(t, "uid-sber", "SBER")

	srv, _ := serve(t, map[string]route{
		portfolioPath: {status: http.StatusOK, body: []byte(`{"positions":[]}`)},
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
	r := NewReconciler(f.store, fakeJournal{ops: journal}, marker, instrument.NewStore(f.pool), nil)

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

// TestReconcileLinkReadsTheMapOfItsOwnConnection: the map is per connection,
// and a reconciliation that read another connection's rows would resolve the
// broker's positions against instruments this account never held.
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

// TestTheInstrumentIndexCarriesTheFIGIToo: the second identifier is read out
// of the same table the resolver writes, because a drifted instrument_uid is
// exactly what it is there for. An index built from the first column alone
// would leave the reconciliation unable to match a position the resolver
// itself matches without trouble.
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

// TestAFIGITwoRowsDisagreeAboutAnswersForNeither: when two rows of one
// connection carry one figi against DIFFERENT instruments there is no honest
// answer to give, and this index gives none — the position falls through to a
// difference, which is what "we could not match this" is supposed to look
// like. Keeping whichever row arrived last would be worse than that: rows come
// back in no particular order, so the answer would depend on how the database
// felt like returning them, and one run would match the position where the
// next reported it, with nothing changed in between.
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

	index, _, err := f.store.instrumentMap(f.ctx, f.conn.ID)
	if err != nil {
		t.Fatalf("instrumentMap: %v", err)
	}
	if _, ok := index.ByUID[""]; ok {
		t.Errorf("map = %v, want no answer under the empty identifier", index.ByUID)
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

// TestReconcileMatchesOnePaperListedOnTwoVenues is the owner's own screen, and
// what it looked like before this.
//
// A foreign share moved to another venue when trading in it was suspended: the
// broker's portfolio reports AMZN-RM while the history that built the journal
// named AMZN, and nothing connects the two identifiers. The check said the
// holding twice — "the broker has 20 and we have none", "we have 20 and the
// broker has none" — for seven of his papers at once, quantities agreeing in
// every one. Here the quantities agree, so after the pairing there is no
// difference at all.
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
	}}, newMarker(), instrument.NewStore(f.pool), nil)

	res, err := r.ReconcileLink(f.ctx, c, f.conn, f.link)
	if err != nil {
		t.Fatalf("ReconcileLink: %v", err)
	}
	// Asserted on the SECURITIES rows alone: this fixture's journal buys
	// dollars it was never given, so the cash comparison has a difference of
	// its own and it is not what is under test here.
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

// TestReconcileStillReportsARealDifferenceAcrossVenues is the other half, and
// the reason the pairing is worth doing: once the phantom pairs are gone, what
// is left is a difference somebody has to act on.
//
// The quantities here are the owner's Amazon: 1 in the journal against 20 at
// the broker, which is the 20-for-1 split of June 2022 that no operation ever
// reported. Pairing the listings is what makes that visible as ONE line about
// one paper instead of two lines that cancel in the reader's head.
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
	}}, newMarker(), instrument.NewStore(f.pool), nil)

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

// TestReconcileUnknownSecurityCarriesTheBrokersPassport: a position nothing of
// ours matched used to reach the screen as a bare broker ticker — «TECH2»,
// which is not a name of ours and says nothing about what the broker holds —
// although the check had already asked the broker exactly that and thrown the
// answer away. The row now carries the passport: ISIN, name, currency (in the
// uppercase shape a catalog row requires), and the instrument type translated
// by the importer's own table. This is the owner's live case: the fund his
// TECH was converted into, under an ISIN this catalog has no row for.
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

	r := NewReconciler(f.store, fakeJournal{}, newMarker(), instrument.NewStore(f.pool), nil)
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
	// Uppercased on the way in (InstrumentBrief), because that is the shape
	// CreateInstrumentRequest.currency requires — the whole point of carrying
	// the field is that a catalog row can be made from it as it stands.
	if m.BrokerCurrency == nil || *m.BrokerCurrency != "RUB" {
		t.Errorf("broker_currency = %v, want RUB", m.BrokerCurrency)
	}
	if m.BrokerType == nil || *m.BrokerType != string(instrument.TypeETF) {
		t.Errorf("broker_type = %v, want %q — our own type word, translated by the importer's table",
			m.BrokerType, instrument.TypeETF)
	}
}

// TestReconcileUnsupportedAssetCarriesNoPassport: the passport is attached to
// an unknown-security row and to nothing else, and this is the case that says
// so. A future is an asset this program does not account for at all, and the
// check asks the broker about it exactly as it asks about any position nothing
// of ours matched — so a passport for it EXISTS by the time the row is built,
// and only the kind stops it reaching the screen. Publishing it would offer a
// reader the makings of a catalog row for a paper no rule of this program can
// book, under a line that says the opposite.
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

	r := NewReconciler(f.store, fakeJournal{}, newMarker(), instrument.NewStore(f.pool), nil)
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

// TestReconcileUnknownSecurityWithoutPassportSaysSo: when the broker will not
// say what its own position is (404 — the live case is a paper the broker
// forgot, like the owner's TCS Group receipts), the row carries no ISIN, no
// name and no currency — an explicit «the passport was not obtained», not a
// passport of empty strings. The TYPE is still published: it comes off the
// position itself, and it is the very fact that classified the row as an
// unknown security rather than an unsupported asset.
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

	r := NewReconciler(f.store, fakeJournal{}, newMarker(), instrument.NewStore(f.pool), nil)
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
