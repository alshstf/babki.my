package corporateaction_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata/yahoo"
)

// stubSplitsFeed knows Amazon: the 2022 split, and two «splits» that are the
// feed's price adjustments for spin-offs, one in whole numbers.
type stubSplitsFeed struct{}

func (stubSplitsFeed) SymbolFor(_ context.Context, isin string) (string, bool, error) {
	return "AMZN", isin == amazonISIN, nil
}

func (stubSplitsFeed) Splits(context.Context, string, time.Time, time.Time) ([]yahoo.Split, error) {
	return []yahoo.Split{
		{On: date("2022-06-06"), Numerator: decimal.NewFromInt(20), Denominator: decimal.NewFromInt(1)},
		{On: date("2023-03-01"), Numerator: decimal.RequireFromString("1.0526"), Denominator: decimal.NewFromInt(1)},
		{On: date("2025-02-24"), Numerator: decimal.NewFromInt(1323), Denominator: decimal.NewFromInt(1000)},
	}, nil
}

func (stubSplitsFeed) SplitsURL(symbol string) string { return "https://feed.example/" + symbol }

// A foreign paper's split, published by the feed, is recorded and reaches the
// holder's journal: one share bought before Amazon's split is twenty after. A
// price adjustment filed as a split is left out.
func TestAForeignPapersSplitReachesItsHoldersFromTheFeed(t *testing.T) {
	f := newFixture(t)
	f.buy(t, f.accountID, "2022-01-10", "1", -330_000)

	w := corporateaction.NewRefreshForeignSplitsWorker(f.store, f.materializer, f.ops, instrument.NewStore(f.pool),
		stubSplitsFeed{}, slog.Default())
	if err := w.Work(f.ctx, &river.Job[corporateaction.RefreshForeignSplitsArgs]{JobRow: &rivertype.JobRow{ID: 1}}); err != nil {
		t.Fatal(err)
	}

	events, err := f.store.ByISIN(f.ctx, amazonISIN)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Source != corporateaction.SourceYahoo || events[0].RatioFrom != 1 || events[0].RatioTo != 20 ||
		events[0].SourceRef != "https://feed.example/AMZN" {
		t.Fatalf("events = %+v, want the 1:20 split from the feed alone", events)
	}
	if got := f.held(t, f.accountID); !got.Equal(decimal.NewFromInt(20)) {
		t.Errorf("held = %s, want 20", got)
	}
}
