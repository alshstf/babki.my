package tinvest

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/money"
	"babki.my/babki/internal/portfolio"
)

// The projection turns ONE row of the mirror into the journal entries it
// means — nothing more. It is a pure function of the row: no database, no
// clock, no randomness, so the same mirror always yields the same journal and
// a rule can be changed and the whole history rebuilt without going near the
// broker again.
//
// WHAT IT DOES NOT DO, so that a reader does not go looking:
//
//   - It does not decide whether the operation is already in the journal, nor
//     write anything. That is the rebuild's (task 8), which computes the
//     difference against the journal and hands it to
//     operation.Service.ApplyImportDelta.
//   - It does not pair the two legs of a transfer. A leg is projected alone,
//     because whether the other side of it is an account this program even
//     knows about is not a property of the row (see projectSecuritiesTransfer).
//   - It does not enforce the journal's own per-type rules. Those live in
//     operation.validateImported and portfolio.Compute, and their refusal is
//     the one worth showing: it names the actual rule that was broken. This
//     function refuses only what it cannot TURN INTO a journal row at all —
//     an operation type it does not know, an amount or a quantity the journal
//     cannot express, a security that was not resolved. Everything it can
//     express, it hands over, and a refusal downstream comes back as
//     ReasonEngineRefused with the journal's own words in it.
//
// NOTHING IS EVER DROPPED IN SILENCE. Every row either becomes journal
// entries, or becomes an *UnparsedError whose Reason the owner is shown next
// to the broker's own document, or — for the two cases below, and only them —
// becomes nothing at all, on purpose and with the mirror still holding the
// row:
//
//   - an operation that did not happen: state is not "executed", or the
//     broker has stopped reporting it (DisappearedAt). Those are not failures
//     of understanding, and marking them "unparsed" would put cancelled
//     orders in the list of things this program could not read.
//   - a BROKER_FEE, while projectBrokerFee says it is the same money as the
//     commission field of the trade it belongs to.

// Source is what the journal calls operations this importer produced. It is
// one of the three values the operations.source column accepts (migration
// 0005) and is half of the journal's deduplication key — (account, source,
// external_id) — which is what keeps one broker record from becoming two
// journal rows however often the projection is rebuilt.
const Source = "tinvest"

// projectBrokerFee decides whether a BROKER_FEE operation becomes a journal
// fee of its own.
//
// It is false because a trade's commission is believed to be reported TWICE:
// once in the commission field of the trade itself (which becomes FeeMinor,
// see projectTrade) and once as a separate BROKER_FEE operation carrying the
// trade's id in parent_operation_id. Projecting both would charge the owner
// the same commission twice.
//
// THIS IS NOT VERIFIED. It is a statement about the broker's data that this
// session never checked against a live account, and it is written down as an
// assumption rather than as a fact on purpose. Task 14 settles it: on a real
// account, over a period, sum the commission fields of the trades and sum the
// BROKER_FEE operations, and see whether they are the same money. If they are
// not, this constant flips to true and the rule table already has the row for
// it.
const projectBrokerFee = false

// stateExecuted is the only operation state that describes something that
// actually happened. The mirror stores the broker's enum name verbatim (see
// MirrorRow.State), and it holds the LATEST observation, so a row that was
// once "in progress" becomes executed here of its own accord on a later sync.
const stateExecuted = "OPERATION_STATE_EXECUTED"

// brokerCurrencyInstrumentType is the broker's instrument_type for a currency.
// It is spelled here rather than added to brokerInstrumentTypes because that
// map's membership IS the resolver's supported set (see its own note), and a
// currency must stay out of it.
const brokerCurrencyInstrumentType = "currency"

// UnparsedReason is why a mirror row did not become journal entries, as a
// code rather than as prose: it is stored in the mirror
// (tinvest_operations_mirror.unparsed_reason), shown to the owner through the
// interface, and will be declared in the contract, so it has to be a value a
// client can branch on.
//
// EACH ONE NAMES A DIFFERENT FAULT and they must not be used
// interchangeably — this project has been caught four times printing a true
// number under a false reason. In particular: an amount with a fraction finer
// than a kopeck (ReasonUnrepresentableAmount) and an amount larger than the
// journal holds (ReasonAmountOutOfBounds) are different accidents with
// different fixes, and so are "this program does not account for that kind of
// asset" (ReasonUnsupportedType) and "the security was not matched to a
// catalog row" (ReasonInstrumentUnresolved).
type UnparsedReason string

