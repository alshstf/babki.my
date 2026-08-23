package tinvest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

// ErrAccountNotInRubles means the babki account a link names is not kept in
// rubles, so the broker's own ruble figure has no business being filed as its
// balance mark: a mark carries no currency of its own (see
// account.Store.SetBalance) and would be read back as whatever the account is
// denominated in. See ReconcileLink on why the check lives at the write rather
// than only in the path that creates such accounts.
var ErrAccountNotInRubles = errors.New("tinvest: the linked account is not kept in rubles")

// ErrBalanceMarkRefused means the broker's ruble balance could not become a
// balance mark: it is either finer than a minor unit — this program does not
// round money into place — or larger than any sum it holds. It is its own
// sentinel rather than the projection's refusal because the two describe
// different work: nothing was being projected when this happened, a balance
// mark was being written.
var ErrBalanceMarkRefused = errors.New("tinvest: the broker's ruble balance cannot be a balance mark")

// Reconciliation is the point of the whole import: after every sync this
// program computes the account's positions ITSELF, from the journal it wrote,
// and compares them with what the broker says it holds. Agreement is stated
// with the moment it was established; disagreement is shown line by line; and
// a check that could not be made says so and draws no tick.
//
// None of the competing products does this (research of 2026-08-04), and the
// two complaints their users make most — "the portfolio does not match my
// broker" and "phantom positions appeared after the import" — are both
// answered by exactly this comparison.
//
// WHAT IS COMPARED IS QUANTITIES AND CASH, NEVER VALUATIONS. A valuation is
// the broker's own rates and its own method applied to the same holding, so
// two honest programs differ on it constantly and the difference means
// nothing. A difference in the NUMBER OF UNITS of a security, or in the money
// standing on the account, is a fact: one of the two sides is wrong about
// what happened.
//
// THE BROKER'S TWO "BLOCKED" FIELDS ARE NOT THE SAME KIND OF THING, and this
// is the trap this file is built around:
//
//   - A SECURITY's Quantity is already the whole position, and its Blocked is
//     a BOOLEAN — the paper is halted at the depository. Adding anything for
//     it would overstate every halted holding, and halted holdings are not a
//     hypothetical case for this owner: he holds frozen FinEx and SPB paper
//     (whether any of it sits at this particular broker is not something this
//     file knows).
//   - MONEY's two figures are two ADDENDS: the free balance and the amount
//     held by standing orders together make the balance. Reading only the
//     free part would report a false difference on every account with an
//     order open.
//
// See PortfolioPosition and MoneyBalance in client.go, whose doc comments
// carry the wire-level evidence for both halves.

// Kinds of difference. Kind is a plain string rather than a named type because
// it travels into a jsonb column and out of the API, and what reads it there
// has to tell a security's row from a currency's: the two carry different
// things (one has an instrument of ours behind it, the other a currency code)
// and are read differently.
const (
	// MismatchInstrument: a number of units differs, or one side names a
	// security the other does not. When the broker names one we could not
	// match, this kind is the evidence that some of its operations did not
	// project — the security IS one this program accounts for, so the gap is
	// in the journal.
	MismatchInstrument = "instrument"
	// MismatchCurrency: a cash balance in one currency differs.
	MismatchCurrency = "currency"
	// MismatchUnsupported: the broker holds an asset of a kind this program
	// does not account for at all — a future, an option, anything whose
	// instrument_type is outside brokerInstrumentTypes. Cash is the one thing
	// outside that table which does NOT land here: it is the account's own
	// money and is compared as money (see compareInstruments).
	//
	// A SEPARATE KIND BECAUSE IT IS A SEPARATE STATEMENT: MismatchInstrument
	// means "part of your history did not parse", which invites looking for
	// the missing operations, while this one means "this asset is outside
	// what the program can hold", which no amount of re-importing will
	// change. Reporting it is not optional — the
	// owner does hold what it names, and passing over it would be the silence
	// this comparison exists to replace — but calling it the other thing
	// would send him looking for operations that are not missing.
	MismatchUnsupported = "unsupported"
	// MismatchUnknownSecurity: the broker holds a security of a kind this
	// program DOES account for, and nothing of ours corresponds to it — no
	// operation on that paper has ever reached the journal, so there is no row
	// in the instrument map to pair it with.
	//
	// A THIRD STATEMENT BECAUSE IT IS A THIRD SITUATION, and the label alone
	// does not tell it from the others. On the owner's own account the broker
	// reports TECH2, TSPX2 and TUSD2 — funds his TECH and TSPX were converted
	// into, under new ISINs — and the row read "the broker has it and we do
	// not", exactly as a paper we both know but count differently does. The
	// only clue was that the ticker was not one of his, which is a thing to
	// NOTICE rather than a thing to be told.
	//
	// What it invites is a different question, too. MismatchInstrument asks
	// "which operations are missing"; this one asks "what happened to this
	// paper" — a corporate action nobody recorded, and an answer that is the
	// owner's to give rather than a rule's to find.
	MismatchUnknownSecurity = "unknown_security"
)

