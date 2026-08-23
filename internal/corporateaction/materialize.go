package corporateaction

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

// journalReader is the slice of operation.Store the materialization reads
// through; *operation.Store satisfies it structurally. Declared here, narrow,
// for the same reason marketdata declares its own: what this package needs of
// the journal is two reads, and a package-wide dependency would let it grow
// quietly into more.
type journalReader interface {
	ListForEngine(ctx context.Context, spaceID, accountID uuid.UUID) ([]operation.Operation, error)
}

// journalWriter is the one write. It is operation.Service's importer door
// rather than the store's, because everything that door does is needed here:
// removals and insertions in one transaction, the engine asked about the
// journal the difference LEAVES, and the stored rows replayed once more before
// the commit.
type journalWriter interface {
	ApplyImportDelta(ctx context.Context, spaceID uuid.UUID, d operation.ImportDelta) (
		[]operation.Operation, []operation.ImportRefusal, error)
}

// Materializer carries the registry's facts into the journals of the accounts
// that held the paper.
//
// IT IS NOT INCREMENTAL. Every run recomputes the rows the registry now asks
// for and diffs them against the rows it wrote last time — the same shape the
// T-Invest rebuild uses against its mirror, and for the same reason: an event
// corrected, a ratio fixed, an account that only now has a purchase old enough
// to be split, and a rule this program changed all reach the journal by one
// path. There is no "what is new since last time" to ask, because an event
// arriving today can be dated 2021 and lands underneath four years of trades.
type Materializer struct {
	store   *Store
	journal journalReader
	ops     journalWriter
	log     *slog.Logger
}

func NewMaterializer(store *Store, journal journalReader, ops journalWriter, log *slog.Logger) *Materializer {
	if log == nil {
		log = slog.Default()
	}
	return &Materializer{store: store, journal: journal, ops: ops, log: log}
}

// Stats is what one run did, for the log line and for the tests to read.
type Stats struct {
	Added, Removed, Refused int
}

func (s *Stats) add(o Stats) {
	s.Added += o.Added
	s.Removed += o.Removed
	s.Refused += o.Refused
}

// ForISIN brings every account that has ever traded this paper into line with
// the registry.
func (m *Materializer) ForISIN(ctx context.Context, isin string) (Stats, error) {
	events, err := m.store.ByISIN(ctx, isin)
	if err != nil {
		return Stats{}, fmt.Errorf("corporateaction: read the events of %s: %w", isin, err)
	}
	holders, err := m.store.holders(ctx, isin)
	if err != nil {
		return Stats{}, err
	}
	// One account can hold the paper under more than one catalog row only in a
	// database older than migration 0020, which made the ISIN unique. Grouping
	// by account rather than by (account, instrument) is what keeps such an
	// account's journal folded ONCE with every row's events in it, instead of
	// twice with each fold blind to the other's rows.
	type accountKey struct{ spaceID, accountID uuid.UUID }
	instruments := map[accountKey][]uuid.UUID{}
	order := []accountKey{}
	for _, h := range holders {
		key := accountKey{h.spaceID, h.accountID}
		if _, seen := instruments[key]; !seen {
			order = append(order, key)
		}
		instruments[key] = append(instruments[key], h.instrumentID)
	}

	var total Stats
	for _, key := range order {
		stats, err := m.forAccount(ctx, key.spaceID, key.accountID, instruments[key], events)
		if err != nil {
			return total, err
		}
		total.add(stats)
	}
	return total, nil
}

// ForAccount brings one account into line with the registry, for every paper
// its journal touches. It is what runs after somebody writes an operation by
// hand: a purchase dated before a split needs that split's row, and the
// registry has known about the split all along.
func (m *Materializer) ForAccount(ctx context.Context, spaceID, accountID uuid.UUID) (Stats, error) {
	isins, err := m.store.isinsOfAccount(ctx, spaceID, accountID)
	if err != nil {
		return Stats{}, err
	}
	var total Stats
	for _, isin := range isins {
		events, err := m.store.ByISIN(ctx, isin)
		if err != nil {
			return total, fmt.Errorf("corporateaction: read the events of %s: %w", isin, err)
		}
		if len(events) == 0 {
			continue
		}
		holders, err := m.store.holders(ctx, isin)
		if err != nil {
			return total, err
		}
		var ours []uuid.UUID
		for _, h := range holders {
			if h.spaceID == spaceID && h.accountID == accountID {
				ours = append(ours, h.instrumentID)
			}
		}
		if len(ours) == 0 {
			continue
		}
		stats, err := m.forAccount(ctx, spaceID, accountID, ours, events)
		if err != nil {
			return total, err
		}
		total.add(stats)
	}
	return total, nil
}

