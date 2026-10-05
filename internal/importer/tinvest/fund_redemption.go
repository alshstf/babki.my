package tinvest

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/operation"
)

// brokerFundInstrumentType is the broker's instrumentType for a fund.
const brokerFundInstrumentType = "etf"

// fundPayoutWindow is how long after the units of a fund leave the account its
// payout may come and still be read as the payout for them. On the owner's
// account the two redemptions of October 2025 paid 14 days after the units went.
const fundPayoutWindow = 62 * 24 * time.Hour

// pairFundRedemptions finds, among one link's rows, each fund payout and the
// withdrawal of the units it pays for (decision Р-13).
//
// A fund redeemed by its manager shows at the broker as two rows: the units
// leave as a withdrawal to another depository, and the money comes some days
// later as a full or partial repayment. Neither says what the other does — the
// payout names no units, the withdrawal no money — and the two even name
// different listings of the paper (the withdrawal an over-the-counter one), so
// they are paired by the paper itself (asset_uid), never by the listing.
//
// A pair is made only when nothing else could be meant: one withdrawal of the
// paper in the window before the payout, and that withdrawal claimed by no
// other payout. Anything less certain stays as it was — the withdrawal a
// transfer out, the payout a visible unparsed row.
//
// Returns, by payout row id, the withdrawal it pays for; and the ids of the
// withdrawals so paired.
func pairFundRedemptions(rows []MirrorRow) (map[uuid.UUID]MirrorRow, map[uuid.UUID]bool) {
	live := func(m MirrorRow) bool {
		return m.State == stateExecuted && m.DisappearedAt == nil &&
			m.InstrumentType == brokerFundInstrumentType && m.AssetUID != ""
	}
	var withdrawals, payouts []MirrorRow
	for _, m := range rows {
		if !live(m) {
			continue
		}
		switch m.OpType {
		case "OPERATION_TYPE_OUTPUT_SECURITIES":
			withdrawals = append(withdrawals, m)
		case "OPERATION_TYPE_BOND_REPAYMENT_FULL", "OPERATION_TYPE_BOND_REPAYMENT":
			payouts = append(payouts, m)
		}
	}
	candidates := map[uuid.UUID][]MirrorRow{}
	claims := map[uuid.UUID]int{}
	for _, pay := range payouts {
		for _, w := range withdrawals {
			if w.AssetUID != pay.AssetUID || w.OccurredAt.After(pay.OccurredAt) || pay.OccurredAt.Sub(w.OccurredAt) > fundPayoutWindow {
				continue
			}
			candidates[pay.ID] = append(candidates[pay.ID], w)
			claims[w.ID]++
		}
	}
	paidFor := map[uuid.UUID]MirrorRow{}
	withdrawn := map[uuid.UUID]bool{}
	for payID, ws := range candidates {
		if len(ws) != 1 || claims[ws[0].ID] != 1 {
			continue
		}
		paidFor[payID] = ws[0]
		withdrawn[ws[0].ID] = true
	}
	return paidFor, withdrawn
}

// projectFundRedemption books a fund payout paired with its withdrawal as one
// redemption on the day of the payout: the withdrawn units leave the position
// and the money arrives. Not a sale — the holder did not sell; the result is
// computed the same way (see portfolio.TypeRedemption).
func projectFundRedemption(payout, withdrawal MirrorRow, accountID uuid.UUID, resolved *Resolved) ([]operation.Operation, *UnparsedError) {
	units := withdrawal.Quantity
	if units < 0 {
		units = -units
	}
	qty, refusal := transferQuantity(withdrawal, units)
	if refusal != nil {
		return nil, refusal
	}
	amount, refusal := minorFromDecimal(payout.Payment)
	if refusal != nil {
		return nil, refusal
	}
	op := base(payout, accountID, operation.TypeRedemption)
	op.AmountMinor = amount
	op.Quantity = &qty
	op.Note = withNote(payout.Description, fmt.Sprintf(noteFundRedeemedUnits, mskDay(withdrawal.OccurredAt).Format("02.01.2006")))
	if refusal := attachInstrument(&op, payout, resolved); refusal != nil {
		return nil, refusal
	}
	feeMinor, feeLeg, refusal := tradeCommission(payout, accountID)
	if refusal != nil {
		return nil, refusal
	}
	op.FeeMinor = feeMinor
	if feeLeg == nil {
		return []operation.Operation{op}, nil
	}
	return []operation.Operation{op, *feeLeg}, nil
}
