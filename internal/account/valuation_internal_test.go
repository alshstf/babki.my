package account

import (
	"testing"
	"time"

	"babki.my/babki/internal/platform/apitypes"
)

// Within 1% of the balance the journal agrees with it, within 5% it is close,
// past that it differs; a balance more than three days old is not graded.
func TestReconciliationIsGradedByHowFarApart(t *testing.T) {
	now := time.Date(2026, 10, 2, 21, 30, 0, 0, time.UTC)
	today := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		diff, balance int64
		asOf          time.Time
		want          apitypes.AccountReconciliationStatus
	}{
		{0, 0, today, apitypes.Agrees},
		{1, 0, today, apitypes.Differs},
		{1_000, 100_000, today, apitypes.Agrees},
		{1_001, 100_000, today, apitypes.Close},
		{-5_000, 100_000, today, apitypes.Close},
		{-5_001, 100_000, today, apitypes.Differs},
		{500, -100_000, today, apitypes.Agrees},
		{100, 100_000, today.AddDate(0, 0, -3), apitypes.Agrees},
		{100, 100_000, today.AddDate(0, 0, -4), apitypes.Stale},
	} {
		if got := reconciliationStatus(tc.diff, tc.balance, tc.asOf, now); got != tc.want {
			t.Errorf("%d against %d on %s = %s, want %s", tc.diff, tc.balance, tc.asOf.Format("2006-01-02"), got, tc.want)
		}
	}
}