// The verdicts beyond ReconcileNotChecked, which lives in store.go because a
// run carries it from the moment it is created.
const (
	// ReconcileMatched: nothing differed — every security's quantity and
	// every currency's balance agreed, and the broker named no asset this
	// program cannot account for.
	ReconcileMatched ReconcileStatus = "matched"
	// ReconcileMismatched: at least one did not, and ReconcileResult says
	// which.
	ReconcileMismatched ReconcileStatus = "mismatched"
)

// ReconcileMismatch is one thing the two sides disagree about, carrying BOTH
// figures: what the broker says and what our journal computes. A row that
// carried only the difference would leave a person unable to tell which side
// to go and look at.
//
// InstrumentID is nil on a currency row, on an unsupported one, and also on a
// security the connection's instrument index does not resolve — there is no
// instrument of ours to name, which is itself the news (see CompareHoldings).
// Label is what a person reads: our instrument's ticker or name when it is
// ours (its id, when the catalog gave neither), the broker's own naming of a
// position that is not ours (see brokerLabel), or a currency code.
//
// THE FOUR Broker* FIELDS ARE THE BROKER'S PASSPORT OF A POSITION THAT IS NOT
// OURS, and they are set on a MismatchUnknownSecurity row alone. Such a row
// used to carry nothing but the broker's ticker — «TECH2», which is not a
// ticker of ours and says nothing about WHAT the broker holds — while the
// check had already asked the broker exactly that (see matchByISIN) and
// thrown the answer away the moment no catalog row carried its ISIN. They are
// pointers so that a row can say «the passport was not obtained» (all nil:
// the broker answered 404, or the position was past the lookup cap) apart
// from a passport whose field happened to be empty; and so that runs recorded
// before these fields existed — whose jsonb has no such keys — read back as
// exactly that, rather than as a passport of empty strings.
//
// BrokerType is NOT the broker's own word but this catalog's InstrumentType,
// translated by brokerInstrumentTypes — the one table the resolver books by
// and unmatchedKind classifies by. It is set from the position's type, not
// from the passport's, because the position's type is what made the row an
// unknown security in the first place (unmatchedKind), so it is in the table
// by construction; the other three come from the passport and are nil when
// none was obtained. A client that wants to create the catalog row therefore
// has every field CreateInstrumentRequest needs, in the shape it needs them,
// without a second copy of the type table on its side.
type ReconcileMismatch struct {
	Kind           string          `json:"kind"`
	InstrumentID   *uuid.UUID      `json:"instrument_id,omitempty"`
	Label          string          `json:"label"`
	Broker         decimal.Decimal `json:"broker"`
	Journal        decimal.Decimal `json:"journal"`
	BrokerISIN     *string         `json:"broker_isin,omitempty"`
	BrokerName     *string         `json:"broker_name,omitempty"`
	BrokerCurrency *string         `json:"broker_currency,omitempty"`
	BrokerType     *string         `json:"broker_type,omitempty"`
}

// ReconcileResult is one reconciliation's whole verdict.
//
// STATUS IS DERIVED FROM MISMATCHES AND NEVER KEPT BESIDE IT: "matched" means
// the list is empty, by construction rather than by two computations that
// happen to agree today (the rule this codebase states in its package docs and
// has been bitten by ignoring). ReconcileNotChecked is the one status the
// comparison itself does not produce — it means no comparison was made.
//
// A value assembled by hand can still say one thing and carry another, so the
// write refuses that pair rather than storing it: see
// ErrReconcileVerdictContradictsItself.
type ReconcileResult struct {
	Status     ReconcileStatus     `json:"status"`
	Mismatches []ReconcileMismatch `json:"mismatches"`
}

// InstrumentIndex is what one connection has already learned about the
// broker's instruments: which catalog instrument of ours each of the broker's
// identifiers stands for.
//
// THERE ARE TWO MAPS BECAUSE THE BROKER'S IDENTIFIERS DRIFT. An
// instrument_uid on old operations has already been seen to change (see
// InstrumentRef), which is the whole reason the resolver looks the map up by
// instrument_uid and then by figi rather than by one of them. A comparison
// that knew only the first would answer a drifted position with TWO false
// lines at once — a phantom "the broker has 100, the journal 0" under the new
// identifier and a "the broker has 0, the journal 100" under our instrument —
// while the journal behind them was in perfect order, because the resolver
// had matched those very operations by figi.
type InstrumentIndex struct {
	ByUID  map[string]uuid.UUID
	ByFIGI map[string]uuid.UUID
}

// lookup finds the instrument of ours a broker position stands for: by
// instrument_uid first and by figi second — the order (*Resolver).lookupMap
// uses, so that a position and the operations behind it are matched by the
// same identifier and end up on the same instrument.
//
// The two are not identical in every case, and the one place they part is
// deliberate: where several map rows claim one figi against DIFFERENT
// instruments, the resolver still picks one (the most recently updated row)
// and this index answers nothing at all — see instrumentMap. Guessing there
// would put a confident wrong match on the screen, while answering nothing
// shows the position as a difference, which is what "we could not match this"
// is supposed to look like.
//
// An empty identifier matches nothing rather than looking itself up: an entry
// under "" would answer for every position that arrived without one, and
// resolve them all to a single instrument.
func (ix InstrumentIndex) lookup(p PortfolioPosition) (uuid.UUID, bool) {
	if p.InstrumentUID != "" {
		if id, ok := ix.ByUID[p.InstrumentUID]; ok {
			return id, true
		}
	}
	if p.FIGI != "" {
		if id, ok := ix.ByFIGI[p.FIGI]; ok {
			return id, true
		}
	}
	return uuid.Nil, false
}

