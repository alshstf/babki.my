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

// ErrAccountNotInRubles means the linked account is not kept in roubles, so the
// broker's rouble figure cannot be its balance mark: a mark has no currency of its
// own (account.Store.SetBalance). Checked at the write (see ReconcileLink).
var ErrAccountNotInRubles = errors.New("tinvest: the linked account is not kept in rubles")

// ErrBalanceMarkRefused means the broker's figure could not become a balance
// mark: finer than a minor unit (never rounded) or beyond any sum this program
// holds.
var ErrBalanceMarkRefused = errors.New("tinvest: the broker's ruble balance cannot be a balance mark")

// Reconciliation is the point of the import: after every sync this program
// computes the account's positions itself from its journal and compares them
// with what the broker holds. Agreement is stated with its moment, disagreement
// line by line, and a check that could not be made says so.
//
// Quantities and cash are compared, never valuations, which honest programs
// differ on constantly.
//
// The broker's two "blocked" fields differ in kind:
//
//   - a security's Quantity is the whole position and its Blocked is a bool (the
//     depository halted it); adding anything overstates halted holdings, such as
//     the owner's frozen FinEx and SPB paper;
//   - money's free and blocked figures are two addends of one balance; reading
//     only the free one misreports every account with an open order.
//
// The wire evidence is on PortfolioPosition and MoneyBalance in client.go.

// Kinds of difference; a plain string because it travels through jsonb and the
// API, where a security's row and a currency's are read differently.
const (
	// MismatchInstrument: units differ, or one side names a security the other
	// does not. A broker security we could not match points at operations that
	// did not project.
	MismatchInstrument = "instrument"
	// MismatchCurrency: a cash balance in one currency differs.
	MismatchCurrency = "currency"
	// MismatchUnsupported: the broker holds an asset kind this program does
	// not account for (outside brokerInstrumentTypes; cash is compared as
	// money instead). Reported, but distinct from MismatchInstrument: no
	// re-import will change it, so the owner should not hunt for missing
	// operations.
	MismatchUnsupported = "unsupported"
	// MismatchUnknownSecurity: the broker holds a supported kind of security
	// and nothing of ours corresponds to it, since no operation on it ever
	// reached the journal. On the owner's account: TECH2, TSPX2, TUSD2, funds
	// his TECH and TSPX were converted into. It asks "what happened to this
	// paper", an unrecorded corporate action, rather than "which operations
	// are missing".
	MismatchUnknownSecurity = "unknown_security"
)

// Verdicts beyond ReconcileNotChecked (store.go).
const (
	// ReconcileMatched: every security's quantity and currency's balance
	// agreed, and nothing unsupported was named.
	ReconcileMatched ReconcileStatus = "matched"
	// ReconcileMismatched: something differed; ReconcileResult says what.
	ReconcileMismatched ReconcileStatus = "mismatched"
)

// ReconcileMismatch is one disagreement, with both figures so a person knows
// which side to check.
//
// InstrumentID is nil for a currency row, an unsupported one, and a security the
// index does not resolve. Label is our ticker or name (or id), the broker's
// naming of a position not ours (brokerLabel), or a currency code.
//
// The Broker* fields are the broker's passport of an unknown-security row only
// (see matchByISIN): pointers, so "no passport" (404 or past the cap, and runs
// from before these fields) differs from an empty field. BrokerType is the
// catalog's InstrumentType via brokerInstrumentTypes, from the position's type,
// so a client has everything CreateInstrumentRequest needs.
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

	// SplitHintFactor is set when the quantities differ by a whole factor of
	// two or more and the registry has no split that could explain it. It is
	// a question, not a finding: the owner's AMZN (1 vs 20) and NVDA (3 vs
	// 30) are unrecorded splits, but a missed purchase leaves the same shape.
	// Nothing acts on it. Its presence is the statement; no separate flag.
	SplitHintFactor *int64 `json:"split_hint_factor,omitempty"`
}

