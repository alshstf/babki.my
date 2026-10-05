package tinvest

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/money"
	"babki.my/babki/internal/portfolio"
)

// The projection turns one mirror row into the journal entries it means. It is
// a pure function of the row (no database, clock or randomness), so a rule can
// change and the whole history be rebuilt without the broker.
//
// It does not:
//
//   - decide what is already in the journal or write anything: the rebuild
//     diffs and calls operation.Service.ApplyImportDelta;
//   - pair transfer legs: whether the other side is an account this program
//     knows is not in the row (see projectSecuritiesTransfer);
//   - enforce the journal's per-type rules: those refuse downstream in their
//     own words (ReasonEngineRefused). It refuses only what it cannot turn into
//     a journal row at all;
//   - look up holdings: a bond's full redemption, which carries only a payment,
//     is returned with a Deferred (see projectRedemption).
//
// Nothing is dropped silently. A row becomes entries, an *UnparsedError shown
// beside the broker's document, or nothing only when the operation did not
// happen (not executed, or disappeared). A broker fee charged as its own
// operation is built and deferred (DeferredBrokerFeeVerdict), since one of the
// owner's 311 was not a duplicate.

// Source is the journal source of this importer's rows, half of the
// (account, source, external_id) key that keeps one broker record one journal
// row across rebuilds.
const Source = "tinvest"

// stateExecuted is the only state describing something that happened. The
// mirror holds the latest state, so an in-progress row becomes executed on a
// later sync.
const stateExecuted = "OPERATION_STATE_EXECUTED"

// brokerCurrencyInstrumentType is the broker's instrument_type for a
// currency, kept out of brokerInstrumentTypes, which is the resolver's
// supported set.
const brokerCurrencyInstrumentType = "currency"

// UnparsedReason is why a row did not become entries, as a code: stored in the
// mirror, declared in the contract and branched on by the interface. The prose is
// UnparsedError.Detail, never branched on. Each names a different fault, with a
// different fix, and they are not interchangeable.
type UnparsedReason string

const (
	// ReasonUnsupportedType: a broker operation type this program does not
	// record, or an asset kind it does not account for (futures, options,
	// currency; see brokerInstrumentTypes).
	ReasonUnsupportedType UnparsedReason = "unsupported_type"
	// ReasonUnrepresentableAmount: a fraction finer than a minor unit, which
	// this program does not round (see MinorFromDecimal).
	ReasonUnrepresentableAmount UnparsedReason = "unrepresentable_amount"
	// ReasonAmountOutOfBounds: the sum is beyond money.MaxAmountMinor, the
	// magnitude every sum in this program is bounded by.
	ReasonAmountOutOfBounds UnparsedReason = "amount_out_of_bounds"
	// ReasonUnrepresentableQty: units finer than the journal's ten places (see
	// journalQuantity; unreachable today).
	ReasonUnrepresentableQty UnparsedReason = "unrepresentable_quantity"
	// ReasonTradeWithoutFill: a purchase or sale with no executed quantity.
	// Falling back to the order's size wrote fifteen of the owner's trades at
	// up to 2.5 times their size: 6644 units recorded as 11100 for the same
	// money (#131).
	ReasonTradeWithoutFill UnparsedReason = "trade_without_filled_quantity"
	// ReasonTransferDirectionUnknown: a move between the owner's accounts
	// with quantity zero; its sign is the only carrier of direction.
	ReasonTransferDirectionUnknown UnparsedReason = "transfer_direction_unknown"
	// ReasonInstrumentUnresolved: the asset kind is supported but no catalog
	// row was matched.
	ReasonInstrumentUnresolved UnparsedReason = "instrument_unresolved"
	// ReasonEngineRefused: the journal's own rules or the engine's replay
	// refused the operation. Produced by the rebuild from
	// operation.ImportRefusal, never here.
	ReasonEngineRefused UnparsedReason = "engine_refused"
	// ReasonRedemptionWithoutQty: a full bond redemption with no count.
	// Nothing produces it any more (the count now comes from the position, see
	// Rebuilder.closeRedemptions); it is declared because older mirror rows
	// still carry it until rebuilt.
	ReasonRedemptionWithoutQty UnparsedReason = "redemption_without_quantity"
	// ReasonRedemptionNothingHeld: a full bond redemption on an account whose
	// journal holds none of the bond by then. Produced by the rebuild
	// (Rebuilder.closeRedemptions), never here.
	ReasonRedemptionNothingHeld UnparsedReason = "redemption_nothing_held"
	// ReasonCurrencyTrade: a purchase or sale of currency. The journal has a
	// type for it (conversion); what is missing is the data for the second
	// leg: the traded currency (the row's currency is the payment side) and
	// the nominal per unit, which is not always one (Kyrgyz som: 100, Uzbek
	// sum: 10 000; live, 2026-08-05). A wrong nominal is wrong a hundredfold
	// (the shape of #87), so the row stays visible until both are supplied.
	ReasonCurrencyTrade UnparsedReason = "currency_trade"
	// ReasonTransferWithoutQuantity: a transfer with zero in every quantity
	// field and no fraction in its description to read (see
	// transferQuantity). Fields are integers, so part of a share arrives as
	// zero.
	ReasonTransferWithoutQuantity UnparsedReason = "transfer_without_quantity"
	// ReasonTransferQuantityContradicted: the quantity field and the
	// description's fraction disagree; neither is taken (see
	// transferQuantity).
	ReasonTransferQuantityContradicted UnparsedReason = "transfer_quantity_contradicted"
	// ReasonFundPayoutUnitsUnknown: a bond-repayment type on a paper the
	// catalog says is not a bond. A matured bond retires the whole holding; a
	// fund's payout retires units the broker does not name. Live
	// (2026-08-22): Т-Капитал redeemed 73 % of «Технологии Америки» and paid
	// it as BOND_REPAYMENT_FULL; booked as a full redemption it closed the
	// 27 % still held. Refused unless the units' withdrawal pairs with it
	// (pairFundRedemptions).
	ReasonFundPayoutUnitsUnknown UnparsedReason = "fund_payout_units_unknown"
	// ReasonForeignCurrencyNoRate: an operation in a currency other than its
	// position's, on a day whose official rate is not known yet (see
	// convertToPositionCurrency); it catches up.
	ReasonForeignCurrencyNoRate UnparsedReason = "foreign_currency_no_rate"
	// ReasonCommissionRefund: a positive commission, money back. FeeMinor
	// is a magnitude, so booking it would turn a refund into a charge (see
	// tradeCommission).
	ReasonCommissionRefund UnparsedReason = "commission_refund"
	// ReasonProjectionIncomplete: a shape in brokerOpTypes with no branch
	// built for it. Only a change to this file produces it, and it keeps such
	// a change visible.
	ReasonProjectionIncomplete UnparsedReason = "projection_incomplete"
	// ReasonBrokerFeeParentMissing: a broker fee naming a trade this
	// connection did not import. Whether it duplicates the trade's
	// commission depends on that trade (DeferredBrokerFeeVerdict).
	ReasonBrokerFeeParentMissing UnparsedReason = "broker_fee_parent_missing"
	// ReasonBrokerFeeParentExplained: a broker fee naming a trade the owner
	// explained by hand; only the owner knows whether the manual entry
	// includes it (see settleBrokerFees).
	ReasonBrokerFeeParentExplained UnparsedReason = "broker_fee_parent_explained"
)