// CompareHoldings compares what the broker says an account holds against what
// our journal says, and is a pure function of its arguments: no database, no
// clock, no network.
//
// SECURITIES ARE MATCHED THROUGH index AND NOTHING ELSE — what the resolver
// has already built for this connection (decision 2 of the task brief). A
// broker position that is in neither of its maps is reported as a difference
// under the broker's own naming, never passed over: nothing of ours
// corresponds to it, which usually means some of its operations did not
// project, and a silent skip would turn the most useful evidence this
// comparison can produce into nothing at all.
//
// TWO KINDS OF POSITION ARE NOT SECURITIES OF OURS AND ARE NOT COMPARED AS
// ONE. A position of type "currency" is the account's own cash, which the
// money half of this comparison handles instead, and a position whose type
// this program does not account for at all gets MismatchUnsupported rather
// than MismatchInstrument. Both are decided in compareInstruments, where the
// reasoning sits next to the code.
//
// labels supply the name to show per instrument of ours; an instrument with no
// label is named by its id, since a poor label is no reason to withhold a
// difference.
//
// A JOURNAL THE ENGINE REFUSES YIELDS ReconcileNotChecked. Our own side of the
// comparison could not be computed at all, and the only two other answers
// available — "agrees" or "here is what differs" — would both be claims about
// a comparison that never happened. The reason is not lost: the reconciler
// itself calls compareHoldings, which returns it.
func CompareHoldings(brokerPositions []PortfolioPosition, brokerBalances []MoneyBalance,
	journal []operation.Operation, index InstrumentIndex,
	labels map[uuid.UUID]string,
) ReconcileResult {
	res, _ := compareHoldings(brokerPositions, brokerBalances, journal, index, labels, nil)
	return res
}

// compareHoldings is CompareHoldings with the engine's refusal kept, for the
// caller inside this package that logs and returns it — and with the
// passports the check obtained for positions nothing of ours matched, keyed by
// instrument_uid (see matchByISIN), which CompareHoldings has none of.
func compareHoldings(brokerPositions []PortfolioPosition, brokerBalances []MoneyBalance,
	journal []operation.Operation, index InstrumentIndex,
	labels map[uuid.UUID]string, passports map[string]InstrumentBrief,
) (ReconcileResult, error) {
	positions, err := portfolio.Compute(journal)
	if err != nil {
		return ReconcileResult{Status: ReconcileNotChecked}, fmt.Errorf(
			"tinvest: reconcile: the journal itself does not compute, so there was nothing to compare: %w", err)
	}

	mismatches := compareInstruments(brokerPositions, positions, index, labels, passports)
	mismatches = append(mismatches, compareCash(brokerBalances, journal)...)
	sortMismatches(mismatches)

	status := ReconcileMatched
	if len(mismatches) > 0 {
		status = ReconcileMismatched
	}
	return ReconcileResult{Status: status, Mismatches: mismatches}, nil
}

// brokerTypeCurrency is the instrument_type the broker gives its own cash
// positions. It is deliberately absent from brokerInstrumentTypes (see the
// long note there on why a currency must never reach the resolver); here it
// is needed by name, because cash arriving in the list of positions has to be
// recognized to be left to the half of this comparison that handles it.
const brokerTypeCurrency = "currency"