const (
	// ReasonUnsupportedType: the broker's operation type is not one this
	// program records — or the operation is against a kind of asset it does
	// not account for (futures, options, currency; see brokerInstrumentTypes
	// and ErrUnsupportedInstrumentType, which says the same thing on the
	// resolver's side).
	ReasonUnsupportedType UnparsedReason = "unsupported_type"
	// ReasonUnrepresentableAmount: the sum carries a fraction finer than a
	// minor unit, and this program does not round money it was not asked to
	// round. See MinorFromDecimal.
	ReasonUnrepresentableAmount UnparsedReason = "unrepresentable_amount"
	// ReasonAmountOutOfBounds: the sum is beyond money.MaxAmountMinor, the
	// magnitude every sum in this program is bounded by.
	ReasonAmountOutOfBounds UnparsedReason = "amount_out_of_bounds"
	// ReasonUnrepresentableQty: the number of units cannot become a journal
	// quantity — it is finer than the ten decimal places the journal keeps,
	// or it is a zero where the journal needs a positive number of units.
	ReasonUnrepresentableQty UnparsedReason = "unrepresentable_quantity"
	// ReasonInstrumentUnresolved: the operation names a security and no
	// catalog row was matched to it. Distinct from ReasonUnsupportedType: the
	// asset is one this program accounts for, the matching is what failed.
	ReasonInstrumentUnresolved UnparsedReason = "instrument_unresolved"
	// ReasonEngineRefused: the journal itself would not take the operation —
	// its own per-type rules, or the portfolio engine replaying the account.
	// Produced by the rebuild (task 8) out of operation.ImportRefusal, never
	// here: this function does not second-guess the journal's rules, so that
	// the reason the owner reads is the one the journal actually gave.
	ReasonEngineRefused UnparsedReason = "engine_refused"
	// ReasonRedemptionWithoutQty: a full bond redemption that says nothing
	// about how many bonds it redeemed. See projectRedemption for why that
	// cannot be booked as anything.
	ReasonRedemptionWithoutQty UnparsedReason = "redemption_without_quantity"
)

// UnparsedError is one refusal: the code that will be stored and shown, and a
// detail for the person reading a log or a report. Detail is not stored
// anywhere — the mirror has a column for the reason only — so nothing may
// depend on its wording.
type UnparsedError struct {
	Reason UnparsedReason
	Detail string
}

func (e *UnparsedError) Error() string {
	return fmt.Sprintf("tinvest: not projected (%s): %s", e.Reason, e.Detail)
}

// minorUnitScale is how many decimal places a major currency unit is split
// into — the same 2 the operations service uses, and the same the frontend
// hardcodes. No currency in this program keeps a different number.
const minorUnitScale = 2

// maxAmountMinorDec is money.MaxAmountMinor as a decimal, for comparing
// against a shifted broker amount before it is narrowed to int64.
var maxAmountMinorDec = decimal.NewFromInt(money.MaxAmountMinor)

// MinorFromDecimal converts one of the broker's amounts into minor units
// WITHOUT ROUNDING ANYTHING.
//
// The mirror stores the broker's numbers as they arrived (units + nano, up to
// nine decimal places), and a tenth of a kopeck is not a sum this journal can
// hold. Rounding it here would be a number this program invented, in a place
// where it is not publishing a figure but recording one that everything else
// is later summed from; this project rounds once, on the figure it publishes,
// and this is not that place. So an amount finer than a minor unit
// refuses (ReasonUnrepresentableAmount) and becomes a visible unparsed row
// with the broker's own document beside it.
//
// An amount beyond money.MaxAmountMinor refuses too, with the OTHER reason
// (ReasonAmountOutOfBounds): "we cannot express this exactly" and "this is
// larger than any sum this program holds" are different accidents. The bound
// is inclusive at the edge, matching operation.validateFields, which refuses
// only what is strictly beyond it.
//
// The fraction is checked before the bound so that a huge amount which is
// ALSO finer than a kopeck is reported as the thing that is wrong with its
// shape rather than with its size; both are true of it, and neither is
// misleading.
func MinorFromDecimal(d decimal.Decimal) (int64, error) {
	v, refusal := minorFromDecimal(d)
	if refusal != nil {
		return 0, refusal
	}
	return v, nil
}

// minorFromDecimal is MinorFromDecimal for this package's own callers, which
// need the typed refusal rather than an error they would have to unwrap. The
// exported one is a wrapper so that a nil *UnparsedError never reaches a
// caller as a non-nil error.
func minorFromDecimal(d decimal.Decimal) (int64, *UnparsedError) {
	shifted := d.Shift(minorUnitScale)
	if !shifted.Equal(shifted.Truncate(0)) {
		return 0, &UnparsedError{
			Reason: ReasonUnrepresentableAmount,
			Detail: fmt.Sprintf("%s is finer than a minor unit and this program does not round money", d),
		}
	}
	if shifted.Abs().GreaterThan(maxAmountMinorDec) {
		return 0, &UnparsedError{
			Reason: ReasonAmountOutOfBounds,
			Detail: fmt.Sprintf("%s is beyond the ±%d minor units this program holds", d, money.MaxAmountMinor),
		}
	}
	return shifted.IntPart(), nil
}

// moscow is the broker's calendar, as a fixed offset rather than a lookup in
// the tz database: the image this program ships in need not carry one, and
// Moscow has been at UTC+3 with no seasonal change since October 2014, so the
// offset is the whole rule. If Russia moves its clocks again this is the line
// that has to change, and it is the only one.
var moscow = time.FixedZone("MSK", 3*60*60)

// mskDay is the calendar day a broker instant belongs to, as midnight UTC of
// that day — the shape the journal's DATE column reads back.
//
// MOSCOW, NOT UTC. The broker reports instants in UTC, and an operation at
// 23:30 Moscow time is already the next day there while UTC still calls it
// yesterday. The day matters beyond bookkeeping: the Bank of Russia publishes
// one rate per Moscow calendar day and the tax rules are written against that
// calendar, so an operation filed a day early is converted at another day's
// rate and lands in another day's tax period.
func mskDay(t time.Time) time.Time {
	m := t.In(moscow)
	return time.Date(m.Year(), m.Month(), m.Day(), 0, 0, 0, 0, time.UTC)
}

// shape is how one broker operation type becomes journal entries. It exists
// so the mapping table below stays a table — the types the broker has are
// many and the ways they project are few.
type shape uint8

