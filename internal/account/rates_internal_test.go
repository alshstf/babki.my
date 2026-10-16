package account

import (
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// A deposit at 16 % from January, then tied to the key rate less 1.5 from
// September: nothing before January, the fixed rate in summer, the key rate's
// in autumn — not below zero, and unknown without the key rate.
func TestRateOn(t *testing.T) {
	day := func(s string) time.Time { d, _ := time.Parse(time.DateOnly, s); return d }
	rates := []Rate{
		{From: day("2026-01-01"), Kind: RateFixed, Percent: decimal.NewFromInt(16)},
		{From: day("2026-09-01"), Kind: RateKey, Percent: decimal.RequireFromString("-1.5"), ByHand: true},
	}
	key := decimal.NewFromInt(17)
	keyOn := func(time.Time) (decimal.Decimal, bool, error) { return key, true, nil }

	if _, _, ok, _ := RateOn(rates, day("2025-12-31"), keyOn); ok {
		t.Error("a rate before the first")
	}
	if pct, row, ok, _ := RateOn(rates, day("2026-08-31"), keyOn); !ok || !pct.Equal(decimal.NewFromInt(16)) || row.Kind != RateFixed {
		t.Errorf("summer = %v %+v %v, want 16 fixed", pct, row, ok)
	}
	if pct, row, ok, _ := RateOn(rates, day("2026-09-01"), keyOn); !ok || pct.String() != "15.5" || !row.ByHand {
		t.Errorf("autumn = %v %+v %v, want 15.5 by hand", pct, row, ok)
	}
	key = decimal.NewFromInt(1)
	if pct, _, ok, _ := RateOn(rates, day("2026-10-01"), keyOn); !ok || !pct.IsZero() {
		t.Errorf("key 1 less 1.5 = %v, want 0", pct)
	}
	none := func(time.Time) (decimal.Decimal, bool, error) { return decimal.Zero, false, nil }
	if _, row, ok, _ := RateOn(rates, day("2026-10-01"), none); ok || row == nil {
		t.Errorf("without the key rate = %+v %v, want the row and no rate", row, ok)
	}
	broken := errors.New("db")
	if _, _, _, err := RateOn(rates, day("2026-10-01"), func(time.Time) (decimal.Decimal, bool, error) { return decimal.Zero, false, broken }); !errors.Is(err, broken) {
		t.Errorf("err = %v", err)
	}
}
