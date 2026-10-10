package marketdata

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/platform/testdb"
)

// stubSchedules knows one bond: two coupons, the second not yet set, and the
// redemption.
type stubSchedules struct{ asked *[]string }

func (s stubSchedules) BondSchedule(_ context.Context, isin, _ string) ([]BondEvent, bool, error) {
	*s.asked = append(*s.asked, isin)
	if isin != "RU000A1038V6" {
		return nil, false, nil
	}
	v := decimal.RequireFromString("35.4")
	face := decimal.RequireFromString("1000")
	return []BondEvent{
		{Kind: BondCoupon, On: day("2026-11-18"), Value: &v, Currency: "RUB"},
		{Kind: BondCoupon, On: day("2027-05-19"), Currency: "RUB"},
		{Kind: BondRedemption, On: day("2041-05-15"), Value: &face, Currency: "RUB"},
	}, true, nil
}

func (stubSchedules) Name() string { return "stub-exchange" }

// Every bond the journals name gets its schedule stored, replaced whole on the
// next run; a share is not asked about.
func TestABondsScheduleIsStoredWhole(t *testing.T) {
	pool := testdb.New(t)
	ctx := t.Context()
	store, papers := NewStore(pool), instrument.NewStore(pool)
	mk := func(typ instrument.Type, name, isin string) uuid.UUID {
		p, err := papers.Create(ctx, instrument.Instrument{Type: typ, Name: name, Ticker: name, ISIN: isin, Currency: "RUB"})
		if err != nil {
			t.Fatal(err)
		}
		return p.ID
	}
	ofz := mk(instrument.TypeBond, "SU26238RMFS4", "RU000A1038V6")
	sber := mk(instrument.TypeShare, "SBER", "RU0009029540")
	// Something stale, to be replaced.
	old := decimal.RequireFromString("1")
	if err := store.ReplaceBondEvents(ctx, ofz, "stub-exchange", []BondEvent{
		{InstrumentID: ofz, Source: "stub-exchange", Kind: BondCoupon, On: day("2020-01-01"), Value: &old, Currency: "RUB"},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	var asked []string
	w := NewBondScheduleWorker(store, firstDays{ofz: day("2025-06-10"), sber: day("2025-06-10")}, papers,
		stubSchedules{&asked}, slog.Default()).(*bondScheduleWorker)
	if err := w.Work(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(asked, []string{"RU000A1038V6"}) {
		t.Errorf("asked %v, want the bond alone", asked)
	}
	got, err := store.BondEventsBetween(ctx, []uuid.UUID{ofz, sber}, day("2019-01-01"), day("2030-12-31"))
	if err != nil {
		t.Fatal(err)
	}
	events := got[ofz]
	if len(events) != 2 || events[0].Kind != BondCoupon || events[0].Value == nil || events[0].Value.String() != "35.4" ||
		events[1].Value != nil || events[0].Source != "stub-exchange" {
		t.Errorf("the bond's events to 2030 = %+v, want the two coupons, the second not yet set, the stale one gone", events)
	}
	if len(got[sber]) != 0 {
		t.Errorf("a share got a schedule: %+v", got[sber])
	}
	all, err := store.BondEventsBetween(ctx, []uuid.UUID{ofz}, day("2019-01-01"), day("2041-12-31"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(all[ofz]); n != 3 || all[ofz][2].Kind != BondRedemption {
		t.Errorf("the whole schedule = %+v, want three events ending in the redemption", all[ofz])
	}
}