// UnparsedError is one refusal: Reason, the contract's closed code that
// clients branch on, and Detail, prose about this row (which security, how much)
// that nothing may depend on. Both are stored and shown. Detail is built from the
// broker's operation, a passport or the journal, never anything secret.
type UnparsedError struct {
	Reason UnparsedReason
	Detail string
}

func (e *UnparsedError) Error() string {
	return fmt.Sprintf("tinvest: not projected (%s): %s", e.Reason, e.Detail)
}

// Deferred is a piece of a journal entry the mirror row lacks and the journal
// has. Not a refusal: the projection builds what it can and names what is owed;
// the rebuild, which sees every row in fold order, supplies it or refuses the row.
// A value rather than a zero, since zero is a quantity. It applies to the first
// entry of the projection, the one external ids are numbered from.
type Deferred uint8

const (
	// DeferredNothing: complete as built.
	DeferredNothing Deferred = iota
	// DeferredRedeemedQuantity: the first entry is a full redemption's sale,
	// with no quantity because the row has none; the count is the position
	// held then (Rebuilder.closeRedemptions). Ignored, the journal refuses a
	// sale without a quantity.
	DeferredRedeemedQuantity
	// DeferredBrokerFeeVerdict: a broker fee charged as its own operation,
	// whose fate depends on the trade in parent_operation_id. The broker
	// reports a commission in the trade's field and again as a BROKER_FEE:
	// 310 of the owner's 311 match to the kopeck (booking both would charge
	// 32 764 ₽ twice). The 311th (42 shares, 2023-11-02) has no commission
	// field and a fee of 11,34 ₽ that is the only record of that money.
	// Rebuilder.settleBrokerFees decides, with the trade in hand.
	DeferredBrokerFeeVerdict
)

// minorUnitScale: two decimal places per major unit, as everywhere in this
// program.
const minorUnitScale = 2

// maxAmountMinorDec is money.MaxAmountMinor as a decimal, compared before
// narrowing to int64.
var maxAmountMinorDec = decimal.NewFromInt(money.MaxAmountMinor)