const (
	asTrade shape = iota + 1
	asCash
	asAmortization
	asRedemption
	asDividendToCard
	asSecuritiesTransfer
	asBrokerFee
)

// transferKind is which side of a securities move a leg is, as far as the
// broker's operation type says. See projectSecuritiesTransfer.
type transferKind uint8

const (
	transferNone transferKind = iota
	// transferFromAnotherBroker: shares arrived from outside this program's
	// knowledge (INPUT_SECURITIES). Nothing will ever pair with such a leg.
	transferFromAnotherBroker
	// transferToAnotherBroker: shares left for outside (OUTPUT_SECURITIES).
	transferToAnotherBroker
	// transferBetweenOwnAccounts: a move between the owner's own accounts
	// (TRANS_IIS_BS, TRANS_BS_BS). The type does not say which side this row
	// is — the same type appears on both accounts — so the direction is read
	// from the sign of the quantity.
	transferBetweenOwnAccounts
)

// rule is what one broker operation type projects into.
type rule struct {
	how shape
	// journal is the journal type the row becomes. For asTrade it is buy or
	// sell; for asCash it is the cash-level type; for asRedemption it is the
	// sell a full redemption is; for asDividendToCard it is the income leg's
	// type. For asSecuritiesTransfer it is unset — the direction decides.
	journal  operation.Type
	transfer transferKind
}

// brokerOpTypes is the whole mapping from the broker's operation types to
// this journal. Its KEYS ARE THE WIRE NAMES, verbatim, because that is what
// the mirror stores (MirrorRow.OpType).
//
// ABSENCE FROM THIS MAP IS THE REFUSAL. There is deliberately no second list
// of unsupported types to keep in step with this one: futures and options
// deliveries and expirations, variation margin, the whole repo-tax family,
// OVER_PLACEMENT, DIVIDEND_TRANSFER, UNSPECIFIED and anything the broker adds
// tomorrow are all simply not here, and every one of them becomes a visible
// unparsed row with ReasonUnsupportedType. That is the owner's own decision
// 5: futures, options, margin and repo are shown as unparsed until there is
// live data to build them from.
var brokerOpTypes = map[string]rule{
	// Trades. BUY_CARD/SELL_CARD are the same trade settled against a card,
	// and BUY_MARGIN/SELL_MARGIN the same trade on margin: what the journal
	// records — units and the money they cost — is identical, and what margin
	// costs is a fee type of its own in the broker's enum (MARGIN_FEE, below).
	"OPERATION_TYPE_BUY":         {how: asTrade, journal: operation.TypeBuy},
	"OPERATION_TYPE_BUY_CARD":    {how: asTrade, journal: operation.TypeBuy},
	"OPERATION_TYPE_BUY_MARGIN":  {how: asTrade, journal: operation.TypeBuy},
	"OPERATION_TYPE_SELL":        {how: asTrade, journal: operation.TypeSell},
	"OPERATION_TYPE_SELL_CARD":   {how: asTrade, journal: operation.TypeSell},
	"OPERATION_TYPE_SELL_MARGIN": {how: asTrade, journal: operation.TypeSell},

	// Money in and out of the account.
	"OPERATION_TYPE_INPUT":            {how: asCash, journal: operation.TypeDeposit},
	"OPERATION_TYPE_INP_MULTI":        {how: asCash, journal: operation.TypeDeposit},
	"OPERATION_TYPE_INPUT_ACQUIRING":  {how: asCash, journal: operation.TypeDeposit},
	"OPERATION_TYPE_INPUT_SWIFT":      {how: asCash, journal: operation.TypeDeposit},
	"OPERATION_TYPE_OUTPUT":           {how: asCash, journal: operation.TypeWithdrawal},
	"OPERATION_TYPE_OUT_MULTI":        {how: asCash, journal: operation.TypeWithdrawal},
	"OPERATION_TYPE_OUTPUT_ACQUIRING": {how: asCash, journal: operation.TypeWithdrawal},
	"OPERATION_TYPE_OUTPUT_SWIFT":     {how: asCash, journal: operation.TypeWithdrawal},

	// Income. A Russian dividend arrives gross and its tax is a separate,
	// unlinked operation of its own — which is why the two are projected
	// independently and neither is netted against the other.
	"OPERATION_TYPE_DIVIDEND": {how: asCash, journal: operation.TypeDividend},
	"OPERATION_TYPE_COUPON":   {how: asCash, journal: operation.TypeCoupon},
	"OPERATION_TYPE_DIV_EXT":  {how: asDividendToCard, journal: operation.TypeDividend},

	// Taxes, including the corrections — which can be refunds, i.e. positive.
	// See projectCash for what a positive one becomes and why.
	//
	// The repo taxes (TAX_REPO, TAX_REPO_HOLD, TAX_REPO_REFUND and their
	// _PROGRESSIVE variants) are deliberately NOT here: repo is out of scope
	// until there is live data for it, so those rows stay visible as
	// unparsed rather than being booked as ordinary taxes.
	"OPERATION_TYPE_TAX":                        {how: asCash, journal: operation.TypeTax},
	"OPERATION_TYPE_TAX_PROGRESSIVE":            {how: asCash, journal: operation.TypeTax},
	"OPERATION_TYPE_BOND_TAX":                   {how: asCash, journal: operation.TypeTax},
	"OPERATION_TYPE_BOND_TAX_PROGRESSIVE":       {how: asCash, journal: operation.TypeTax},
	"OPERATION_TYPE_DIVIDEND_TAX":               {how: asCash, journal: operation.TypeTax},
	"OPERATION_TYPE_DIVIDEND_TAX_PROGRESSIVE":   {how: asCash, journal: operation.TypeTax},
	"OPERATION_TYPE_BENEFIT_TAX":                {how: asCash, journal: operation.TypeTax},
	"OPERATION_TYPE_BENEFIT_TAX_PROGRESSIVE":    {how: asCash, journal: operation.TypeTax},
	"OPERATION_TYPE_TAX_CORRECTION":             {how: asCash, journal: operation.TypeTax},
	"OPERATION_TYPE_TAX_CORRECTION_PROGRESSIVE": {how: asCash, journal: operation.TypeTax},
	"OPERATION_TYPE_TAX_CORRECTION_COUPON":      {how: asCash, journal: operation.TypeTax},

	// Bonds paying down.
	"OPERATION_TYPE_BOND_REPAYMENT":      {how: asAmortization, journal: operation.TypeAmortization},
	"OPERATION_TYPE_BOND_REPAYMENT_FULL": {how: asRedemption, journal: operation.TypeSell},

	// Fees the broker charges outside a trade.
	"OPERATION_TYPE_SERVICE_FEE":    {how: asCash, journal: operation.TypeFee},
	"OPERATION_TYPE_MARGIN_FEE":     {how: asCash, journal: operation.TypeFee},
	"OPERATION_TYPE_CASH_FEE":       {how: asCash, journal: operation.TypeFee},
	"OPERATION_TYPE_OUT_FEE":        {how: asCash, journal: operation.TypeFee},
	"OPERATION_TYPE_OUT_STAMP_DUTY": {how: asCash, journal: operation.TypeFee},
	"OPERATION_TYPE_OUTPUT_PENALTY": {how: asCash, journal: operation.TypeFee},
	"OPERATION_TYPE_SUCCESS_FEE":    {how: asCash, journal: operation.TypeFee},
	"OPERATION_TYPE_TRACK_MFEE":     {how: asCash, journal: operation.TypeFee},
	"OPERATION_TYPE_TRACK_PFEE":     {how: asCash, journal: operation.TypeFee},
	"OPERATION_TYPE_ADVICE_FEE":     {how: asCash, journal: operation.TypeFee},
	"OPERATION_TYPE_OVER_COM":       {how: asCash, journal: operation.TypeFee},
	"OPERATION_TYPE_BROKER_FEE":     {how: asBrokerFee, journal: operation.TypeFee},

	// Interest on the cash balance and on lent securities.
	"OPERATION_TYPE_OVERNIGHT":   {how: asCash, journal: operation.TypeInterest},
	"OPERATION_TYPE_OVER_INCOME": {how: asCash, journal: operation.TypeInterest},

	// Securities moving in and out.
	"OPERATION_TYPE_INPUT_SECURITIES":  {how: asSecuritiesTransfer, transfer: transferFromAnotherBroker},
	"OPERATION_TYPE_OUTPUT_SECURITIES": {how: asSecuritiesTransfer, transfer: transferToAnotherBroker},
	"OPERATION_TYPE_TRANS_IIS_BS":      {how: asSecuritiesTransfer, transfer: transferBetweenOwnAccounts},
	"OPERATION_TYPE_TRANS_BS_BS":       {how: asSecuritiesTransfer, transfer: transferBetweenOwnAccounts},
}

