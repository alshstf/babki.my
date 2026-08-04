package money_test

import (
	"errors"
	"math"
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/platform/money"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// TestMinorRoundsHalfAwayFromZero pins the rounding this codebase already
// applies wherever a decimal figure becomes money — .Round(0), half away from
// zero, so a negative amount never shrinks in magnitude. Minor took that step
// over from the call sites; if it ever rounded differently, every converted
// figure in the application would move by up to a minor unit at once.
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

// TestMinorPublishesTheWholeInt64Range checks the two ends themselves. A guard
// that refuses one unit too early would quietly stop publishing a figure that
// is perfectly representable, which is the same class of harm as publishing a
// wrapped one: a number the owner is entitled to, missing with no explanation.
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

// TestMinorRefusesOneUnitPastTheEdge is the other half of the boundary: the
// smallest figure that does NOT fit must be refused, not wrapped.
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

// TestMinorRefusesAFigureThatWouldWrap is the defect itself (#27):
// decimal.IntPart() past int64 does not fail, it wraps — 1e30 comes back as
// 5076944270305263616, a positive, plausible, entirely wrong amount of money.
// The wrapped value is named here rather than merely "some error expected", so
// that a guard removed by mistake is caught by the very number it would let
// through.
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

// TestMinorDecidesAfterRounding, not before. The figure that gets published is
// the ROUNDED one, so that is the figure the range question is about: a value
// a fraction above the maximum rounds down onto it and fits, and one a
// fraction higher rounds up past it and does not. Checking the unrounded value
// instead would refuse the first of these — a representable figure withheld
// over a fraction of a kopeck.
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

// TestAddRefusesATotalThatWouldWrap and its neighbours are about figures that
// are ALREADY int64 minor units. Go's + and - on int64 wrap exactly as quietly
// as decimal.IntPart() does past its range, and the terms of a total are each
// perfectly ordinary by construction — they had to survive Minor to become
// int64 at all. The guard therefore belongs on the total, which is the figure
// that gets published (#83).
func TestAddRefusesATotalThatWouldWrap(t *testing.T) {
	// Through a variable: the same expression written with constants does not
	// compile, which is precisely why this wrap is invisible in real code — the
	// values there are variables.
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

// TestAddPublishesTheLargestTotalThatFits is the other side of the same edge: a
// total landing exactly on the last int64 is a real figure and must be handed
// back, not refused one unit early.
func TestAddPublishesTheLargestTotalThatFits(t *testing.T) {
	got, err := money.Add(math.MaxInt64-1, 1)
	if err != nil {
		t.Fatalf("Add(maxint64-1, 1): %v — the total is exactly maxint64 and is a figure", err)
	}
	if got != math.MaxInt64 {
		t.Errorf("Add(maxint64-1, 1) = %d, want %d", got, int64(math.MaxInt64))
	}
}

// TestSubRefusesADifferenceThatWouldWrap covers the other operator. It is not
// Add with a negated argument: negating math.MinInt64 overflows on its own, so
// a difference is taken as a difference.
func TestSubRefusesADifferenceThatWouldWrap(t *testing.T) {
	got, err := money.Sub(math.MinInt64, 1)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("Sub(minint64, 1) = %d, err = %v; want ErrOverflow", got, err)
	}
	if got != 0 {
		t.Errorf("Sub returned %d alongside the refusal, want 0", got)
	}
	// The term that cannot be negated, subtracted rather than added. -1 minus
	// minint64 is exactly maxint64 — a figure, and this must hand it back — while
	// negating minint64 first overflows on its own and would refuse it.
	fits, err := money.Sub(-1, math.MinInt64)
	if err != nil {
		t.Fatalf("Sub(-1, minint64): %v — the difference is maxint64 and is a figure; only Add(a, -b) fails here", err)
	}
	if fits != math.MaxInt64 {
		t.Errorf("Sub(-1, minint64) = %d, want %d", fits, int64(math.MaxInt64))
	}
}