// MinorFromDecimal converts a broker amount into minor units without rounding:
// the mirror keeps up to nine decimals, and this is a recorded figure everything
// else sums from, not a published one. A fraction finer than a minor unit is
// ReasonUnrepresentableAmount; beyond money.MaxAmountMinor (inclusive edge, as
// operation.validateFields) is ReasonAmountOutOfBounds. The fraction is checked
// first.
func MinorFromDecimal(d decimal.Decimal) (int64, error) {
	v, refusal := minorFromDecimal(d)
	if refusal != nil {
		return 0, refusal
	}
	return v, nil
}

// minorFromDecimal returns the typed refusal; the exported wrapper keeps a
// nil *UnparsedError from becoming a non-nil error.
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

// moscow is the broker's calendar as a fixed offset, since the image need not
// carry a tz database: UTC+3 with no seasonal change since October 2014.
var moscow = time.FixedZone("MSK", 3*60*60)

// mskDay is the Moscow calendar day of a broker instant, as UTC midnight. The
// broker reports UTC, but 23:30 in Moscow is already the next day; the Bank of
// Russia's rates and the tax calendar are Moscow days.
func mskDay(t time.Time) time.Time {
	m := t.In(moscow)
	return time.Date(m.Year(), m.Month(), m.Day(), 0, 0, 0, 0, time.UTC)
}

// shape is how a broker operation type becomes journal entries; the types
// are many, the shapes few.
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

// transferKind is which side of a securities move a leg is, by type.
type transferKind uint8

const (
	// transferNone: every non-transfer rule; a transfer shape iff a
	// transfer kind is checked by TestBrokerOpTypesPairShapeWithDirection.
	transferNone transferKind = iota
	// transferFromAnotherBroker: INPUT_SECURITIES; never paired.
	transferFromAnotherBroker
	// transferToAnotherBroker: shares left for outside (OUTPUT_SECURITIES).
	transferToAnotherBroker
	// transferBetweenOwnAccounts: TRANS_IIS_BS, TRANS_BS_BS. The same type
	// appears on both sides, so the direction is the quantity's sign.
	transferBetweenOwnAccounts
)

// rule is what one broker operation type projects into.
type rule struct {
	how shape
	// journal is the type the row becomes: buy or sell for asTrade, the cash
	// type for asCash, the redemption for asRedemption, the income leg for
	// asDividendToCard; unset for transfers, where direction decides.
	journal  operation.Type
	transfer transferKind
}