// compareInstruments compares the number of UNITS of every security either
// side names.
//
// passports is what the broker said the unmatched positions ARE, keyed by
// instrument_uid; nil when nothing was asked (CompareHoldings). It fills the
// Broker* fields of an unknown-security row and decides nothing: a position
// is matched or not by the index alone, as before.
func compareInstruments(brokerPositions []PortfolioPosition, positions map[uuid.UUID]*portfolio.Position,
	index InstrumentIndex, labels map[uuid.UUID]string, passports map[string]InstrumentBrief,
) []ReconcileMismatch {
	out := []ReconcileMismatch{}
	compared := make(map[uuid.UUID]bool, len(brokerPositions))

	for _, p := range brokerPositions {
		// THE BROKER'S LIST OF POSITIONS IS NOT A LIST OF SECURITIES: the
		// account's cash stands in it too, as a position of type "currency".
		// Checked on a live sandbox account that was topped up with 50 000 ₽
		// and never traded — its portfolio came back holding exactly one
		// position, the rubles (2026-08-05, testdata/portfolio_cash_only.json).
		//
		// PASSING IT OVER IS NOT A GAP BUT A DIVISION OF LABOUR: compareCash
		// below compares this account's cash, currency by currency, against
		// what the journal accounts for — from GetPositions, the broker's own
		// statement of the same money. Comparing it here as well would count
		// the owner's cash twice, once correctly and once as a security
		// nothing of ours corresponds to, so every account holding any cash
		// would carry a permanent phantom position and could never reach
		// "agrees".
		if p.InstrumentType == brokerTypeCurrency {
			continue
		}

		// THE BROKER'S QUANTITY IS ALREADY THE WHOLE POSITION and its Blocked
		// is a boolean — the paper is halted at the depository — so nothing is
		// added for it here. Adding one would overstate every halted holding
		// by exactly the flag. (Money is the other way round; see
		// compareCash.)
		brokerQty := p.Quantity.Decimal()

		id, ok := index.lookup(p)
		if !ok {
			m := ReconcileMismatch{
				Kind:    unmatchedKind(p),
				Label:   brokerLabel(p),
				Broker:  brokerQty,
				Journal: decimal.Zero,
			}
			if m.Kind == MismatchUnknownSecurity {
				attachPassport(&m, p, passports)
			}
			out = append(out, m)
			continue
		}
		compared[id] = true

		ours := decimal.Zero
		if pos, found := positions[id]; found {
			ours = pos.Quantity
		}
		if brokerQty.Equal(ours) {
			continue
		}
		instrumentID := id
		out = append(out, ReconcileMismatch{
			Kind:         MismatchInstrument,
			InstrumentID: &instrumentID,
			Label:        instrumentLabel(labels, id),
			Broker:       brokerQty,
			Journal:      ours,
		})
	}

	for id, pos := range positions {
		// A position sold out to the last unit stays in the engine's answer
		// with a quantity of zero, and the broker does not report such a thing
		// at all. Calling that a difference would put a permanent false alarm
		// on the screen of anyone who ever closed a trade.
		if compared[id] || pos.Quantity.IsZero() {
			continue
		}
		instrumentID := id
		out = append(out, ReconcileMismatch{
			Kind:         MismatchInstrument,
			InstrumentID: &instrumentID,
			Label:        instrumentLabel(labels, id),
			Broker:       decimal.Zero,
			Journal:      pos.Quantity,
		})
	}
	return out
}

// compareCash compares the money standing on the account, currency by
// currency.
//
// BOTH SIDES ARE STATED IN WHOLE CURRENCY UNITS — rubles, not kopecks — which
// is how the broker states its own and how a person reads the row. Only OUR
// side crosses over, and that crossing is exact: every journal figure is a
// whole number of minor units, so shifting the decimal point by minorUnitScale
// rounds nothing and invents nothing.
//
// THE CROSSING IN THE OTHER DIRECTION IS NOT EXACT, which is why this
// function does not make it: the gateway's amounts carry nine decimal places,
// and a tenth of a kopeck cannot become a whole number of minor units without
// a decision this function has no business making. (The balance mark does have
// to make that crossing, and it refuses outright rather than rounding — see
// markBalance.)
//
// A CURRENCY ONLY ONE SIDE MENTIONS IS COMPARED AGAINST ZERO rather than
// skipped: the broker's answer is a complete statement of its cash, so a
// currency absent from it is a currency it holds none of, and our journal
// claiming otherwise is exactly the kind of difference this exists to show.
//
// OUR SIDE IS THE WHOLE ACCOUNT, not the imported part of it. The journal
// handed over is the account's entire journal (the reconciler reads it through
// ListForEngine), so a deposit somebody typed in by hand on an imported account
// stands in this sum, the broker knows nothing about it, and the currency shows
// up here as a difference — with a label naming the currency and nothing about
// where the extra money came from. That is right under the precondition this
// whole importer is built on, which is the owner's own decision: an import
// feeds ACCOUNTS OF ITS OWN, so everything in one of those accounts is
// something the broker reported and the comparison is between two statements
// about the same thing. Summing only the imported rows would be worse under
// that precondition and no better without it: the sum would agree with the
// broker while the account itself held money the broker never reported, and
// nothing anywhere would say so. See the rebuild's own note on the precondition
// and where it belongs (rebuild.go's heading).
func compareCash(brokerBalances []MoneyBalance, journal []operation.Operation) []ReconcileMismatch {
	broker := make(map[string]decimal.Decimal, len(brokerBalances))
	for _, b := range brokerBalances {
		// THE MONEY HALF OF THE ASYMMETRY: the free balance and the amount
		// held by standing orders are two ADDENDS of one balance (see
		// MoneyBalance), unlike a security's boolean Blocked, which is not a
		// quantity at all. Summing per currency rather than assigning, because
		// GetPositions is free to name a currency in either of its two lists.
		broker[b.Currency] = broker[b.Currency].Add(b.Value).Add(b.Blocked)
	}
	ours := journalCashMinor(journal)

	out := []ReconcileMismatch{}
	for _, code := range currencyUnion(broker, ours) {
		theirs := broker[code]
		mine := ours[code].Shift(-minorUnitScale)
		if theirs.Equal(mine) {
			continue
		}
		out = append(out, ReconcileMismatch{
			Kind:    MismatchCurrency,
			Label:   code,
			Broker:  theirs,
			Journal: mine,
		})
	}
	return out
}