// Journal notes this projection adds. They are RUSSIAN because they are data,
// not code: the note travels into the journal and is shown to the owner
// verbatim, and there is no translation layer on this side of the program
// (the frontend's t() translates the interface, never a stored note).
const (
	// noteDividendToCard marks both legs of a dividend the broker paid
	// straight to a card — see projectDividendToCard.
	noteDividendToCard = "выплата на карту, минуя брокерский счёт"
	// noteTaxRefund marks a tax the broker gave back — see projectCash.
	noteTaxRefund = "возврат налога"
	// noteBasisUnknown marks shares that arrived from another broker with no
	// cost behind them. It is put ONLY on that case (INPUT_SECURITIES): a leg
	// of a move between the owner's own accounts may yet be paired with its
	// other half, and then the basis is known exactly — a note claiming
	// otherwise would be false on every paired transfer.
	noteBasisUnknown = "стоимость приобретения неизвестна: брокер её не передаёт"
	// noteFeeOtherCurrency marks the commission leg split off a trade whose
	// commission was charged in another currency — see tradeCommission.
	noteFeeOtherCurrency = "комиссия сделки, списанная в другой валюте"
)

// ProjectRow turns one mirror row into the journal entries it means: none,
// one, or two.
//
// resolved is the catalog instrument for the security the row names, or nil
// when the row names none — or when the caller could not resolve it, in which
// case the caller has a truer reason to record than anything here could infer
// and should not be calling this at all. Either way this function checks: a
// row that names a security which is not resolved refuses rather than
// quietly booking an operation with the attribution missing.
//
// accountID is the babki account the broker account is linked to. SpaceID is
// left unset: the write path takes it from the account itself (see
// operation.insertSQL), so stating it here would be a second copy of one fact.
func ProjectRow(row MirrorRow, accountID uuid.UUID, resolved *Resolved) ([]operation.Operation, *UnparsedError) {
	// The double door. The caller filters these out too — projecting a
	// cancelled order or one the broker has taken back would put money in the
	// journal that never moved — and the check is repeated here because it
	// costs nothing and because this function is the one that would be
	// reused, one day, by a caller that forgot.
	if row.State != stateExecuted || row.DisappearedAt != nil {
		return nil, nil
	}

	r, ok := brokerOpTypes[row.OpType]
	if !ok {
		return nil, &UnparsedError{
			Reason: ReasonUnsupportedType,
			Detail: fmt.Sprintf("broker operation type %q", row.OpType),
		}
	}

	if r.how == asBrokerFee && !projectBrokerFee {
		return nil, nil
	}

	// A currency trade is refused before anything else looks at it, and it is
	// refused even if the caller passed a resolved instrument (which it never
	// should — the resolver deliberately does not resolve currencies).
	//
	// The plan's mapping table calls for two "conversion" rows here: the
	// money paid in one currency and the money received in the other. THE
	// SECOND ROW CANNOT BE BUILT. A conversion row is a sum in a currency,
	// and nothing in the mirror names the currency that was bought: the
	// operation's own currency is the one that was PAID, the resolver is
	// deliberately never called for a currency instrument (see
	// brokerInstrumentTypes, where leaving currency out prevented a duplicate
	// catalog row per connection per currency), and the broker's identifiers
	// for the paper are opaque strings. Guessing the code from a figi table
	// would be this program inventing a currency, which is exactly the class
	// of invention the whole importer is built to avoid; projecting only the
	// paid leg would say money left the account and nothing came back.
	//
	// So a currency trade becomes a visible unparsed row with the broker's
	// own document next to it, under the same reason the resolver would have
	// given for the same instrument — this program does not account for that
	// kind of asset. Making these projectable needs the acquired currency
	// carried into the projection, which changes this function's inputs and
	// is a decision for the owner, not a detail to slip in here.
	if r.how == asTrade && row.InstrumentType == brokerCurrencyInstrumentType {
		return nil, &UnparsedError{
			Reason: ReasonUnsupportedType,
			Detail: "a currency trade: the mirror does not name the currency that was bought, so the receiving leg cannot be built",
		}
	}

	var (
		ops     []operation.Operation
		refusal *UnparsedError
	)
	switch r.how {
	case asTrade:
		ops, refusal = projectTrade(row, accountID, resolved, r.journal)
	case asCash:
		ops, refusal = projectCash(row, accountID, resolved, r.journal)
	case asAmortization:
		ops, refusal = projectAmortization(row, accountID, resolved)
	case asRedemption:
		ops, refusal = projectRedemption(row, accountID, resolved)
	case asDividendToCard:
		ops, refusal = projectDividendToCard(row, accountID, resolved)
	case asSecuritiesTransfer:
		ops, refusal = projectSecuritiesTransfer(row, accountID, resolved, r.transfer)
	case asBrokerFee:
		// Reached only with projectBrokerFee on, when the broker's separate
		// fee operation is NOT the same money as a trade's commission field.
		ops, refusal = projectCash(row, accountID, resolved, r.journal)
	}
	if refusal != nil {
		return nil, refusal
	}
	return withExternalIDs(row.ID, ops), nil
}

