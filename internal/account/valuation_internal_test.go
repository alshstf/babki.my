package account

import (
	"testing"

	"babki.my/babki/internal/platform/apitypes"
)

// Within 1% of the balance the journal agrees with it, within 5% it is close,
// past that it differs.
func TestReconciliationIsGradedByHowFarApart(t *testing.T) {
	for _, tc := range []struct {
		diff, balance int64
		want          apitypes.AccountReconciliationStatus
	}{
		{0, 0, apitypes.Agrees},
		{1, 0, apitypes.Differs},
		{1_000, 100_000, apitypes.Agrees},
		{1_001, 100_000, apitypes.Close},
		{-5_000, 100_000, apitypes.Close},
		{-5_001, 100_000, apitypes.Differs},
		{500, -100_000, apitypes.Agrees},
	} {
		if got := reconciliationStatus(tc.diff, tc.balance); got != tc.want {
			t.Errorf("%d against %d = %s, want %s", tc.diff, tc.balance, got, tc.want)
		}
	}
}