// ReconcileResult is one reconciliation's verdict. Status is derived from
// Mismatches: matched means the list is empty. ReconcileNotChecked means no
// comparison was made. A hand-built contradictory pair is refused on write
// (ErrReconcileVerdictContradictsItself).
type ReconcileResult struct {
	Status     ReconcileStatus     `json:"status"`
	Mismatches []ReconcileMismatch `json:"mismatches"`
}

// InstrumentIndex is which catalog instrument each broker identifier stands
// for. Two maps because identifiers drift: knowing only instrument_uid would turn
// one drifted position into two false lines while the journal was fine, since the
// resolver had matched those operations by figi.
type InstrumentIndex struct {
	ByUID  map[string]uuid.UUID
	ByFIGI map[string]uuid.UUID
}

// lookup finds our instrument for a broker position by instrument_uid, then
// figi, the resolver's order. Where map rows give one figi to different
// instruments, the resolver picks one and this answers nothing (see
// instrumentMap): unmatched is better than a confident wrong match. An empty
// identifier matches nothing.
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

// CompareHoldings compares the broker's holdings with our journal's, as a pure
// function. Securities are matched through index only; a broker position in
// neither map is reported under the broker's naming, never skipped. Cash
// positions and unsupported kinds are handled in compareInstruments. labels name
// our instruments, falling back to the id. A journal the engine refuses yields
// ReconcileNotChecked, since no comparison happened; compareHoldings returns the
// reason.
func CompareHoldings(brokerPositions []PortfolioPosition, brokerBalances []MoneyBalance,
	journal []operation.Operation, index InstrumentIndex,
	labels map[uuid.UUID]string,
) ReconcileResult {
	res, _ := compareHoldings(brokerPositions, brokerBalances, journal, index, labels, nil)
	return res
}

// compareHoldings is CompareHoldings keeping the engine's refusal, and with
// the passports obtained for unmatched positions (see matchByISIN).
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

// brokerTypeCurrency is the broker's instrument_type for its cash positions,
// recognized here to leave them to the cash comparison.
const brokerTypeCurrency = "currency"