// base is everything a journal entry gets from the mirror row regardless of
// what kind of entry it is. Written in one place so that the day, the source
// and the note cannot come out differently on one branch than on another.
func base(row MirrorRow, accountID uuid.UUID, t operation.Type) operation.Operation {
	return operation.Operation{
		AccountID:  accountID,
		Type:       t,
		OccurredOn: mskDay(row.OccurredAt),
		Currency:   row.Currency,
		Note:       row.Description,
		Source:     Source,
	}
}

// projectTrade turns a purchase or a sale into the journal entry for it, plus
// — when the commission was charged in another currency — a fee entry of its
// own.
//
// The AMOUNT is the broker's payment and nothing else: the commission is a
// separate field and becomes FeeMinor, which is what the engine adds to the
// cost of a purchase and subtracts from the proceeds of a sale. Adding the
// commission into the amount as well would charge it twice.
//
// The QUANTITY is in units, not lots — the broker's own documentation for
// this field — so it goes into the journal as it is.
//
// Whether a bond's price arrives as money per bond or as a percent of par is
// NOT verified here or anywhere in this session; it is a question for task
// 14's live run. Nothing computed from this row depends on the answer: the
// amount is the broker's payment, and the price is an annotation the engine
// never reads.
func projectTrade(row MirrorRow, accountID uuid.UUID, resolved *Resolved, t operation.Type) ([]operation.Operation, *UnparsedError) {
	amount, refusal := minorFromDecimal(row.Payment)
	if refusal != nil {
		return nil, refusal
	}
	qty, refusal := journalQuantity(row.Quantity)
	if refusal != nil {
		return nil, refusal
	}

	op := base(row, accountID, t)
	op.AmountMinor = amount
	op.Quantity = &qty
	op.Price = tradePrice(row)
	if refusal := attachInstrument(&op, row, resolved); refusal != nil {
		return nil, refusal
	}

	feeMinor, feeLeg, refusal := tradeCommission(row, accountID)
	if refusal != nil {
		return nil, refusal
	}
	op.FeeMinor = feeMinor
	if feeLeg == nil {
		return []operation.Operation{op}, nil
	}
	return []operation.Operation{op, *feeLeg}, nil
}