// All sweeps the whole registry. It is the safety net behind the synchronous
// triggers rather than the normal path: a trigger that failed after its write
// had committed leaves the journal a row short, and nothing else would ever
// notice.
func (m *Materializer) All(ctx context.Context) (Stats, error) {
	isins, err := m.store.DistinctISINs(ctx)
	if err != nil {
		return Stats{}, err
	}
	var total Stats
	for _, isin := range isins {
		stats, err := m.ForISIN(ctx, isin)
		if err != nil {
			return total, err
		}
		total.add(stats)
	}
	return total, nil
}

// forAccount is the whole of the arithmetic: what the registry asks this
// account's journal to hold, against what it holds, applied as a difference.
func (m *Materializer) forAccount(ctx context.Context, spaceID, accountID uuid.UUID,
	instrumentIDs []uuid.UUID, events []Event,
) (Stats, error) {
	journal, err := m.journal.ListForEngine(ctx, spaceID, accountID)
	if err != nil {
		return Stats{}, fmt.Errorf("corporateaction: read the journal of account %s: %w", accountID, err)
	}

	ours := make(map[uuid.UUID]bool, len(instrumentIDs))
	for _, id := range instrumentIDs {
		ours[id] = true
	}

	// The rows this materialization owns on this account: registry rows on the
	// catalog rows of this paper. They are recomputed from scratch below, so
	// they are held out of the journal the recomputation folds — otherwise the
	// quantity a split is decided on would already have that split in it.
	//
	// WHEN CONVERSIONS AND SPIN-OFFS ARE MATERIALIZED this ownership rule needs
	// revisiting: those write a row onto the paper they PRODUCE as well, and a
	// row on that instrument would belong to this ISIN's event while sitting
	// under another ISIN's name. Today only splits are carried into journals
	// and a split touches nothing but its own paper, so the two readings agree.
	owned := map[uuid.UUID]operation.Operation{}
	base := make([]operation.Operation, 0, len(journal))
	for _, o := range journal {
		if o.Source == operation.SourceRegistry && o.InstrumentID != nil && ours[*o.InstrumentID] {
			owned[o.ID] = o
			continue
		}
		base = append(base, o)
	}

	want, err := m.desired(base, accountID, instrumentIDs, events)
	if err != nil {
		return Stats{}, err
	}

	delta, err := diff(want, owned)
	if err != nil {
		return Stats{}, err
	}
	if len(delta.Add) == 0 && len(delta.Remove) == 0 {
		return Stats{}, nil
	}

	_, refused, err := m.ops.ApplyImportDelta(ctx, spaceID, delta)
	if err != nil {
		return Stats{}, fmt.Errorf("corporateaction: write the registry's rows into account %s: %w", accountID, err)
	}
	for _, r := range refused {
		// A refusal here is not a broker's odd data, which is what the import
		// path's refusals usually are: it is this program asking the journal to
		// hold a split it cannot hold. It is logged loudly and the run
		// continues, because the other accounts' rows are not at fault — and
		// nothing on a screen can report it yet, which is why the log has to.
		m.log.Error("corporateaction: the journal would not take a split the registry asks for",
			"account", accountID, "event", r.ExternalID, "err", r.Err)
	}
	return Stats{Added: len(delta.Add) - len(refused), Removed: len(delta.Remove), Refused: len(refused)}, nil
}