// journalCashMinor is the cash the journal accounts for, per currency, in
// minor units: the sum of the entries' amounts less the sum of their fees.
//
// THIS IS A NEW COMPUTATION AND THE ONLY ONE OF ITS KIND HERE. An account's
// balance on the accounts screen is a mark somebody entered by hand, not
// anything derived from the journal, so there is no second implementation of
// this to check against — which is why it is pinned by a test with the
// figures written out rather than by agreement with something else.
//
// A buy carries its commission beside the amount rather than inside it (see
// the projection's projectTrade), so the fee is subtracted here; a standalone
// charge — a service fee, a tax — is a negative amount with no fee of its own,
// and is therefore counted once, by the same formula.
//
// TRANSFERS ARE NOT CASH AND ARE LEFT OUT. A transfer's AmountMinor is the
// cost basis that travelled with the shares, with no cash meaning at all (see
// portfolio.Operation) — and this importer does produce transfers, for shares
// moving between the owner's own accounts. Summing that basis as money would
// invent a balance nobody has and hang a false difference on the account for
// good.
//
// WHICH TYPES THOSE ARE IS NOT DECIDED HERE. portfolio.MovesCash answers it, and
// this function asks rather than keeping a list of its own: the engine's own
// cash fold asks the same question, and two hand-maintained lists of "types that
// are not money" would agree only until one of them learned about a new type.
// A conversion of one paper into another is the type that arrived after this was
// written, its two legs both on THIS account, and a list kept here would have
// counted its basis as cash twice and hung a difference against the broker that
// nothing on the screen could explain.
//
// The running total is a decimal rather than an int64 because a sum of
// arbitrarily many entries has no bound of its own, and a wrapped int64 is a
// plausible-looking figure of the wrong sign. Nothing is rounded: every term
// is a whole number of minor units.
func journalCashMinor(journal []operation.Operation) map[string]decimal.Decimal {
	cash := make(map[string]decimal.Decimal)
	for _, o := range journal {
		if !portfolio.MovesCash(o) {
			continue
		}
		cash[o.Currency] = cash[o.Currency].
			Add(decimal.NewFromInt(o.AmountMinor)).
			Sub(decimal.NewFromInt(o.FeeMinor))
	}
	return cash
}

// currencyUnion is every currency either side names, sorted.
func currencyUnion(broker, ours map[string]decimal.Decimal) []string {
	seen := make(map[string]bool, len(broker)+len(ours))
	codes := make([]string, 0, len(broker)+len(ours))
	for _, m := range []map[string]decimal.Decimal{broker, ours} {
		for code := range m {
			if seen[code] {
				continue
			}
			seen[code] = true
			codes = append(codes, code)
		}
	}
	sort.Strings(codes)
	return codes
}

// unmatchedKind decides what a broker position that resolves to no instrument
// of ours is: a security this program could hold but has never seen an
// operation on (MismatchUnknownSecurity), or an asset of a kind it does not
// account for at all (MismatchUnsupported).
//
// THE ANSWER IS READ OFF brokerInstrumentTypes AND NOTHING ELSE — the same
// table the resolver refuses by, so the screen cannot come to call an asset
// unsupported that the importer would happily book, or the other way round.
// A second list of "types we do not support" would be exactly the pair of
// independent computations of one thing that this codebase has watched drift.
//
// The type read here comes off the portfolio position and the resolver reads
// it off the instrument's passport; they are the same field of the same API,
// the broker's instrument_type, which is why one table can answer for both.
func unmatchedKind(p PortfolioPosition) string {
	if _, supported := brokerInstrumentTypes[p.InstrumentType]; supported {
		return MismatchUnknownSecurity
	}
	return MismatchUnsupported
}

// attachPassport fills the Broker* fields of an unknown-security row: the
// type from the position itself, translated by the one table unmatchedKind
// has just classified it by (so the lookup cannot miss — a type outside the
// table is MismatchUnsupported and never reaches here; the guard is for the
// day that invariant is broken, and it then publishes no type rather than an
// empty one), and ISIN, name and currency from the passport when one was
// obtained. An empty field of an obtained passport stays nil too: a blank
// ISIN is nothing a catalog row can be made from, and publishing "" would let
// a client build one.
func attachPassport(m *ReconcileMismatch, p PortfolioPosition, passports map[string]InstrumentBrief) {
	if t, ok := brokerInstrumentTypes[p.InstrumentType]; ok {
		s := string(t)
		m.BrokerType = &s
	}
	brief, ok := passports[p.InstrumentUID]
	if !ok {
		return
	}
	m.BrokerISIN = nonEmpty(brief.ISIN)
	m.BrokerName = nonEmpty(brief.Name)
	m.BrokerCurrency = nonEmpty(brief.Currency)
}

// nonEmpty is a pointer to s, or nil when s is blank.
func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// brokerLabel names a broker position that resolves to nothing of ours, in
// the words a person is likeliest to recognize: the ticker, then the figi,
// then the identifiers that exist for machines. Ordered this way because such
// a row is the one thing on this screen with no name of OURS behind it, and an
// instrument_uid is a bare UUID — true, and unreadable.
//
// All four being empty would mean the broker returned a position it did not
// identify at all; the difference is still reported, because a badly labelled
// difference is news and a swallowed one is not.
func brokerLabel(p PortfolioPosition) string {
	switch {
	case p.Ticker != "":
		return p.Ticker
	case p.FIGI != "":
		return p.FIGI
	case p.InstrumentUID != "":
		return p.InstrumentUID
	default:
		return p.InstrumentType
	}
}