// tradePrice is the per-unit price to record, or nothing.
//
// A price that is absent, zero or negative becomes no price at all rather
// than a refusal: the journal's price is an optional annotation — the engine
// never reads it, the amount does not come from it — while
// operation.validateByType refuses a non-positive one outright, so passing it
// on would cost the whole trade over a field nothing is computed from.
func tradePrice(row MirrorRow) *decimal.Decimal {
	if row.Price == nil || !row.Price.IsPositive() {
		return nil
	}
	p := *row.Price
	return &p
}

// tradeCommission decides where a trade's commission goes: into the trade's
// own FeeMinor, or into a fee entry of its own.
//
// A JOURNAL ROW HOLDS ONE CURRENCY. FeeMinor sits in the same row as the
// amount and is therefore in the same currency; a commission charged in
// another one cannot go there without being a different currency's number
// added to this row's. So it becomes a second entry, in ITS currency, and the
// trade keeps a zero fee.
//
// That second entry carries NO INSTRUMENT, deliberately: the engine holds one
// currency per position (portfolio.Compute's get), so a fee in another
// currency attached to the same instrument would make the whole account
// unreadable rather than record a fee.
//
// The commission's SIGN is not used, only its magnitude: FeeMinor is a
// magnitude by the journal's own rule (fee_minor >= 0), and a fee entry's
// amount is negative by that rule. The broker is believed to report a
// commission as money leaving, i.e. negative — not verified live, and nothing
// here depends on it being so.
//
// A commission with an amount but no currency is treated as being in the
// operation's own currency. It is malformed either way (the gateway's
// MoneyValue carries its currency — see moneyOrNothing), and the alternative
// is worse: an entry of its own would need a currency the journal would
// refuse as not ISO-4217, so the commission would be lost outright.
func tradeCommission(row MirrorRow, accountID uuid.UUID) (int64, *operation.Operation, *UnparsedError) {
	if row.Commission == nil {
		return 0, nil, nil
	}
	amount, refusal := minorFromDecimal(*row.Commission)
	if refusal != nil {
		return 0, nil, refusal
	}
	if amount == 0 {
		return 0, nil, nil
	}
	if amount < 0 {
		amount = -amount
	}
	currency := row.CommissionCurrency
	if currency == "" {
		currency = row.Currency
	}
	if currency == row.Currency {
		return amount, nil, nil
	}
	leg := base(row, accountID, operation.TypeFee)
	leg.Currency = currency
	leg.AmountMinor = -amount
	leg.Note = withNote(row.Description, noteFeeOtherCurrency)
	return 0, &leg, nil
}

// projectCash turns an operation whose whole content is money into the
// journal entry for it: a top-up, a withdrawal, income, a tax, a fee, an
// interest payment.
//
// A TAX THAT GAVE MONEY BACK IS NOT A TAX ROW. The journal's tax is money
// leaving — operation.validateByType refuses a tax whose amount is not
// negative — while the broker's corrections (TAX_CORRECTION and its family)
// are refunds and arrive positive. Rather than invent a rule the journal does
// not have, such a row is recorded as what it factually is on the account:
// money arriving, a deposit, marked in the note as a tax refund. The cost of
// that choice, stated plainly: a deposit carries no instrument (the engine
// refuses one — see acceptsInstrument), so a refund is not attributed to the
// position the tax was taken from. Whether the broker's corrections really
// arrive positive is task 14's live check; a negative one is booked as an
// ordinary tax by this same code.
//
// A commission field on a cash operation is NOT projected. The broker's own
// fee operations are types of their own (SERVICE_FEE and the rest), and a
// commission attached to a top-up or a dividend is not something this session
// has seen; task 14's reconciliation of commission fields against fee
// operations is what would show it happening.
func projectCash(row MirrorRow, accountID uuid.UUID, resolved *Resolved, t operation.Type) ([]operation.Operation, *UnparsedError) {
	amount, refusal := minorFromDecimal(row.Payment)
	if refusal != nil {
		return nil, refusal
	}

	note := row.Description
	if t == operation.TypeTax && amount > 0 {
		t = operation.TypeDeposit
		note = withNote(row.Description, noteTaxRefund)
	}

	op := base(row, accountID, t)
	op.AmountMinor = amount
	op.Note = note
	if refusal := attachInstrument(&op, row, resolved); refusal != nil {
		return nil, refusal
	}
	return []operation.Operation{op}, nil
}

// projectAmortization turns a bond's partial repayment into the journal's
// amortization: principal coming back, retiring cost basis.
//
// NO QUANTITY IS RECORDED, though the broker sends one. The engine reads a
// quantity as units that moved, and an amortization moves none — the bonds
// stay in the position and only their basis shrinks (see portfolio.Compute's
// amortization branch, whose released pieces carry cost and dates but no
// quantity). Writing the broker's count into that column would say a number
// of bonds changed hands.
func projectAmortization(row MirrorRow, accountID uuid.UUID, resolved *Resolved) ([]operation.Operation, *UnparsedError) {
	amount, refusal := minorFromDecimal(row.Payment)
	if refusal != nil {
		return nil, refusal
	}
	op := base(row, accountID, operation.TypeAmortization)
	op.AmountMinor = amount
	if refusal := attachInstrument(&op, row, resolved); refusal != nil {
		return nil, refusal
	}
	return []operation.Operation{op}, nil
}

