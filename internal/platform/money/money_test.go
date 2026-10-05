package money_test

import (
	"errors"
	"math"
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/platform/money"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// Rounding is half away from zero, so a negative amount never shrinks.
func TestMinorRoundsHalfAwayFromZero(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"150.5", 151},
		{"-150.5", -151},
		{"150.4", 150},
		{"-150.4", -150},
		{"0", 0},
	} {
		got, err := money.Minor(dec(tc.in))
		if err != nil {
			t.Fatalf("Minor(%s): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("Minor(%s) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestMinorPublishesTheWholeInt64Range(t *testing.T) {
	for _, want := range []int64{math.MaxInt64, math.MinInt64} {
		got, err := money.Minor(decimal.NewFromInt(want))
		if err != nil {
			t.Fatalf("Minor(%d): %v — a figure exactly at the edge still fits", want, err)
		}
		if got != want {
			t.Errorf("Minor(%d) = %d", want, got)
		}
	}
}

func TestMinorRefusesOneUnitPastTheEdge(t *testing.T) {
	for _, in := range []string{"9223372036854775808", "-9223372036854775809"} {
		got, err := money.Minor(dec(in))
		if !errors.Is(err, money.ErrOverflow) {
			t.Fatalf("Minor(%s) err = %v, want ErrOverflow", in, err)
		}
		if got != 0 {
			t.Errorf("Minor(%s) = %d alongside the refusal, want 0", in, got)
		}
	}
}

// decimal.IntPart wraps 1e30 to 5076944270305263616; Minor must refuse it (#27).
func TestMinorRefusesAFigureThatWouldWrap(t *testing.T) {
	huge := dec("1e30")
	if wrapped := huge.IntPart(); wrapped != 5076944270305263616 {
		t.Fatalf("decimal(1e30).IntPart() = %d, want 5076944270305263616 — the wrapping this guard exists for has changed shape", wrapped)
	}
	got, err := money.Minor(huge)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("Minor(1e30) = %d, err = %v, want ErrOverflow", got, err)
	}
}

// The range is checked after rounding: a value a fraction above the maximum
// rounds onto it and fits.
func TestMinorDecidesAfterRounding(t *testing.T) {
	fits, err := money.Minor(dec("9223372036854775807.4"))
	if err != nil {
		t.Fatalf("Minor(maxint64 + 0.4): %v — it rounds down onto the maximum", err)
	}
	if fits != math.MaxInt64 {
		t.Errorf("Minor(maxint64 + 0.4) = %d, want %d", fits, int64(math.MaxInt64))
	}

	if _, err := money.Minor(dec("9223372036854775807.6")); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("Minor(maxint64 + 0.6) err = %v, want ErrOverflow — it rounds up past the maximum", err)
	}
}

// Go's int64 addition wraps silently; Add must refuse (#83).
func TestAddRefusesATotalThatWouldWrap(t *testing.T) {
	// A variable: the constant expression would not compile.
	largest := int64(math.MaxInt64)
	if wrapped := largest + 1; wrapped != math.MinInt64 {
		t.Fatalf("maxint64 + 1 = %d, want %d — the wrapping this guard exists for has changed shape", wrapped, int64(math.MinInt64))
	}
	got, err := money.Add(math.MaxInt64, 1)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("Add(maxint64, 1) = %d, err = %v; want ErrOverflow", got, err)
	}
	if got != 0 {
		t.Errorf("Add returned %d alongside the refusal, want 0", got)
	}
}

func TestAddPublishesTheLargestTotalThatFits(t *testing.T) {
	got, err := money.Add(math.MaxInt64-1, 1)
	if err != nil {
		t.Fatalf("Add(maxint64-1, 1): %v — the total is exactly maxint64 and is a figure", err)
	}
	if got != math.MaxInt64 {
		t.Errorf("Add(maxint64-1, 1) = %d, want %d", got, int64(math.MaxInt64))
	}
}

// Sub is not Add with a negated argument: negating MinInt64 overflows.
func TestSubRefusesADifferenceThatWouldWrap(t *testing.T) {
	got, err := money.Sub(math.MinInt64, 1)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("Sub(minint64, 1) = %d, err = %v; want ErrOverflow", got, err)
	}
	if got != 0 {
		t.Errorf("Sub returned %d alongside the refusal, want 0", got)
	}
	// -1 − MinInt64 is exactly MaxInt64.
	fits, err := money.Sub(-1, math.MinInt64)
	if err != nil {
		t.Fatalf("Sub(-1, minint64): %v — the difference is maxint64 and is a figure; only Add(a, -b) fails here", err)
	}
	if fits != math.MaxInt64 {
		t.Errorf("Sub(-1, minint64) = %d, want %d", fits, int64(math.MaxInt64))
	}
}