// compareInstruments compares units of every security either side names.
// passports (by instrument_uid; nil from CompareHoldings) fill an unknown row's
// Broker* fields and decide nothing.
func compareInstruments(brokerPositions []PortfolioPosition, positions map[uuid.UUID]*portfolio.Position,
	index InstrumentIndex, labels map[uuid.UUID]string, passports map[string]InstrumentBrief,
) []ReconcileMismatch {
	out := []ReconcileMismatch{}
	// Summed per catalog row: one paper on two listings is two broker
	// positions and one holding here (#135). order keeps the broker's sequence.
	held := make(map[uuid.UUID]decimal.Decimal, len(brokerPositions))
	var order []uuid.UUID

	for _, p := range brokerPositions {
		// Cash stands in the position list as type "currency" (live sandbox,
		// 2026-08-05, testdata/portfolio_cash_only.json). compareCash compares it
		// from GetPositions; comparing it here too would leave a phantom position
		// on every account with cash.
		if p.InstrumentType == brokerTypeCurrency {
			continue
		}

		// Quantity is the whole position; Blocked is a flag, so nothing is added
		// (money is the opposite; see compareCash).
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
		if _, seen := held[id]; !seen {
			order = append(order, id)
		}
		held[id] = held[id].Add(brokerQty)
	}

	for _, id := range order {
		ours := decimal.Zero
		if pos, found := positions[id]; found {
			ours = pos.Quantity
		}
		if held[id].Equal(ours) {
			continue
		}
		instrumentID := id
		out = append(out, ReconcileMismatch{
			Kind:         MismatchInstrument,
			InstrumentID: &instrumentID,
			Label:        instrumentLabel(labels, id),
			Broker:       held[id],
			Journal:      ours,
		})
	}

	for id, pos := range positions {
		// A sold-out position stays in the engine's answer at zero; the broker
		// does not report it, and that is no difference.
		if _, compared := held[id]; compared || pos.Quantity.IsZero() {
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

// compareCash compares cash per currency, in whole currency units: our minor
// units shift exactly, while the broker's nine-decimal amounts cannot become minor
// units without a rounding decision (markBalance refuses instead). A currency
// only one side names is compared against zero. Our side is the whole account's
// journal, hand entries included, under the precondition that an import feeds
// accounts of its own (see rebuild.go).
func compareCash(brokerBalances []MoneyBalance, journal []operation.Operation) []ReconcileMismatch {
	broker := make(map[string]decimal.Decimal, len(brokerBalances))
	for _, b := range brokerBalances {
		// Free plus blocked: two addends of one balance. Summed, since a currency
		// may appear in either list.
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

// journalCashMinor is the cash the journal accounts for per currency, in minor
// units: amounts less fees. A buy carries its commission beside the amount, a
// standalone charge is a negative amount without a fee. Rows that are not cash
// (a transfer's or conversion's basis) are excluded by asking portfolio.MovesCash,
// the engine's own rule, rather than a local list. Decimal, not int64, so a long
// sum cannot wrap. Pinned by a test with figures written out, since nothing else
// computes it.
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

// unmatchedKind classifies an unmatched broker position as an unknown security
// or an unsupported kind, by brokerInstrumentTypes alone, the resolver's own
// table, so the two cannot disagree.
func unmatchedKind(p PortfolioPosition) string {
	if _, supported := brokerInstrumentTypes[p.InstrumentType]; supported {
		return MismatchUnknownSecurity
	}
	return MismatchUnsupported
}

// attachPassport fills an unknown-security row's Broker* fields: the type from
// the position via brokerInstrumentTypes (it cannot miss; the guard publishes no
// type if it ever does), ISIN, name and currency from the passport if obtained.
// Empty fields stay nil so no client builds a row from "".
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

// brokerLabel names an unmatched broker position by ticker, then figi, then
// the machine identifiers. Reported even with no identifier at all.
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

// instrumentLabel names one of our instruments, falling back to its id.
func instrumentLabel(labels map[uuid.UUID]string, id uuid.UUID) string {
	if l := labels[id]; l != "" {
		return l
	}
	return id.String()
}

// sortMismatches gives a stable order: half the rows come from map
// iteration, which Go randomizes, and a reshuffle would look like news.
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

// balanceMarker reads the account as well as marking it: a mark is in the
// account's currency, so the account must be checked to be in roubles first.
type balanceMarker interface {
	ByID(ctx context.Context, spaceID, id uuid.UUID) (account.WithBalance, error)
	SetBalance(ctx context.Context, spaceID, accountID uuid.UUID, asOf time.Time, amountMinor int64) error
}

// engineReader is one account's journal in engine order.
type engineReader interface {
	ListForEngine(ctx context.Context, spaceID, accountID uuid.UUID) ([]operation.Operation, error)
}

// Reconciler compares one linked account against the broker and records what
// the broker says the account is worth.
type Reconciler struct {
	store    *Store
	ops      engineReader
	accounts balanceMarker
	// catalog finds our row for an ISIN, recognizing a position under a
	// listing this connection never imported (see matchByISIN).
	catalog isinCatalog
	// registry answers whether a split-shaped difference is unrecorded; nil
	// offers no hint (see attachSplitHints).
	registry splitRegistry
	log      *slog.Logger
	// now pins the day a mark is filed under in tests.
	now func() time.Time
}

// NewReconciler builds the check. registry may be nil.
func NewReconciler(store *Store, ops engineReader, accounts balanceMarker, catalog isinCatalog,
	registry splitRegistry, log *slog.Logger,
) *Reconciler {
	if log == nil {
		log = slog.Default()
	}
	return &Reconciler{
		store: store, ops: ops, accounts: accounts, catalog: catalog,
		registry: registry, log: log, now: time.Now,
	}
}

// isinCatalog is the part of instrument.Store the reconciliation needs.
type isinCatalog interface {
	ByISIN(ctx context.Context, isin string) (instrument.Instrument, error)
	// ByIDs maps differences to ISINs for the split hint, in one query.
	ByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]instrument.Instrument, error)
}

// splitRegistry answers whether the registry knows a split of this paper on
// or before a day. Declared here: the registry knows nothing about brokers.
type splitRegistry interface {
	HasSplitOnOrBefore(ctx context.Context, isin string, day time.Time) (bool, error)
}

// passportLookupsPerReconcile bounds the passports one check asks for: the
// positions this connection has no mapping for (none on a healthy account,
// fourteen on the owner's). Positions past it stay unmatched and the skipped
// count is logged.
const passportLookupsPerReconcile = 40

// matchByISIN teaches the index broker positions it does not know, by asking
// the broker which paper each is and finding that ISIN in our catalog. A share
// moved to another venue appears under a new listing while the journal names the
// old one; without this, seven of the owner's papers showed twice ("broker 20,
// ours 0" and "ours 20, broker 0"). Only the ISIN identifies a security. Nothing
// is written: the instrument map decides where future trades are booked, and a
// check only looks. The third result is the passports that matched nothing, by
// instrument_uid, so the row can say what the paper is; a 404 or a capped
// position is absent.
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

	// Copies: nothing learned here outlives this comparison.
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
			// Not a match guard (ByISIN refuses ""), but the log distinguishes "the
			// broker would not say" from "nothing of ours has it". The passport is
			// kept for its name and currency.
			r.log.Debug("tinvest: the broker names no ISIN for one of its own positions",
				"instrument_uid", p.InstrumentUID, "ticker", brief.Ticker)
			unpaired[p.InstrumentUID] = brief
			continue
		}
		inst, err := r.catalog.ByISIN(ctx, brief.ISIN)
		if err != nil {
			// Including no row: a real difference, reported with its passport.
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

// ReconcileLink checks one linked account against the broker and, when the
// broker answered, marks the account's balance with the broker's own figure for
// it (see markBalance), filed under the Moscow day.
//
// The mark is written whenever the broker answered, even if differences were
// found or our journal did not compute: the broker's statement stands either
// way. It is not written when the broker did not answer (yesterday's mark is
// better than a guess) or our database failed a read on the way.
//
// The account must be a rouble account, checked here (ErrAccountNotInRubles)
// rather than trusted to the path that creates links: the mark has no currency
// of its own, and a link can point at an existing account.
//
// A broker that did not answer yields ReconcileNotChecked and the error.
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

	brokerPortfolio, err := c.GetPortfolio(ctx, link.BrokerAccountID)
	if err != nil {
		return notChecked, err
	}
	brokerPositions := brokerPortfolio.Positions
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
	r.attachSplitHints(ctx, res.Mismatches)

	// The mark is written whatever the verdict: it is the broker's own
	// statement. Both errors are returned when both happened.
	if err := r.markBalance(ctx, conn, link, brokerPortfolio.Total); err != nil {
		return res, errors.Join(cmpErr, err)
	}

	// The message claims only what its fields carry; the status may be
	// "not checked".
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

// attachSplitHints marks differences by a whole factor of two or more, either
// way, on papers the registry has no split for (AMZN 1 vs 20 after Amazon's 20:1
// of June 2022; NVDA 3 vs 30 after NVIDIA's 10:1 of June 2024; no broker reports
// splits as operations). A recorded split would already be in the journal, so it
// is not hinted. It changes nothing, and a failed lookup just shows no hint.
func (r *Reconciler) attachSplitHints(ctx context.Context, mismatches []ReconcileMismatch) {
	if r.registry == nil || r.catalog == nil {
		return
	}
	ids := make([]uuid.UUID, 0, len(mismatches))
	for i := range mismatches {
		if mismatches[i].Kind == MismatchInstrument && mismatches[i].InstrumentID != nil {
			ids = append(ids, *mismatches[i].InstrumentID)
		}
	}
	if len(ids) == 0 {
		return
	}
	rows, err := r.catalog.ByIDs(ctx, ids)
	if err != nil {
		r.log.Debug("tinvest: could not read the papers of the differences, so no split hint is offered", "err", err)
		return
	}
	// The hint's day: a later event could not have moved today's holding.
	today := r.now()
	for i := range mismatches {
		m := &mismatches[i]
		if m.Kind != MismatchInstrument || m.InstrumentID == nil {
			continue
		}
		factor, ok := wholeFactor(m.Broker, m.Journal)
		if !ok {
			continue
		}
		inst, found := rows[*m.InstrumentID]
		if !found || inst.ISIN == "" {
			// No ISIN: the registry is keyed by it, so nothing could be recorded.
			continue
		}
		known, err := r.registry.HasSplitOnOrBefore(ctx, inst.ISIN, today)
		if err != nil {
			r.log.Debug("tinvest: could not ask the registry about a paper, so no split hint is offered",
				"isin", inst.ISIN, "err", err)
			continue
		}
		if known {
			continue
		}
		f := factor
		m.SplitHintFactor = &f
	}
}

// wholeFactor is the whole multiple, at least two, of the larger quantity over
// the smaller, in either direction; a zero on either side is no factor.
func wholeFactor(a, b decimal.Decimal) (int64, bool) {
	if !a.IsPositive() || !b.IsPositive() {
		return 0, false
	}
	hi, lo := a, b
	if hi.LessThan(lo) {
		hi, lo = lo, hi
	}
	q := hi.Div(lo)
	if !q.Equal(q.Truncate(0)) {
		return 0, false
	}
	f := q.IntPart()
	if f < 2 {
		return 0, false
	}
	return f, true
}

// markBalance files the broker's whole-account worth (securities at its prices
// plus cash, in roubles) as today's balance mark, after checking the account is a
// rouble account. Since Р-2 (2026-10-02) the account is valued from its journal
// and checked against this figure; before that the mark was the broker's roubles
// alone. No total leaves the previous mark standing.
func (r *Reconciler) markBalance(ctx context.Context, conn Connection, link AccountLink, total *MoneyValue) error {
	acc, err := r.accounts.ByID(ctx, conn.SpaceID, link.AccountID)
	if err != nil {
		return fmt.Errorf("tinvest: reconcile: read account %s before marking its balance: %w", link.AccountID, err)
	}
	if acc.Currency != rubCode {
		return fmt.Errorf("%w: account %s is kept in %s and the mark would be %s",
			ErrAccountNotInRubles, link.AccountID, acc.Currency, rubCode)
	}

	if total == nil {
		r.log.Warn("tinvest: the broker named no total for the account, its previous balance mark stands",
			"account", link.AccountID)
		return nil
	}
	if total.Currency != rubCode {
		return fmt.Errorf("%w: account %s: the broker stated its total in %s, and rubles were asked for",
			ErrBalanceMarkRefused, link.AccountID, total.Currency)
	}

	minor, refusal := minorFromDecimal(total.Decimal())
	if refusal != nil {
		// The refusal's Detail, not its Error(): nothing was being projected.
		return fmt.Errorf("%w: account %s: %s", ErrBalanceMarkRefused, link.AccountID, refusal.Detail)
	}
	if err := r.accounts.SetBalance(ctx, conn.SpaceID, link.AccountID, mskDay(r.now()), minor); err != nil {
		return fmt.Errorf("tinvest: reconcile: mark the balance of account %s: %w", link.AccountID, err)
	}
	return nil
}

// rubCode is the balance mark's currency.
const rubCode = "RUB"
