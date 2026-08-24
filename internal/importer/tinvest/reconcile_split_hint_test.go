package tinvest

import (
	"context"
	"net/http"
	"testing"
	"time"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
)

// stubRegistry answers the split question without a registry behind it, and
// records what it was asked. An empty map means "the registry knows of no split
// at all", which is the state the owner's AMZN and NVDA are actually in.
type stubRegistry struct {
	known map[string]bool
	asked []string
	err   error
}

func (s *stubRegistry) HasSplitOnOrBefore(_ context.Context, isin string, _ time.Time) (bool, error) {
	s.asked = append(s.asked, isin)
	if s.err != nil {
		return false, s.err
	}
	return s.known[isin], nil
}

// hintFixture is one paper mapped to one broker position, and a journal holding
// `ours` of it against the broker's `theirs`.
func hintFixture(t *testing.T, ours, theirs string, registry splitRegistry) []ReconcileMismatch {
	t.Helper()
	f := newFixture(t)
	inst := f.instrumentWithISIN(t, "AMZN", "US0231351067")
	if err := f.store.saveMap(f.ctx, f.conn.ID, inst.ID,
		InstrumentRef{InstrumentUID: "uid-amzn"}, inst.ISIN, inst.Ticker, "USD"); err != nil {
		t.Fatalf("saveMap: %v", err)
	}
	srv, _ := serve(t, map[string]route{
		portfolioPath: {status: http.StatusOK, body: []byte(
			`{"positions":[{"instrumentUid":"uid-amzn","instrumentType":"share",` +
				`"quantity":{"units":"` + theirs + `","nano":0},"blocked":false}]}`)},
		positionsPath: {status: http.StatusOK, body: []byte(`{"money":[]}`)},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)

	r := NewReconciler(f.store, fakeJournal{ops: []operation.Operation{
		aBuy(inst.ID, ours, -320_000, 0, "USD"),
	}}, newMarker(), instrument.NewStore(f.pool), registry, nil)

	res, err := r.ReconcileLink(f.ctx, c, f.conn, f.link)
	if err != nil {
		t.Fatalf("ReconcileLink: %v", err)
	}
	return securitiesMismatches(res)
}

// TestADifferenceByAWholeFactorAsksAboutASplit is the owner's own screen: AMZN
// at 1 against the broker's 20, which is Amazon's 20:1 of June 2022 that nobody
// recorded. The broker reports no corporate action of any kind — its operation
// enum carries 71 values and not one of them is a split — so the only way this
// ever becomes a question is for the check to notice the shape of the
// difference and say so.
func TestADifferenceByAWholeFactorAsksAboutASplit(t *testing.T) {
	registry := &stubRegistry{}
	got := hintFixture(t, "1", "20", registry)
	if len(got) != 1 {
		t.Fatalf("got %d differences about papers, want 1: %+v", len(got), got)
	}
	if got[0].SplitHintFactor == nil {
		t.Fatalf("no split hint on 1 against 20, though the registry holds no split of this paper")
	}
	if *got[0].SplitHintFactor != 20 {
		t.Errorf("split_hint_factor = %d, want 20 — the larger quantity over the smaller",
			*got[0].SplitHintFactor)
	}
	if len(registry.asked) != 1 || registry.asked[0] != "US0231351067" {
		t.Errorf("the registry was asked about %v, want the paper's own ISIN once", registry.asked)
	}
}

// TestAReverseSplitShapedDifferenceAsksToo: the journal holding the multiple is
// the same question read the other way, and a hint that only looked one way
// would be silent on exactly half the cases.
func TestAReverseSplitShapedDifferenceAsksToo(t *testing.T) {
	got := hintFixture(t, "30", "3", &stubRegistry{})
	if len(got) != 1 {
		t.Fatalf("got %d differences about papers, want 1: %+v", len(got), got)
	}
	if got[0].SplitHintFactor == nil || *got[0].SplitHintFactor != 10 {
		t.Fatalf("split_hint_factor = %v on 30 against 3, want 10", got[0].SplitHintFactor)
	}
}

// TestADifferenceTheRegistryAlreadyExplainsAsksNothing. Once the event is
// recorded the split is in the journal and this difference is gone — so a hint
// over a paper the registry knows about could only point at a row that is
// already there and already right.
func TestADifferenceTheRegistryAlreadyExplainsAsksNothing(t *testing.T) {
	got := hintFixture(t, "1", "20", &stubRegistry{known: map[string]bool{"US0231351067": true}})
	if len(got) != 1 {
		t.Fatalf("got %d differences about papers, want 1: %+v", len(got), got)
	}
	if got[0].SplitHintFactor != nil {
		t.Errorf("split_hint_factor = %d on a paper the registry already holds a split of: the "+
			"difference has some other cause and this points at the wrong place",
			*got[0].SplitHintFactor)
	}
}

// TestADifferenceThatIsNotAWholeMultipleAsksNothing. Nineteen against twenty is
// a purchase nobody recorded, not a split — and a hint there would send a reader
// looking for a corporate action that never happened.
func TestADifferenceThatIsNotAWholeMultipleAsksNothing(t *testing.T) {
	registry := &stubRegistry{}
	got := hintFixture(t, "19", "20", registry)
	if len(got) != 1 {
		t.Fatalf("got %d differences about papers, want 1: %+v", len(got), got)
	}
	if got[0].SplitHintFactor != nil {
		t.Errorf("split_hint_factor = %d on 19 against 20, which is no multiple at all",
			*got[0].SplitHintFactor)
	}
	if len(registry.asked) != 0 {
		t.Errorf("the registry was asked about %v though the difference is no multiple: the "+
			"question is only worth asking about a shape that looks like a split", registry.asked)
	}
}