// brokerOpTypes maps the broker's operation types (wire names, as the mirror
// stores them) to the journal. Absence is the refusal: futures and options,
// variation margin, repo taxes, OVER_PLACEMENT, DIVIDEND_TRANSFER and anything
// new become ReasonUnsupportedType (owner's decision 5: shown unparsed until there
// is live data).
var brokerOpTypes = map[string]rule{
	// Trades. Card and margin variants record the same units and money;
	// margin's cost is its own fee type (MARGIN_FEE).
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

	// Income. A Russian dividend arrives gross with its tax as a separate,
	// unlinked operation, so neither is netted.
	"OPERATION_TYPE_DIVIDEND": {how: asCash, journal: operation.TypeDividend},
	"OPERATION_TYPE_COUPON":   {how: asCash, journal: operation.TypeCoupon},
	"OPERATION_TYPE_DIV_EXT":  {how: asDividendToCard, journal: operation.TypeDividend},

	// Taxes and their corrections, which may be refunds (see projectCash).
	// Repo taxes are out of scope and stay unparsed.
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
	"OPERATION_TYPE_BOND_REPAYMENT_FULL": {how: asRedemption, journal: operation.TypeRedemption},

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

// Notes this projection adds, in Russian: stored data shown verbatim, with
// no translation layer on this side.
const (
	// noteDividendToCard marks both legs of a dividend paid to a card.
	noteDividendToCard = "выплата на карту, минуя брокерский счёт"
	// noteBasisUnknown marks shares from another broker (INPUT_SECURITIES)
	// only; it says what the broker did, and stays after the owner states the
	// purchases.
	noteBasisUnknown = "стоимость приобретения брокер не передаёт"
	// noteFeeOtherCurrency marks a commission split off because it was
	// charged in another currency (see tradeCommission).
	noteFeeOtherCurrency = "комиссия сделки, списанная в другой валюте"
	// noteFundRedeemedUnits marks a fund redemption assembled from two rows,
	// naming the day the units left.
	noteFundRedeemedUnits = "паи выведены под погашение %s"
)

// ProjectRow turns one mirror row into zero, one or two entries and, for the
// one shape a single row cannot finish, names what the journal owes it
// (Deferred). resolved is the catalog instrument of the named security, or nil
// when the row names none; a row naming an unresolved security is refused. SpaceID
// is left unset: the write path takes it from the account.
func ProjectRow(row MirrorRow, accountID uuid.UUID, resolved *Resolved, traded *TradedCurrency) ([]operation.Operation, Deferred, *UnparsedError) {
	// A cancelled or withdrawn operation moved no money. Callers skip these
	// before resolving, but the rule holds here regardless.
	if row.State != stateExecuted || row.DisappearedAt != nil {
		return nil, DeferredNothing, nil
	}

	r, ok := brokerOpTypes[row.OpType]
	if !ok {
		return nil, DeferredNothing, &UnparsedError{
			Reason: ReasonUnsupportedType,
			Detail: fmt.Sprintf("broker operation type %q", row.OpType),
		}
	}

	// A currency trade is two conversion entries, handled before anything
	// else since the resolver does not resolve currencies. The traded
	// currency and its unit nominal come from CurrencyBy via traded; without
	// them the row stays unparsed.
	if r.how == asTrade && row.InstrumentType == brokerCurrencyInstrumentType {
		ops, refusal := projectCurrencyTrade(row, accountID, traded)
		if refusal != nil {
			return nil, DeferredNothing, refusal
		}
		return withExternalIDs(row.ID, ops), DeferredNothing, nil
	}

	var (
		ops      []operation.Operation
		deferred Deferred
		refusal  *UnparsedError
	)
	switch r.how {
	case asTrade:
		ops, refusal = projectTrade(row, accountID, resolved, r.journal)
	case asCash:
		ops, refusal = projectCash(row, accountID, resolved, r.journal)
	case asAmortization:
		ops, refusal = projectAmortization(row, accountID, resolved)
	case asRedemption:
		ops, deferred, refusal = projectRedemption(row, accountID, resolved, r.journal)
	case asDividendToCard:
		ops, refusal = projectDividendToCard(row, accountID, resolved)
	case asSecuritiesTransfer:
		ops, refusal = projectSecuritiesTransfer(row, accountID, resolved, r.transfer)
	case asBrokerFee:
		// Built like a cash entry and deferred (DeferredBrokerFeeVerdict).
		// Without the instrument: the security on a fee row is the trade's.
		// Reading it as the fee's refused the commissions of 79 currency trades
		// for a second, redundant reason and charged the fee to a position, when
		// it is money off the account; the commission capitalized into cost is
		// already on the purchase.
		ops, refusal = projectBrokerFee(row, accountID)
		if refusal == nil {
			deferred = DeferredBrokerFeeVerdict
		}
	default:
		// Unreachable from broker data; a shape added without a branch would
		// otherwise produce nothing and say nothing.
		refusal = &UnparsedError{
			Reason: ReasonProjectionIncomplete,
			Detail: fmt.Sprintf("broker operation type %q maps to projection shape %d, which nothing in this file builds", row.OpType, r.how),
		}
	}
	if refusal != nil {
		return nil, DeferredNothing, refusal
	}
	return withExternalIDs(row.ID, ops), deferred, nil
}

// base is what every entry takes from the row, in one place.
func base(row MirrorRow, accountID uuid.UUID, t operation.Type) operation.Operation {
	return operation.Operation{
		AccountID:  accountID,
		Type:       t,
		OccurredOn: mskDay(row.OccurredAt),
		Currency:   row.Currency,
		Note:       row.Description,
		// The broker's trading mode, or nil when it sent none.
		TradingMode: tradingModeOrNothing(row.ClassCode),
		Source:      Source,
	}
}

// tradingModeOrNothing is classCode as stored: nil when absent (money in
// and out carries none: 83 deposits and 52 withdrawals on the owner's
// account).
func tradingModeOrNothing(classCode string) *string {
	if classCode == "" {
		return nil
	}
	return &classCode
}

// projectTrade turns a purchase or sale into its entry, plus a fee entry when
// the commission was charged in another currency.
//
// The amount is the broker's payment; the commission becomes FeeMinor, which the
// engine adds to cost or subtracts from proceeds. The quantity is the executed
// one, in units (see OperationItem.QuantityDone).
//
// A bond trade's accrued interest is not read: the payment already includes it.
// All 173 of the owner's bond trades with accrued interest satisfy payment =
// quantity × price + accrued interest to the kopeck (115 ОФЗ 29008 on 2026-02-05:
// 115 × 1036,98 + 7868,30 = 127 121,00), and none satisfies it without.
//
// A bond's price here is money per bond, not the percent-of-par quote (78 ОФЗ
// 26226 at 989,60 paid 79 074,84). Nothing is computed from it.
func projectTrade(row MirrorRow, accountID uuid.UUID, resolved *Resolved, t operation.Type) ([]operation.Operation, *UnparsedError) {
	amount, refusal := minorFromDecimal(row.Payment)
	if refusal != nil {
		return nil, refusal
	}
	if row.QuantityDone <= 0 {
		return nil, &UnparsedError{
			Reason: ReasonTradeWithoutFill,
			Detail: fmt.Sprintf("the broker reports an order of %d units and no executed part of it", row.Quantity),
		}
	}
	qty, refusal := journalQuantity(row.QuantityDone)
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

// projectCurrencyTrade turns a currency purchase or sale into its two
// conversion entries: the payment as sent in the row's currency, and the mirror
// image in the traded currency, executed units × unit nominal. Neither names an
// instrument. Quantity × price must equal the payment (see
// checkMoneyDividesByUnits): 23 000 × 12.3565 = 284 199.50 on the owner's trades.
// The commission rides on the paid leg, or its own entry (tradeCommission).
func projectCurrencyTrade(row MirrorRow, accountID uuid.UUID, traded *TradedCurrency) ([]operation.Operation, *UnparsedError) {
	// An unfilled order gets the ordinary unfilled reason before any
	// currency lookup.
	if row.QuantityDone <= 0 {
		return nil, &UnparsedError{
			Reason: ReasonTradeWithoutFill,
			Detail: fmt.Sprintf("the broker reports an order of %d units and no executed part of it", row.Quantity),
		}
	}
	if traded == nil {
		return nil, &UnparsedError{
			Reason: ReasonCurrencyTrade,
			Detail: "a currency trade whose traded currency and nominal the broker would not say",
		}
	}
	paidMinor, refusal := minorFromDecimal(row.Payment)
	if refusal != nil {
		return nil, refusal
	}
	units := decimal.NewFromInt(row.QuantityDone)
	if refusal := checkMoneyDividesByUnits(row, units); refusal != nil {
		return nil, refusal
	}
	receivedMinor, refusal := minorFromDecimal(units.Mul(traded.NominalPerUnit))
	if refusal != nil {
		return nil, refusal
	}
	if paidMinor > 0 {
		// A sale: roubles came in, the traded currency went out.
		receivedMinor = -receivedMinor
	}

	paid := base(row, accountID, operation.TypeConversion)
	paid.AmountMinor = paidMinor

	received := base(row, accountID, operation.TypeConversion)
	received.AmountMinor = receivedMinor
	received.Currency = traded.Code

	feeMinor, feeLeg, refusal := tradeCommission(row, accountID)
	if refusal != nil {
		return nil, refusal
	}
	paid.FeeMinor = feeMinor
	if feeLeg == nil {
		return []operation.Operation{paid, received}, nil
	}
	return []operation.Operation{paid, received, *feeLeg}, nil
}

// checkMoneyDividesByUnits guards against a quantity that means something
// else, as the order size once did. It tolerates one minor unit: prices have six
// decimals and payments are rounded to the kopeck (942 yuan at 12.341497 is
// 11 625.690174, paid 11 625.69); an exact check refused 44 of 52 live trades. A
// misread quantity is off by a factor, not a kopeck. No price passes.
func checkMoneyDividesByUnits(row MirrorRow, units decimal.Decimal) *UnparsedError {
	if row.Price == nil || !row.Price.IsPositive() {
		return nil
	}
	expected := units.Mul(*row.Price).Shift(minorUnitScale).Round(0)
	paid := row.Payment.Abs().Shift(minorUnitScale).Round(0)
	if expected.Sub(paid).Abs().LessThanOrEqual(decimal.NewFromInt(1)) {
		return nil
	}
	return &UnparsedError{
		Reason: ReasonCurrencyTrade,
		Detail: fmt.Sprintf("the money does not divide by the units: %s units at %s is not a payment of %s",
			units, row.Price, row.Payment.Abs()),
	}
}

// tradePrice is the per-unit price to record, or nil for an absent, zero or
// negative one: an annotation nothing computes from, which validateByType would
// otherwise refuse with the whole trade. The mirror keeps no price currency, so
// the price is read in the row's currency, and the journal screen prints it so
// (#114). If a price currency is ever kept, this is where to compare.
func tradePrice(row MirrorRow) *decimal.Decimal {
	if row.Price == nil || !row.Price.IsPositive() {
		return nil
	}
	p := *row.Price
	return &p
}

// tradeCommission puts a trade's commission in its FeeMinor, or in a fee entry
// of its own when charged in another currency: a row holds one currency. That
// entry has no instrument, since the engine keeps a position's fees in one
// currency (portfolio.Type.mustMatchPositionCurrency). A positive commission is
// a refund and is refused (ReasonCommissionRefund): both places hold only
// charges, and the journal would take a flipped sign silently. Every commission
// on the owner's account is negative. Zero is ordinary. An amount with no
// currency is taken in the operation's currency rather than lost.
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
	if amount > 0 {
		return 0, nil, &UnparsedError{
			Reason: ReasonCommissionRefund,
			Detail: fmt.Sprintf("the commission of %s came back rather than being charged, and neither fee_minor nor a fee entry can hold money arriving", *row.Commission),
		}
	}
	amount = -amount
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

// projectCash turns an operation that is only money into its entry: top-up,
// withdrawal, income, tax, fee, interest.
//
// A positive tax (a refund) becomes a visible unparsed row. The journal's tax
// must be negative; booking it as a deposit would detach it from the position's
// income and invent a top-up, as income it would inflate dividends. Seven of the
// owner's nine TAX_CORRECTION rows are positive; all 67 ordinary taxes are
// negative. A negative correction is an ordinary tax; a zero is passed on and the
// journal refuses it.
//
// No other sign is rescued: wrong-signed withdrawals, dividends or fees go to the
// journal as sent and its refusal is shown. A commission field on a cash
// operation is not projected; none on the owner's account has one.
func projectCash(row MirrorRow, accountID uuid.UUID, resolved *Resolved, t operation.Type) ([]operation.Operation, *UnparsedError) {
	amount, refusal := minorFromDecimal(row.Payment)
	if refusal != nil {
		return nil, refusal
	}

	op := base(row, accountID, t)
	op.AmountMinor = amount
	if refusal := attachInstrument(&op, row, resolved); refusal != nil {
		return nil, refusal
	}
	return []operation.Operation{op}, nil
}

// projectBrokerFee turns a fee charged as its own operation into an
// account-level fee: projectCash without the instrument (see ProjectRow's
// asBrokerFee branch). Refunds are refused on the trade (tradeCommission).
func projectBrokerFee(row MirrorRow, accountID uuid.UUID) ([]operation.Operation, *UnparsedError) {
	amount, refusal := minorFromDecimal(row.Payment)
	if refusal != nil {
		return nil, refusal
	}
	op := base(row, accountID, operation.TypeFee)
	op.AmountMinor = amount
	return []operation.Operation{op}, nil
}

// projectAmortization turns a partial bond repayment into an amortization.
// No quantity: bonds stay in the position and only basis shrinks; a count would
// say bonds changed hands.
func projectAmortization(row MirrorRow, accountID uuid.UUID, resolved *Resolved) ([]operation.Operation, *UnparsedError) {
	if refusal := refuseFundPayout(row, resolved, "a partial repayment"); refusal != nil {
		return nil, refusal
	}
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

// projectRedemption turns a bond's full repayment into a redemption: the bonds
// leave and the money arrives (computed as a sale, see portfolio.TypeRedemption).
//
// Without a quantity it is built without one and deferred
// (DeferredRedeemedQuantity): all 23 of the owner's full redemptions arrived as
// money only (live, 2026-08-07). The count cannot be divided out of the money:
// Быстроденьги has a 100 CNY nominal and was redeemed in roubles. It is the
// position held at the time, filled in by Rebuilder.closeRedemptions. With a
// quantity it is a complete sale.
//
// A payout on anything but a bond is refused (refuseFundPayout; see
// ReasonFundPayoutUnitsUnknown). A commission is kept by tradeCommission's rule,
// though none of the 23 carries one.
func projectRedemption(row MirrorRow, accountID uuid.UUID, resolved *Resolved, t operation.Type) ([]operation.Operation, Deferred, *UnparsedError) {
	if refusal := refuseFundPayout(row, resolved, "a full redemption"); refusal != nil {
		return nil, DeferredNothing, refusal
	}
	amount, refusal := minorFromDecimal(row.Payment)
	if refusal != nil {
		return nil, DeferredNothing, refusal
	}
	// The type comes from the rule table.
	op := base(row, accountID, t)
	op.AmountMinor = amount
	op.Price = tradePrice(row)

	// Zero or negative: either way the count is the position. The order's
	// count is read, not the fill: a redemption is not an order, and both are
	// zero on every observed one.
	deferred := DeferredNothing
	if row.Quantity > 0 {
		qty, refusal := journalQuantity(row.Quantity)
		if refusal != nil {
			return nil, DeferredNothing, refusal
		}
		op.Quantity = &qty
	} else {
		deferred = DeferredRedeemedQuantity
	}
	if refusal := attachInstrument(&op, row, resolved); refusal != nil {
		return nil, DeferredNothing, refusal
	}

	feeMinor, feeLeg, refusal := tradeCommission(row, accountID)
	if refusal != nil {
		return nil, DeferredNothing, refusal
	}
	op.FeeMinor = feeMinor
	if feeLeg == nil {
		return []operation.Operation{op}, deferred, nil
	}
	return []operation.Operation{op, *feeLeg}, deferred, nil
}

// projectDividendToCard turns a dividend paid straight to a card into income
// plus the same money leaving the account that day; either alone would be wrong.
// Both legs carry the note.
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

// projectSecuritiesTransfer turns one side of a securities move into one leg;
// pairing is the rebuild's.
//
// The currency is the paper's (passport for a created row, catalog for a matched
// one), not the payment's: the payment is zero and its currency arbitrary
// ("rub"), while the engine fixes a position's currency from its first
// cost-bearing operation, so a rouble leg on a dollar paper would break the
// position. That a paper's passport currency is its trading currency is
// unverified on the owner's account, which has no non-rouble transfer, but it at
// least describes the paper. An unresolved leg is refused by attachInstrument;
// the nil guard is kept anyway.
//
// The basis is zero here: a departing leg's is released by the write path
// (operation.checkImportContract refuses a supplied one), and an arriving leg from
// another broker has none; only that leg carries noteBasisUnknown.
//
// For moves between the owner's accounts the direction is the quantity's sign,
// per the broker's documentation only (no TRANS_* operation on the owner's
// account). Zero is refused.
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
				Reason: ReasonTransferDirectionUnknown,
				Detail: "a transfer between accounts of zero units: nothing in the row says which way it went",
			}
		}
	}

	// The order's count: a transfer has no fill, both fields match, and the
	// direction is read from this sign.
	units := row.Quantity
	if units < 0 {
		units = -units
	}
	qty, refusal := transferQuantity(row, units)
	if refusal != nil {
		return nil, refusal
	}

	op := base(row, accountID, t)
	// Zero: a transfer's amount is a basis, set by the write path.
	op.AmountMinor = 0
	op.Quantity = &qty
	if resolved != nil {
		op.Currency = resolved.Currency
	}
	if kind == transferFromAnotherBroker {
		op.Note = withNote(row.Description, noteBasisUnknown)
	}
	if refusal := attachInstrument(&op, row, resolved); refusal != nil {
		return nil, refusal
	}
	return []operation.Operation{op}, nil
}

