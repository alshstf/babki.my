package tinvest

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

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
	// security the other does not.
	MismatchInstrument = "instrument"
	// MismatchCurrency: a cash balance in one currency differs.
	MismatchCurrency = "currency"
)

// The verdicts beyond ReconcileNotChecked, which lives in store.go because a
// run carries it from the moment it is created.
const (
	// ReconcileMatched: every position and every currency agreed.
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
// InstrumentID is nil on a currency row, and also on a security the
// connection's instrument map does not resolve — there is no instrument of
// ours to name, which is itself the news (see CompareHoldings). Label is what
// a person reads: a ticker, an instrument's name, the broker's own identifier
// for a security we could not match, or a currency code.
type ReconcileMismatch struct {
	Kind         string          `json:"kind"`
	InstrumentID *uuid.UUID      `json:"instrument_id,omitempty"`
	Label        string          `json:"label"`
	Broker       decimal.Decimal `json:"broker"`
	Journal      decimal.Decimal `json:"journal"`
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

// CompareHoldings compares what the broker says an account holds against what
// our journal says, and is a pure function of its arguments: no database, no
// clock, no network.
//
// SECURITIES ARE MATCHED THROUGH mapByUID AND NOTHING ELSE — the map the
// resolver has already built for this connection (decision 2 of the task
// brief). A broker position that is not in it is reported as a difference
// under the broker's own identifier, never passed over: nothing of ours
// corresponds to it, which usually means some of its operations did not
// project, and a silent skip would turn the most useful evidence this
// comparison can produce into nothing at all.
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
	journal []operation.Operation, mapByUID map[string]uuid.UUID,
	labels map[uuid.UUID]string,
) ReconcileResult {
	res, _ := compareHoldings(brokerPositions, brokerBalances, journal, mapByUID, labels)
	return res
}

// compareHoldings is CompareHoldings with the engine's refusal kept, for the
// caller inside this package that logs and returns it.
func compareHoldings(brokerPositions []PortfolioPosition, brokerBalances []MoneyBalance,
	journal []operation.Operation, mapByUID map[string]uuid.UUID,
	labels map[uuid.UUID]string,
) (ReconcileResult, error) {
	positions, err := portfolio.Compute(journal)
	if err != nil {
		return ReconcileResult{Status: ReconcileNotChecked}, fmt.Errorf(
			"tinvest: reconcile: the journal itself does not compute, so there was nothing to compare: %w", err)
	}

	mismatches := compareInstruments(brokerPositions, positions, mapByUID, labels)
	mismatches = append(mismatches, compareCash(brokerBalances, journal)...)
	sortMismatches(mismatches)

	status := ReconcileMatched
	if len(mismatches) > 0 {
		status = ReconcileMismatched
	}
	return ReconcileResult{Status: status, Mismatches: mismatches}, nil
}

// compareInstruments compares the number of UNITS of every security either
// side names.
func compareInstruments(brokerPositions []PortfolioPosition, positions map[uuid.UUID]*portfolio.Position,
	mapByUID map[string]uuid.UUID, labels map[uuid.UUID]string,
) []ReconcileMismatch {
	out := []ReconcileMismatch{}
	compared := make(map[uuid.UUID]bool, len(brokerPositions))

	for _, p := range brokerPositions {
		// THE BROKER'S QUANTITY IS ALREADY THE WHOLE POSITION and its Blocked
		// is a boolean — the paper is halted at the depository — so nothing is
		// added for it here. Adding one would overstate every halted holding
		// by exactly the flag. (Money is the other way round; see
		// compareCash.)
		brokerQty := p.Quantity.Decimal()

		id, ok := mapByUID[p.InstrumentUID]
		if !ok {
			out = append(out, ReconcileMismatch{
				Kind:    MismatchInstrument,
				Label:   brokerLabel(p),
				Broker:  brokerQty,
				Journal: decimal.Zero,
			})
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
// is how the broker states its own and how a person reads the row. The
// conversion is a shift of the decimal point and is exact in both directions,
// so nothing is rounded and no figure is invented; going the other way (the
// broker's number into minor units) would not be, since the gateway's amounts
// carry nine decimal places and a tenth of a kopeck cannot become an integer
// without a decision this function has no business making. (The balance mark
// does have to make that crossing, and it refuses outright rather than
// rounding — see markBalance.)
//
// A CURRENCY ONLY ONE SIDE MENTIONS IS COMPARED AGAINST ZERO rather than
// skipped: the broker's answer is a complete statement of its cash, so a
// currency absent from it is a currency it holds none of, and our journal
// claiming otherwise is exactly the kind of difference this exists to show.
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
// The running total is a decimal rather than an int64 because a sum of
// arbitrarily many entries has no bound of its own, and a wrapped int64 is a
// plausible-looking figure of the wrong sign. Nothing is rounded: every term
// is a whole number of minor units.
func journalCashMinor(journal []operation.Operation) map[string]decimal.Decimal {
	cash := make(map[string]decimal.Decimal)
	for _, o := range journal {
		if o.Type == operation.TypeTransferIn || o.Type == operation.TypeTransferOut {
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

// brokerLabel names a broker position that resolves to nothing of ours, using
// the broker's own identifiers. All three being empty would mean the broker
// returned a position it did not identify at all; the difference is still
// reported, because a badly labelled difference is news and a swallowed one is
// not.
func brokerLabel(p PortfolioPosition) string {
	switch {
	case p.InstrumentUID != "":
		return p.InstrumentUID
	case p.FIGI != "":
		return p.FIGI
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
type balanceMarker interface {
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
	log      *slog.Logger
	// now stands in for time.Now so a test can pin the day a mark is filed
	// under instead of racing the wall clock (the pattern
	// marketdata.backfillFxWorker uses).
	now func() time.Time
}

func NewReconciler(store *Store, ops engineReader, accounts balanceMarker, log *slog.Logger) *Reconciler {
	if log == nil {
		log = slog.Default()
	}
	return &Reconciler{store: store, ops: ops, accounts: accounts, log: log, now: time.Now}
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
// PRECONDITION: the babki account this link names is a RUBLE account. The mark
// is a bare int64 in the account's own currency (see account.Store.SetBalance),
// so writing rubles into an account denominated in anything else would file
// one currency's figure under another's name. Accounts for this importer are
// created by the path that makes a link, and this is the requirement on it.
// Cash in other currencies is not lost from sight: it is compared like any
// other and shows up in the differences.
//
// A broker that did not answer yields ReconcileNotChecked and the error, which
// is a different thing from "no differences" and must be shown as one.
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
	mapByUID, labels, err := r.store.instrumentMap(ctx, conn.ID)
	if err != nil {
		return notChecked, err
	}

	res, cmpErr := compareHoldings(brokerPositions, brokerBalances, journal, mapByUID, labels)

	// The mark goes on whatever the verdict was, cmpErr included: it is the
	// broker's own statement about the account, and a journal of ours that
	// does not compute says nothing about whether that statement is true.
	if err := r.markBalance(ctx, conn, link, brokerBalances); err != nil {
		return res, err
	}

	r.log.Info("tinvest: reconciled an account against the broker",
		"connection", conn.ID, "link", link.ID, "account", link.AccountID,
		"status", res.Status, "mismatches", len(res.Mismatches))
	return res, cmpErr
}

// markBalance files the broker's own ruble figure as the account's balance
// mark for today.
func (r *Reconciler) markBalance(ctx context.Context, conn Connection, link AccountLink, balances []MoneyBalance) error {
	var rubles decimal.Decimal
	for _, b := range balances {
		if b.Currency != rubCode {
			continue
		}
		rubles = rubles.Add(b.Value).Add(b.Blocked)
	}

	minor, refusal := minorFromDecimal(rubles)
	if refusal != nil {
		return fmt.Errorf("tinvest: reconcile: the broker's ruble balance of account %s cannot be a balance mark: %w",
			link.AccountID, refusal)
	}
	if err := r.accounts.SetBalance(ctx, conn.SpaceID, link.AccountID, mskDay(r.now()), minor); err != nil {
		return fmt.Errorf("tinvest: reconcile: mark the balance of account %s: %w", link.AccountID, err)
	}
	return nil
}

// rubCode is the currency the balance mark is written in — see ReconcileLink
// on why the mark is rubles and what that requires of the account.
const rubCode = "RUB"