// desired is the set of journal rows the registry asks this account to hold.
//
// Events are applied in date order and the working journal grows as it goes,
// because each one acts on the holding the ones before it left: a paper that
// split ten for one in 2021 and two for one in 2024 is held in the 2021 answer
// when the 2024 event asks whether anything is held at all.
func (m *Materializer) desired(base []operation.Operation, accountID uuid.UUID,
	instrumentIDs []uuid.UUID, events []Event,
) ([]operation.Operation, error) {
	var want []operation.Operation
	working := base
	for _, e := range events {
		if !e.Kind.Materialized() {
			continue
		}
		for _, instrumentID := range instrumentIDs {
			held, err := heldAtStartOf(working, instrumentID, e.EffectiveOn)
			if err != nil {
				// The account's journal does not replay even without this
				// event. Nothing this package writes can put that right, and
				// deciding a split against a position the engine will not
				// compute would be inventing one — so the paper is left alone
				// and the failure is reported, not swallowed.
				return nil, fmt.Errorf("corporateaction: account %s does not replay, so no event can be applied to it: %w",
					accountID, err)
			}
			if !held.IsPositive() {
				continue
			}
			if hasForeignSplit(working, instrumentID, e.EffectiveOn) {
				// Somebody else's split of this paper on this very day is
				// already in the journal. Adding ours would multiply the
				// holding twice for one corporate action. It cannot happen
				// through any door this program has today — the hand-entry path
				// refuses a split outright and no importer writes one — so this
				// is about journals written before that was true, and about
				// rows a future writer might add.
				m.log.Warn("corporateaction: a split of this paper on this date is already in the journal, leaving it alone",
					"account", accountID, "instrument", instrumentID, "on", e.EffectiveOn.Format(time.DateOnly))
				continue
			}
			row := splitRow(e, accountID, instrumentID, held)
			want = append(want, row)
			working = append(working, row)
		}
	}
	return want, nil
}

// splitRow is the journal row one event asks one account for.
//
// The currency is the position's own, and it is read from the holding rather
// than stated: the engine requires every operation that touches cost or
// quantity to repeat the currency the position was settled in
// (portfolio.Type.mustMatchPositionCurrency lists split among them), so a row
// carrying anything else is refused — correctly, and after the fact. Reading it
// off the position means the question never arises.
func splitRow(e Event, accountID, instrumentID uuid.UUID, held heldPosition) operation.Operation {
	ratio := e.Ratio()
	externalID := externalIDFor(e, accountID, instrumentID)
	return operation.Operation{
		AccountID:    accountID,
		InstrumentID: &instrumentID,
		Type:         operation.TypeSplit,
		OccurredOn:   e.EffectiveOn,
		Currency:     held.currency,
		SplitRatio:   &ratio,
		Source:       operation.SourceRegistry,
		ExternalID:   &externalID,
		Note:         splitNote(e),
	}
}

// splitNote is what the journal row says about itself on the screen. RUSSIAN
// because it is data rather than code: it travels into the journal and is shown
// verbatim, exactly as the T-Invest projection's notes are (there is no
// translation layer on this side — t() translates the interface, never a stored
// note).
//
// It names the ratio the way the exchange publishes it and the source the fact
// came from, so a reader looking at a quantity that changed on its own can see
// in one line what changed it and who said so.
func splitNote(e Event) string {
	switch e.Source {
	case SourceMOEX:
		return fmt.Sprintf("Дробление %d:%d — из реестра корпоративных действий (Московская биржа)", e.RatioFrom, e.RatioTo)
	default:
		return fmt.Sprintf("Дробление %d:%d — из реестра корпоративных действий (внесено вручную)", e.RatioFrom, e.RatioTo)
	}
}

// externalIDFor names the row one event produces on one account's holding of
// one catalog row.
//
// ALL THREE PARTS ARE NEEDED. The event and the account are obvious. The
// instrument is there because a single account can hold one ISIN under two
// catalog rows in a database older than migration 0020, and both need a split
// of their own — with the instrument left out, the two rows would collide on
// the journal's (account, source, external id) index and the second would be
// refused for ever.
//
// It is deterministic, which is what makes the difference below a matching
// rather than a guess: the same event recomputed produces the same name, so the
// row already in the journal is recognised as the row this run is asking for.
func externalIDFor(e Event, accountID, instrumentID uuid.UUID) string {
	return fmt.Sprintf("%s:%s:%s", e.ID, accountID, instrumentID)
}

// heldPosition is what the fold says about a holding at a moment: how much, and
// in what currency the cost is denominated.
type heldPosition struct {
	quantity decimal.Decimal
	currency string
}

// IsPositive reports whether anything is held at all.
func (h heldPosition) IsPositive() bool { return h.quantity.IsPositive() }

