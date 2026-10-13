package goals

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func ptr[T any](v T) *T { return &v }

// The note's family (Р-44), on 13.10.2026: the holiday — 70 000 saved, 20 000
// a month, 250 000 by 01.07.2027 — falls short, 22 500 a month would do; the
// first payment — 800 000 and 30 000 a month by 01.09.2029 — falls short
// without growth, is ahead at 10 %; the cushion with no date reaches its
// 570 000 in six months — by April — at 15 000 a month and 14 %.
func TestProgress(t *testing.T) {
	today := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	holiday := Goal{Target: ptr(int64(250_000_00)), TargetOn: ptr(time.Date(2027, 7, 1, 0, 0, 0, 0, time.UTC)), Monthly: ptr(int64(20_000_00))}
	p := progress(holiday, 70_000_00, today)
	if p.Status != AtRisk || p.Months != 8 || *p.Projected != 230_000_00 || *p.Needed != 22_500_00 {
		t.Errorf("holiday = %+v, projected %d, needed %d", p, *p.Projected, *p.Needed)
	}
	first := Goal{Target: ptr(int64(2_000_000_00)), TargetOn: ptr(time.Date(2029, 9, 1, 0, 0, 0, 0, time.UTC)), Monthly: ptr(int64(30_000_00))}
	if p := progress(first, 800_000_00, today); p.Status != AtRisk || p.Months != 34 || *p.Projected != 1_820_000_00 {
		t.Errorf("first payment at 0 %% = %+v", p)
	}
	first.Yield = decimal.NewFromInt(10)
	if p := progress(first, 800_000_00, today); p.Status != Ahead || *p.Projected < 2_230_000_00 || *p.Projected > 2_240_000_00 {
		t.Errorf("first payment at 10 %% = %+v, projected %d", p, *p.Projected)
	}
	cushion := Goal{Target: ptr(int64(570_000_00)), Monthly: ptr(int64(15_000_00)), Yield: decimal.NewFromInt(14)}
	if p := progress(cushion, 450_000_00, today); p.ReachOn == nil || p.Months != 6 || p.Status != NoStatus {
		t.Errorf("cushion = %+v", p)
	}
	if p := progress(Goal{Target: ptr(int64(100_00))}, 200_00, today); p.Status != Done {
		t.Errorf("done = %+v", p)
	}
}