// projectRedemption turns a bond's full repayment into a sale at par: the
// bonds leave the position and the money arrives, which is what redemption is
// and what the journal has a shape for.
//
// A REDEMPTION THAT NAMES NO QUANTITY IS REFUSED, with a reason of its own.
// What such an operation actually is, is unknown — task 14's live run is
// where that gets settled — and neither guess is safe: booking it as an
// amortization would leave the bonds in the position for good, since nothing
// would ever remove them, and inventing "all of what is held" would be this
// program deciding how many bonds the broker redeemed.
func projectRedemption(row MirrorRow, accountID uuid.UUID, resolved *Resolved) ([]operation.Operation, *UnparsedError) {
	if row.Quantity <= 0 {
		return nil, &UnparsedError{
			Reason: ReasonRedemptionWithoutQty,
			Detail: fmt.Sprintf("a full bond redemption of %d units says nothing about how many bonds were redeemed", row.Quantity),
		}
	}
	amount, refusal := minorFromDecimal(row.Payment)
	if refusal != nil {
		return nil, refusal
	}
	qty, refusal := journalQuantity(row.Quantity)
	if refusal != nil {
		return nil, refusal
	}
	op := base(row, accountID, operation.TypeSell)
	op.AmountMinor = amount
	op.Quantity = &qty
	op.Price = tradePrice(row)
	if refusal := attachInstrument(&op, row, resolved); refusal != nil {
		return nil, refusal
	}
	return []operation.Operation{op}, nil
}

// projectDividendToCard turns a dividend the broker paid straight to a card
// into the two entries it really is: the income, and the same money leaving
// the brokerage account the same day.
//
// ONE ENTRY WOULD BE WRONG EITHER WAY. Recorded as a dividend alone, the
// account's cash balance grows by money that never reached it; dropped, the
// income disappears from the year's dividends. Both legs carry the note
// saying where the money went, because on their own each looks like something
// it is not.
func projectDividendToCard(row MirrorRow, accountID uuid.UUID, resolved *Resolved) ([]operation.Operation, *UnparsedError) {
	amount, refusal := minorFromDecimal(row.Payment)
	if refusal != nil {
		return nil, refusal
	}
	note := withNote(row.Description, noteDividendToCard)

	income := base(row, accountID, operation.TypeDividend)
	income.AmountMinor = amount
	income.Note = note
	if refusal := attachInstrument(&income, row, resolved); refusal != nil {
		return nil, refusal
	}

	out := base(row, accountID, operation.TypeWithdrawal)
	out.AmountMinor = -amount
	out.Note = note
	return []operation.Operation{income, out}, nil
}

// projectSecuritiesTransfer turns one side of a securities move into one
// journal leg. It is ALWAYS a single leg: whether the other side of the move
// is an account this program also mirrors is not something the row says, and
// pairing two legs into one transfer group is the rebuild's job (task 8).
//
// THE COST BASIS IS NOT SET HERE, and it is zero for a reason on each side.
// A departing leg's basis is released from the source account's own journal
// by the write path itself — operation.checkImportContract refuses a supplied
// one outright, since a FIFO basis is a property of the history and not of
// the broker's message. An arriving leg from ANOTHER broker has no basis
// anywhere: this broker does not report what the shares cost at the last one.
// That leg alone carries the note saying so, because a leg of a move between
// the owner's own accounts may still be paired, and then the basis is known
// exactly and any note claiming otherwise would be false.
//
// THE DIRECTION of a move between the owner's own accounts is read from the
// SIGN OF THE QUANTITY, and that is the one assumption here that has not been
// checked against live data (task 14). The type cannot say it: TRANS_IIS_BS
// and TRANS_BS_BS describe a move between two accounts and appear on both
// sides of it, so the same type is an arrival on one account and a departure
// on the other. A zero leaves the direction unknowable and is refused rather
// than guessed.
func projectSecuritiesTransfer(row MirrorRow, accountID uuid.UUID, resolved *Resolved, kind transferKind) ([]operation.Operation, *UnparsedError) {
	t := operation.TypeTransferIn
	switch kind {
	case transferFromAnotherBroker:
		t = operation.TypeTransferIn
	case transferToAnotherBroker:
		t = operation.TypeTransferOut
	case transferBetweenOwnAccounts:
		switch {
		case row.Quantity > 0:
			t = operation.TypeTransferIn
		case row.Quantity < 0:
			t = operation.TypeTransferOut
		default:
			return nil, &UnparsedError{
				Reason: ReasonUnrepresentableQty,
				Detail: "a transfer between accounts of zero units: the journal records no transfer of nothing, and nothing else in the row says which way it went",
			}
		}
	}

	units := row.Quantity
	if units < 0 {
		units = -units
	}
	qty, refusal := journalQuantity(units)
	if refusal != nil {
		return nil, refusal
	}

	op := base(row, accountID, t)
	// Zero, always: see the note above. The journal treats a transfer's
	// amount as a cost basis, not as cash, and refuses a negative one.
	op.AmountMinor = 0
	op.Quantity = &qty
	if kind == transferFromAnotherBroker {
		op.Note = withNote(row.Description, noteBasisUnknown)
	}
	if refusal := attachInstrument(&op, row, resolved); refusal != nil {
		return nil, refusal
	}
	return []operation.Operation{op}, nil
}