// attachInstrument sets, refuses or leaves the instrument by what the engine
// does with the type. A type the engine will not fold with one (deposit,
// withdrawal, interest) gets none and the named security is ignored. A type that
// can carry one is refused without a resolution when it requires one or names an
// unmatched security, so a dividend is never booked unattributed.
func attachInstrument(op *operation.Operation, row MirrorRow, resolved *Resolved) *UnparsedError {
	if !acceptsInstrument(op.Type) {
		return nil
	}
	if resolved != nil {
		// A copy, not a pointer into the caller's struct.
		id := resolved.InstrumentID
		op.InstrumentID = &id
		return nil
	}
	if op.Type.RequiresInstrument() || namesSecurity(row) {
		return instrumentRefusal(row)
	}
	return nil
}

// acceptsInstrument reports whether the engine folds this type with an
// instrument, mirroring portfolio.Compute's switch (plus conversion, skipped
// earlier). TestAcceptsInstrumentAgreesWithTheEngine asks the engine type by
// type.
func acceptsInstrument(t operation.Type) bool {
	switch t {
	case operation.TypeBuy, operation.TypeSell, operation.TypeRedemption,
		operation.TypeDividend, operation.TypeCoupon,
		operation.TypeTax, operation.TypeFee,
		operation.TypeAmortization,
		operation.TypeTransferIn, operation.TypeTransferOut,
		operation.TypeSplit, operation.TypeConversion:
		return true
	}
	return false
}