// instrumentLabel is what to call one of our instruments, falling back to its
// id when the caller supplied no label for it.
func instrumentLabel(labels map[uuid.UUID]string, id uuid.UUID) string {
	if l := labels[id]; l != "" {
		return l
	}
	return id.String()
}

// sortMismatches puts the differences in an order that does not change between
// two runs over the same data. Half of them are found by walking the engine's
// positions, which come out of a map, and Go randomizes map iteration on
// purpose — so without this the rows would reshuffle on every refresh and each
// reshuffle would look like news.
func sortMismatches(m []ReconcileMismatch) {
	sort.Slice(m, func(i, j int) bool {
		if m[i].Kind != m[j].Kind {
			return m[i].Kind < m[j].Kind
		}
		if m[i].Label != m[j].Label {
			return m[i].Label < m[j].Label
		}
		return idString(m[i].InstrumentID) < idString(m[j].InstrumentID)
	})
}

func idString(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// balanceMarker is the balance mark this reconciliation leaves on the account
// — a narrow local interface for the reason this package declares the others
// (see journalDelta in rebuild.go). *account.Store satisfies it.
//
// IT READS THE ACCOUNT AS WELL AS WRITING THE MARK, because a mark is a bare
// int64 whose currency is the account's own (see account.Store.SetBalance) and
// this reconciliation only ever has rubles to file — so what currency the
// account keeps decides whether the mark may be written at all. An interface
// with the write and not the read could not ask, and this program would have
// no way to keep the promise it makes in ReconcileLink's doc comment.
type balanceMarker interface {
	ByID(ctx context.Context, spaceID, id uuid.UUID) (account.WithBalance, error)
	SetBalance(ctx context.Context, spaceID, accountID uuid.UUID, asOf time.Time, amountMinor int64) error
}

// engineReader is the journal of one account, in the order the engine reads
// it. *operation.Store satisfies it.
type engineReader interface {
	ListForEngine(ctx context.Context, spaceID, accountID uuid.UUID) ([]operation.Operation, error)
}

// Reconciler compares one linked account against the broker and records what
// the broker said the account is worth.
type Reconciler struct {
	store    *Store
	ops      engineReader
	accounts balanceMarker
	// catalog answers "which of our rows is this ISIN", which is how a broker
	// position under a listing this connection never imported is recognized as
	// a paper the owner does hold (see matchByISIN).
	catalog isinCatalog
	log     *slog.Logger
	// now stands in for time.Now so a test can pin the day a mark is filed
	// under instead of racing the wall clock (the pattern
	// marketdata.backfillFxWorker uses).
	now func() time.Time
}

func NewReconciler(store *Store, ops engineReader, accounts balanceMarker, catalog isinCatalog, log *slog.Logger) *Reconciler {
	if log == nil {
		log = slog.Default()
	}
	return &Reconciler{store: store, ops: ops, accounts: accounts, catalog: catalog, log: log, now: time.Now}
}

// isinCatalog is the narrow view of instrument.Store the reconciliation needs:
// one question, asked about a security the broker named and this program may
// already hold under another of its listings.
type isinCatalog interface {
	ByISIN(ctx context.Context, isin string) (instrument.Instrument, error)
}

// ReconcileLink checks one linked account against the broker and, when the
// broker answered, marks the account's balance with the figure the broker
// itself named.
//
// THE MARK IS THE BROKER'S OWN RUBLES — free plus blocked — and not any sum of
// ours (the owner's decision of 2026-08-04). An imported account is one nobody
// will ever type a balance mark into by hand, and the accounts screen shows
// that mark and not a derivation, so without this the screen would show an
// imported account as empty. It is filed under the Moscow day, which is the
// calendar this importer files its journal entries under.
//
// The mark is written whenever the broker ANSWERED and this program got as far
// as comparing — including when the comparison then found differences, and
// including when our own journal turned out not to compute: what the broker
// says it holds is no less true for our side of it being wrong or missing. It
// is not written when the broker did not answer, because then there is no
// figure of the broker's to write and the previous mark is better left
// standing than replaced by a guess; nor when this program's own database
// refused a read on the way, where the mark's write would fail with it.
//
// THE ACCOUNT MUST BE A RUBLE ACCOUNT, AND THAT IS CHECKED HERE rather than
// trusted. The mark is a bare int64 in the account's own currency (see
// account.Store.SetBalance), so writing rubles into an account denominated in
// anything else would file one currency's figure under another's name — a
// wrong number with nothing on the screen to say so. Accounts for this
// importer are created by the path that makes a link, and that path is where
// the requirement belongs; but a precondition only ever written down is one
// that the caller wiring that path, or a link made against an account that
// already existed, can break in silence — so this refuses
// (ErrAccountNotInRubles) instead of marking. (An account's own currency
// cannot change after it is created: account.Update carries no currency
// field. What can change is which account a link points at.) Cash in other
// currencies is not lost from sight either way: it is compared like any other
// and shows up in the differences.
//
// A broker that did not answer yields ReconcileNotChecked and the error, which
// is a different thing from "no differences" and must be shown as one.
// passportLookupsPerReconcile bounds how many of the broker's own positions one
// check asks a passport for. Each is a request; the set is the positions this
// connection has no mapping for, which on a healthy account is empty and on the
// owner's is fourteen. Whatever the cap leaves out stays reported as unmatched,
// which is the honest answer for a position nothing was learned about — and the
// count that was skipped is logged rather than passed over.
const passportLookupsPerReconcile = 40

// matchByISIN teaches the index the broker positions it does not already know,
// by asking the broker what paper each one IS and looking that paper up in this
// program's own catalog.
//
// ONE PAPER, TWO LISTINGS, TWO ROWS OF NONSENSE. When a foreign share was moved
// to another venue after trading in it was suspended, the broker's portfolio
// reports the new listing while the history that built the journal named the old
// one. Nothing connects the two identifiers, so the check reported the holding
// TWICE — once as "the broker has 20 and we have none", once as "we have 20 and
// the broker has none" — for seven of the owner's papers at once, with the
// quantities agreeing in every one of them. A list like that is not read.
//
// THE ISIN IS WHAT IDENTIFIES A SECURITY, which is why it and nothing else is
// matched on. A ticker is not unique across venues or issuers, and matching on
// one would file a stranger's position against the owner's paper.
//
// NOTHING IS WRITTEN DOWN. The learned pairs live for this one comparison. The
// instrument map is what the IMPORT resolves operations through, and a pairing
// put there — however sound — would decide where future trades are booked, on
// the strength of a check whose whole job is to look and report.
//
// THE THIRD VALUE IS THE PASSPORTS THAT PAIRED NOTHING, keyed by
// instrument_uid: what the broker said a position is when no catalog row of
// ours carries that ISIN. Before it existed the check asked the question,
// received the answer, and dropped it — and the row then went to the screen
// under the broker's bare ticker, leaving the reader to notice that «TECH2»
// is not a name of ours and to go and look it up by hand. A passport the
// broker would not give (404) is simply absent from the map, and the row says
// so by carrying nothing. Positions past the lookup cap are absent for the
// same reason.
func (r *Reconciler) matchByISIN(ctx context.Context, c *Client, index InstrumentIndex,
	labels map[uuid.UUID]string, positions []PortfolioPosition,
) (InstrumentIndex, map[uuid.UUID]string, map[string]InstrumentBrief) {
	unknown := make([]PortfolioPosition, 0, len(positions))
	for _, p := range positions {
		if p.InstrumentType == brokerTypeCurrency {
			continue
		}
		if _, ok := index.lookup(p); !ok && p.InstrumentUID != "" {
			unknown = append(unknown, p)
		}
	}
	unpaired := map[string]InstrumentBrief{}
	if len(unknown) == 0 {
		return index, labels, unpaired
	}
	if len(unknown) > passportLookupsPerReconcile {
		r.log.Info("tinvest: more unmatched broker positions than one check looks up, the rest stay unmatched",
			"unmatched", len(unknown), "looked_up", passportLookupsPerReconcile)
		unknown = unknown[:passportLookupsPerReconcile]
	}

	// Copies, so a failure halfway leaves the caller's own index untouched and
	// so nothing learned here can outlive this comparison.
	byUID := make(map[string]uuid.UUID, len(index.ByUID)+len(unknown))
	for k, v := range index.ByUID {
		byUID[k] = v
	}
	grown := InstrumentIndex{ByUID: byUID, ByFIGI: index.ByFIGI}
	grownLabels := make(map[uuid.UUID]string, len(labels))
	for k, v := range labels {
		grownLabels[k] = v
	}

	for _, p := range unknown {
		brief, err := c.InstrumentByUID(ctx, p.InstrumentUID)
		if err != nil {
			r.log.Debug("tinvest: the broker would not say what one of its own positions is",
				"instrument_uid", p.InstrumentUID, "err", err)
			continue
		}
		if brief.ISIN == "" {
			// NOT A GUARD AGAINST A WRONG MATCH — instrument.Store.ByISIN
			// refuses an empty ISIN itself, and one rule kept in two places is
			// how the two eventually disagree. This is here so the LOG says
			// which of two different things happened: the broker would not say
			// what its own position is, or it said and nothing of ours carries
			// that ISIN. Those send a reader to different places.
			//
			// The passport is kept all the same: a name and a currency are
			// still what the broker said, and the row is allowed to show them
			// — it just cannot be turned into a catalog row from them, which
			// the missing ISIN says on its own.
			r.log.Debug("tinvest: the broker names no ISIN for one of its own positions",
				"instrument_uid", p.InstrumentUID, "ticker", brief.Ticker)
			unpaired[p.InstrumentUID] = brief
			continue
		}
		inst, err := r.catalog.ByISIN(ctx, brief.ISIN)
		if err != nil {
			// Including "no such row": the owner does not hold this paper in
			// this program at all, which is a real difference and is reported
			// as one — with the passport on it, so that the report says what
			// the paper is and not only that it is not ours.
			r.log.Debug("tinvest: no catalog row carries the ISIN of a broker position",
				"instrument_uid", p.InstrumentUID, "isin", brief.ISIN, "err", err)
			unpaired[p.InstrumentUID] = brief
			continue
		}
		grown.ByUID[p.InstrumentUID] = inst.ID
		if _, named := grownLabels[inst.ID]; !named {
			grownLabels[inst.ID] = instrumentLabel(grownLabels, inst.ID)
		}
	}
	return grown, grownLabels, unpaired
}

func (r *Reconciler) ReconcileLink(ctx context.Context, c *Client, conn Connection, link AccountLink) (ReconcileResult, error) {
	notChecked := ReconcileResult{Status: ReconcileNotChecked}

	if link.ConnectionID != conn.ID {
		return notChecked, fmt.Errorf("%w: link %s is under connection %s, not %s",
			ErrLinkNotInConnection, link.ID, link.ConnectionID, conn.ID)
	}
	if link.SpaceID != conn.SpaceID {
		return notChecked, fmt.Errorf("%w: link %s is in space %s and connection %s in space %s",
			ErrLinkOutsideSpace, link.ID, link.SpaceID, conn.ID, conn.SpaceID)
	}

	brokerPositions, err := c.GetPortfolio(ctx, link.BrokerAccountID)
	if err != nil {
		return notChecked, err
	}
	brokerBalances, err := c.GetPositions(ctx, link.BrokerAccountID)
	if err != nil {
		return notChecked, err
	}

	journal, err := r.ops.ListForEngine(ctx, conn.SpaceID, link.AccountID)
	if err != nil {
		return notChecked, fmt.Errorf("tinvest: reconcile: read the journal of account %s: %w", link.AccountID, err)
	}
	index, labels, err := r.store.instrumentMap(ctx, conn.ID)
	if err != nil {
		return notChecked, err
	}
	index, labels, passports := r.matchByISIN(ctx, c, index, labels, brokerPositions)

	res, cmpErr := compareHoldings(brokerPositions, brokerBalances, journal, index, labels, passports)

	// The mark goes on whatever the verdict was, cmpErr included: it is the
	// broker's own statement about the account, and a journal of ours that
	// does not compute says nothing about whether that statement is true.
	//
	// BOTH REFUSALS TRAVEL WHEN BOTH HAPPENED. The engine refusing our journal
	// and the mark failing to be written are two independent accidents with
	// two different remedies, and returning only the later one would leave the
	// person who has to act on this looking at half of what went wrong.
	if err := r.markBalance(ctx, conn, link, brokerBalances); err != nil {
		return res, errors.Join(cmpErr, err)
	}

	// THE MESSAGE CLAIMS ONLY WHAT ITS OWN FIELDS CARRY. This line is written
	// for the run where cmpErr is not nil too — status is then "not checked"
	// and nothing was compared at all — so it says the attempt ended, and
	// leaves the fields to say how. Saying "reconciled" over a not-checked
	// status would be the caption that outruns its number, in a log.
	attrs := []any{
		"connection", conn.ID, "link", link.ID, "account", link.AccountID,
		"status", res.Status, "mismatches", len(res.Mismatches),
	}
	if cmpErr != nil {
		attrs = append(attrs, "not_checked_because", cmpErr)
	}
	r.log.Info("tinvest: an account's check against the broker finished", attrs...)
	return res, cmpErr
}

// markBalance files the broker's own ruble figure as the account's balance
// mark for today, after making sure the account is one rubles may be filed
// under at all (see ReconcileLink).
func (r *Reconciler) markBalance(ctx context.Context, conn Connection, link AccountLink, balances []MoneyBalance) error {
	acc, err := r.accounts.ByID(ctx, conn.SpaceID, link.AccountID)
	if err != nil {
		return fmt.Errorf("tinvest: reconcile: read account %s before marking its balance: %w", link.AccountID, err)
	}
	if acc.Currency != rubCode {
		return fmt.Errorf("%w: account %s is kept in %s and the mark would be %s",
			ErrAccountNotInRubles, link.AccountID, acc.Currency, rubCode)
	}

	var rubles decimal.Decimal
	for _, b := range balances {
		if b.Currency != rubCode {
			continue
		}
		rubles = rubles.Add(b.Value).Add(b.Blocked)
	}

	minor, refusal := minorFromDecimal(rubles)
	if refusal != nil {
		// The refusal's Detail is reused and its Error() is not: the substance
		// is right — this sum is finer than a minor unit, or larger than any
		// this program holds — but its wording names the projection ("not
		// projected"), and nothing was being projected here. What failed was
		// the writing of a balance mark, and that is what this says.
		return fmt.Errorf("%w: account %s: %s", ErrBalanceMarkRefused, link.AccountID, refusal.Detail)
	}
	if err := r.accounts.SetBalance(ctx, conn.SpaceID, link.AccountID, mskDay(r.now()), minor); err != nil {
		return fmt.Errorf("tinvest: reconcile: mark the balance of account %s: %w", link.AccountID, err)
	}
	return nil
}

// rubCode is the currency the balance mark is written in — see ReconcileLink
// on why the mark is rubles and what that requires of the account.
const rubCode = "RUB"