// attachInstrument puts the resolved catalog instrument on the entry, or
// refuses, or leaves it alone — by what the ENGINE does with a row of that
// type that names one.
//
// A type the engine will not fold with an instrument (a deposit, a
// withdrawal, an interest payment) gets none, and the security the row may
// have named is ignored rather than refused: attaching it would make the
// whole account unreadable (portfolio.Compute's default branch), and refusing
// would lose an operation the journal records perfectly well without it.
//
// A type that CAN carry one and has no resolution refuses — always when the
// type requires an instrument, and also when the row names a security this
// program failed to match. The second half is what keeps a dividend on an
// unmatched share from being booked as unattributed income: the amount would
// be right and the row would quietly stop being about that share.
func attachInstrument(op *operation.Operation, row MirrorRow, resolved *Resolved) *UnparsedError {
	if !acceptsInstrument(op.Type) {
		return nil
	}
	if resolved != nil {
		// A local copy: the entry must not point into the caller's struct.
		id := resolved.InstrumentID
		op.InstrumentID = &id
		return nil
	}
	if op.Type.RequiresInstrument() || namesSecurity(row) {
		return instrumentRefusal(row)
	}
	return nil
}

// acceptsInstrument reports whether the portfolio engine will fold an
// operation of this type that names an instrument. It mirrors
// portfolio.Compute's own switch — the types with a branch there, plus
// conversion, which Compute skips before it looks at the instrument at all —
// and the types without one, which Compute refuses through its default arm.
//
// It is a second statement of the engine's behaviour, which this codebase
// otherwise avoids; there is no exported predicate to derive it from. What
// keeps the two from drifting is a test that asks the ENGINE, type by type,
// and compares (see TestAcceptsInstrumentAgreesWithTheEngine).
func acceptsInstrument(t operation.Type) bool {
	switch t {
	case operation.TypeBuy, operation.TypeSell,
		operation.TypeDividend, operation.TypeCoupon,
		operation.TypeTax, operation.TypeFee,
		operation.TypeAmortization,
		operation.TypeTransferIn, operation.TypeTransferOut,
		operation.TypeSplit, operation.TypeConversion:
		return true
	}
	return false
}

// namesSecurity reports whether the broker said this operation was about a
// paper at all. Both identifiers are checked because either may be the one an
// old operation carries — the broker's own documentation says figi and
// instrument_uid have been rewritten on historical operations — and the
// resolver looks the instrument up by both for that reason.
func namesSecurity(row MirrorRow) bool {
	return row.InstrumentUID != "" || row.FIGI != ""
}

// instrumentRefusal names WHY there is no instrument, in the same two words
// the resolver uses for the same two situations: a kind of asset this program
// does not account for at all, and a security it does account for but did not
// match. The set of kinds it accounts for is read from brokerInstrumentTypes
// — the resolver's own list — so the two answers cannot come apart.
func instrumentRefusal(row MirrorRow) *UnparsedError {
	if _, ok := brokerInstrumentTypes[row.InstrumentType]; !ok {
		return &UnparsedError{
			Reason: ReasonUnsupportedType,
			Detail: fmt.Sprintf("the broker calls this instrument type %q, which this program does not account for", row.InstrumentType),
		}
	}
	return &UnparsedError{
		Reason: ReasonInstrumentUnresolved,
		Detail: fmt.Sprintf("no catalog instrument for instrument_uid %q / figi %q", row.InstrumentUID, row.FIGI),
	}
}

// journalQuantity brings the broker's count of units onto the scale the
// journal stores a quantity at, BEFORE the entry leaves this function — the
// same discipline operation.normalizeForStorage applies to a hand-entered
// one, and for the same reason: a quantity accepted at one value and stored
// at another once broke an account's positions screen for good.
//
// Truncation cannot change anything while the mirror's quantity is a whole
// number of units, which it is (MirrorRow.Quantity is an int64 and the broker
// reports units, not lots), so the refusal below is unreachable through
// ProjectRow today. That is not a reason to leave it out: it is the guard
// that keeps a mirror whose quantity became a decimal from slipping past
// silently, and it refuses on the same condition the write path does — a
// positive quantity that the scale leaves at nothing (see
// operation.normalizeForStorage).
func journalQuantity(units int64) (decimal.Decimal, *UnparsedError) {
	q := decimal.NewFromInt(units).Truncate(portfolio.QuantityScale)
	if units > 0 && !q.IsPositive() {
		return decimal.Zero, &UnparsedError{
			Reason: ReasonUnrepresentableQty,
			Detail: fmt.Sprintf("%d units is finer than the %d decimal places the journal records", units, portfolio.QuantityScale),
		}
	}
	return q, nil
}

// withExternalIDs names every entry one mirror row produced, so the journal's
// deduplication index over (account, source, external_id) can tell them
// apart.
//
// ONE ENTRY TAKES THE ROW'S OWN ID; two take it with "/1" and "/2". The
// suffixes are assigned here and nowhere else, from the order the entries
// were built in, which is fixed for each kind of row (income then withdrawal;
// trade then its commission) — so a rebuild of an unchanged mirror produces
// the same names and updates rather than duplicates.
func withExternalIDs(rowID uuid.UUID, ops []operation.Operation) []operation.Operation {
	if len(ops) == 1 {
		id := rowID.String()
		ops[0].ExternalID = &id
		return ops
	}
	for i := range ops {
		id := fmt.Sprintf("%s/%d", rowID, i+1)
		ops[i].ExternalID = &id
	}
	return ops
}

// withNote adds this program's own mark to whatever the broker called the
// operation, keeping both: the broker's wording is what the owner recognizes,
// and the mark is what this program is claiming about it.
func withNote(description, mark string) string {
	if description == "" {
		return mark
	}
	return description + " — " + mark
}