// namesSecurity reports whether the row names a paper by either identifier;
// old operations have had both rewritten.
func namesSecurity(row MirrorRow) bool {
	return row.InstrumentUID != "" || row.FIGI != ""
}

// instrumentRefusal says why there is no instrument, in the resolver's terms:
// an unsupported asset kind (by brokerInstrumentTypes) or an unmatched one.
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

// journalQuantity brings a unit count onto the journal's scale before it
// leaves, as operation.normalizeForStorage does. With an int64 input the
// truncation is a no-op and the refusal unreachable (TestJournalQuantity says so);
// they are kept together for the day the count stops being an integer.
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

// describedFraction is the one figure read from the broker's prose: a number
// with a dot and a fractional part right before the unit word ("Завод 0.24 акций",
// "Вывод 44380.35 лотов", both from the owner's account). Whole numbers are left
// to the integer field; commas have never been seen and are not guessed.
var describedFraction = regexp.MustCompile(`(?:^|\s)(\d+\.\d+)\s+(?:акци[яий]|лот(?:ов|а)?|па[её]в|па[йя]|штук[аи]?)(?:\s|$|[.,])`)

// transferQuantity is the units a transfer moved. Every quantity field is an
// integer, so part of a share arrives as 0 ("Завод 0.24 акций Warner Bros.
// Discovery из другого депозитария") and fractional fund units as their whole part
// ("Вывод 44380.35 лотов фонда Технологии Америки", field 44380). A fraction in the
// description is taken only when its whole part equals the field; otherwise the
// row is refused as contradicted. Without a fraction the field stands, and a zero
// field is ReasonTransferWithoutQuantity.
func transferQuantity(row MirrorRow, units int64) (decimal.Decimal, *UnparsedError) {
	if m := describedFraction.FindStringSubmatch(row.Description); m != nil {
		// The pattern admits only digits and a dot; refuse rather than panic
		// mid-sync.
		described, err := decimal.NewFromString(m[1])
		if err != nil {
			return decimal.Zero, &UnparsedError{
				Reason: ReasonTransferWithoutQuantity,
				Detail: fmt.Sprintf("the description's figure %q could not be read as a number: %v", m[1], err),
			}
		}
		if !described.Truncate(0).Equal(decimal.NewFromInt(units)) {
			return decimal.Zero, &UnparsedError{
				Reason: ReasonTransferQuantityContradicted,
				Detail: fmt.Sprintf("the broker's quantity field says %d units and its description says %s: %q", units, described.String(), row.Description),
			}
		}
		q := described.Truncate(portfolio.QuantityScale)
		if !q.IsPositive() {
			return decimal.Zero, &UnparsedError{
				Reason: ReasonUnrepresentableQty,
				Detail: fmt.Sprintf("%s units is finer than the %d decimal places the journal records", described.String(), portfolio.QuantityScale),
			}
		}
		return q, nil
	}
	if units == 0 {
		// Only for the one-sided kinds; between own accounts a zero is refused
		// earlier. The reader learns the broker sent no number, not a journal
		// rule.
		return decimal.Zero, &UnparsedError{
			Reason: ReasonTransferWithoutQuantity,
			Detail: fmt.Sprintf("the broker reports no units for this transfer and its description names no fraction to read: %q", row.Description),
		}
	}
	return journalQuantity(units)
}

