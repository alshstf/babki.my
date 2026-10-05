package tinvest

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// What makes a withdrawal and a payout one redemption, case by case: the same
// paper, a fund, the withdrawal first and within the window — and anything
// else leaves the two apart.
func TestPairFundRedemptionsPairsOnlyTheSamePaperBeforeThePayout(t *testing.T) {
	payDay := time.Date(2025, 10, 29, 14, 0, 0, 0, time.UTC)
	row := func(opType, instrumentType, asset string, at time.Time) MirrorRow {
		return MirrorRow{
			ID: uuid.New(), OpType: opType, InstrumentType: instrumentType, AssetUID: asset,
			State: stateExecuted, OccurredAt: at,
		}
	}
	out := func(instrumentType, asset string, at time.Time) MirrorRow {
		return row("OPERATION_TYPE_OUTPUT_SECURITIES", instrumentType, asset, at)
	}
	pay := row("OPERATION_TYPE_BOND_REPAYMENT_FULL", "etf", "asset-tech", payDay)
	twoWeeksBefore := payDay.AddDate(0, 0, -14)

	for name, c := range map[string]struct {
		withdrawal MirrorRow
		want       bool
	}{
		"the same fund, two weeks before": {out("etf", "asset-tech", twoWeeksBefore), true},
		"another fund":                    {out("etf", "asset-tspx", twoWeeksBefore), false},
		"after the payout":                {out("etf", "asset-tech", payDay.Add(time.Hour)), false},
		"outside the window":              {out("etf", "asset-tech", payDay.Add(-fundPayoutWindow-time.Hour)), false},
		"a bond, not a fund":              {out("bond", "asset-tech", twoWeeksBefore), false},
		"no paper named":                  {out("etf", "", twoWeeksBefore), false},
	} {
		t.Run(name, func(t *testing.T) {
			paidFor, withdrawn := pairFundRedemptions([]MirrorRow{c.withdrawal, pay})
			_, paired := paidFor[pay.ID]
			if paired != c.want || withdrawn[c.withdrawal.ID] != c.want {
				t.Errorf("paired = %v (withdrawal marked %v), want %v", paired, withdrawn[c.withdrawal.ID], c.want)
			}
		})
	}
}