// heldAtStartOf folds the journal up to, but not including, the given day and
// reports what was held then.
//
// UP TO AND NOT INCLUDING is the whole rule about when an event applies: the
// effective date is the first day the paper trades in the new quantity, so what
// the split multiplies is the holding at the CLOSE OF THE DAY BEFORE, and a
// trade dated the effective day is already in the new quantity. Ordering the
// row first within its day (see operation.foldRank) is the other half of the
// same statement — this decides whether to write it, that decides where it
// folds.
func heldAtStartOf(journal []operation.Operation, instrumentID uuid.UUID, day time.Time) (heldPosition, error) {
	before := make([]operation.Operation, 0, len(journal))
	for _, o := range journal {
		if o.OccurredOn.Before(day) {
			before = append(before, o)
		}
	}
	positions, err := portfolio.Compute(before)
	if err != nil {
		return heldPosition{}, err
	}
	p, ok := positions[instrumentID]
	if !ok {
		return heldPosition{}, nil
	}
	return heldPosition{quantity: p.Quantity, currency: p.Currency}, nil
}

// hasForeignSplit reports whether the journal already carries a split of this
// paper on this day that this package did not write.
func hasForeignSplit(journal []operation.Operation, instrumentID uuid.UUID, day time.Time) bool {
	for _, o := range journal {
		if o.Type != operation.TypeSplit || o.Source == operation.SourceRegistry {
			continue
		}
		if o.InstrumentID != nil && *o.InstrumentID == instrumentID && o.OccurredOn.Equal(day) {
			return true
		}
	}
	return false
}

// diff turns "what the registry asks for" and "what it wrote last time" into
// the delta the journal takes.
//
// MATCHED BY EXTERNAL ID, which is deterministic (see externalIDFor), so a row
// already saying what this run says is left exactly as it stands — no removal,
// no insertion, and above all no new created_at. A row that says something else
// is removed and rewritten, INHERITING the stamp of the row it replaces: within
// a day the journal folds by stamp, that order breaks ties in the FIFO queue,
// and a ratio corrected from 1:100 to 1:1000 must not also move where the row
// sits in its day. The write path requires that inheritance to be from a row
// the same delta removes and refuses anything else (see
// operation.checkInheritedStamps).
//
// Anything the registry wrote that nothing now asks for is removed: an event
// deleted, an account that turned out to hold nothing on the day, a paper whose
// last purchase was itself removed.
//
// It is not the T-Invest rebuild's difference and does not share code with it.
// That one carries bookkeeping this has no counterpart for — which mirror row
// each journal row came from, and transfers whose two legs are accepted or
// refused as one — and the part they have in common is the three lines below.
// The rule they share is stated in both places rather than abstracted into a
// helper neither of them would read.
func diff(want []operation.Operation, owned map[uuid.UUID]operation.Operation) (operation.ImportDelta, error) {
	byName := make(map[string]operation.Operation, len(owned))
	for _, o := range owned {
		if o.ExternalID == nil || *o.ExternalID == "" {
			// Not a row this package wrote: everything it hands over is named.
			// Left in owned so the loop below removes it — a nameless registry
			// row is one nothing can ever ask for again.
			continue
		}
		byName[*o.ExternalID] = o
	}

	var delta operation.ImportDelta
	kept := map[uuid.UUID]bool{}
	for _, w := range want {
		stored, ok := byName[*w.ExternalID]
		if ok && sameSplitRow(w, stored) {
			kept[stored.ID] = true
			continue
		}
		if ok {
			delta.Remove = append(delta.Remove, stored.ID)
			w.CreatedAt = stored.CreatedAt
		}
		delta.Add = append(delta.Add, w)
	}
	for id, o := range owned {
		if kept[id] {
			continue
		}
		if slicesContains(delta.Remove, id) {
			continue
		}
		delta.Remove = append(delta.Remove, o.ID)
	}
	return delta, nil
}

// sameSplitRow reports whether the stored row already says what this run says.
//
// EVERY FIELD THIS PACKAGE SETS IS COMPARED. A field left out would be a field
// the registry could no longer correct: the note would go on describing a ratio
// the row no longer carries, or a row would keep an instrument the paper no
// longer maps to, and every run would report nothing to do.
func sameSplitRow(want, stored operation.Operation) bool {
	if want.AccountID != stored.AccountID || want.Type != stored.Type ||
		want.Currency != stored.Currency || want.Note != stored.Note ||
		want.Source != stored.Source {
		return false
	}
	if !want.OccurredOn.Equal(stored.OccurredOn) {
		return false
	}
	if want.InstrumentID == nil || stored.InstrumentID == nil || *want.InstrumentID != *stored.InstrumentID {
		return false
	}
	if want.SplitRatio == nil || stored.SplitRatio == nil || !want.SplitRatio.Equal(*stored.SplitRatio) {
		return false
	}
	return true
}

func slicesContains(ids []uuid.UUID, id uuid.UUID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