// refuseFundPayout guards both bond-repayment shapes: the broker sends a
// fund's payouts under bond types, and bond rules are false for a fund (see
// ReasonFundPayoutUnitsUnknown). The catalog type decides; an unresolved row is
// left to attachInstrument. what names the shape in the detail.
func refuseFundPayout(row MirrorRow, resolved *Resolved, what string) *UnparsedError {
	if resolved == nil || resolved.Type == instrument.TypeBond {
		return nil
	}
	return &UnparsedError{
		Reason: ReasonFundPayoutUnitsUnknown,
		Detail: fmt.Sprintf("%s of %s %s on %q, whose catalog row is %s and not a bond: a fund pays out against units it does not name, and the count a bond's redemption takes from the position is not this payout's to take",
			what, row.Payment.String(), row.Currency, row.Description, resolved.Type),
	}
}

// withExternalIDs names each entry of one row "<id>/1", "/2" in build order,
// so a rebuild of an unchanged mirror updates rather than duplicates. A lone
// entry is suffixed too: a row can change shape (a commission later reported in
// another currency adds an entry), and a renamed "<id>" -> "<id>/1" would be a
// new record to the journal's key.
func withExternalIDs(rowID uuid.UUID, ops []operation.Operation) []operation.Operation {
	for i := range ops {
		id := fmt.Sprintf("%s%d", externalIDPrefix(rowID), i+1)
		ops[i].ExternalID = &id
	}
	return ops
}

// externalIDPrefix is the part of a name that identifies the mirror row. It is
// the one statement of the name's shape for both directions: building (here) and
// matching a row's entries (EntriesOfRows).
func externalIDPrefix(rowID uuid.UUID) string {
	return rowID.String() + "/"
}

// EntriesOfRows picks the journal entries the given mirror rows produced, by
// name; an explanation replaces them. Unnamed rows of this source are not this
// projection's and are left alone.
func EntriesOfRows(journal []operation.Operation, rows []MirrorRow) []uuid.UUID {
	prefixes := make([]string, 0, len(rows))
	for _, m := range rows {
		prefixes = append(prefixes, externalIDPrefix(m.ID))
	}
	var ids []uuid.UUID
	for _, o := range journal {
		if o.ExternalID == nil {
			continue
		}
		for _, p := range prefixes {
			if strings.HasPrefix(*o.ExternalID, p) {
				ids = append(ids, o.ID)
				break
			}
		}
	}
	return ids
}

// withNote appends this program's mark to the broker's description.
func withNote(description, mark string) string {
	if description == "" {
		return mark
	}
	return description + " — " + mark
}
