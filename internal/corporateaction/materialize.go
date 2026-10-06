package corporateaction

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

// journalWriter is operation.Service's importer door: removals and insertions
// in one transaction, judged by the journal they leave, replayed as stored before
// the commit.
type journalWriter interface {
	BuildAndApplyImportDelta(ctx context.Context, spaceID, accountID uuid.UUID,
		build func(journal []operation.Operation) (operation.ImportDelta, error),
	) (operation.ImportDelta, []operation.Operation, []operation.ImportRefusal, error)
}

// rechecker asks for a fresh broker comparison for the accounts a run changed:
// a verdict describes the journal when it was struck, and this package changes
// journals underneath it (see tinvest.Rechecker). Declared here so the registry
// does not import the importer. Nil means nothing is asked.
type rechecker interface {
	QueueRecheckForAccounts(ctx context.Context, accountIDs []uuid.UUID) (int, error)
}

// catalog finds the paper a conversion or spin-off produces: the registry names
// it by ISIN, a journal row by instrument id. *instrument.Store satisfies it.
type catalog interface {
	ByISIN(ctx context.Context, isin string) (instrument.Instrument, error)
}

// Materializer carries the registry's facts into the journals of the accounts
// that held the paper. Every run recomputes the rows and diffs them against what
// it wrote last time: an event recorded today can be dated 2021, so there is no
// "new since last time".
type Materializer struct {
	store   *Store
	ops     journalWriter
	papers  catalog
	recheck rechecker
	log     *slog.Logger
}

// NewMaterializer wires the registry to the journal. recheck may be nil;
// papers may not.
func NewMaterializer(store *Store, ops journalWriter,
	papers catalog, recheck rechecker, log *slog.Logger,
) *Materializer {
	if log == nil {
		log = slog.Default()
	}
	return &Materializer{store: store, ops: ops, papers: papers, recheck: recheck, log: log}
}

// Stats is what one run did. Accounts are the accounts whose journals changed,
// not those looked at, so a no-op sweep asks the broker for nothing.
type Stats struct {
	Added, Removed, Refused int
	Accounts                []uuid.UUID
}

func (s *Stats) add(o Stats) {
	s.Added += o.Added
	s.Removed += o.Removed
	s.Refused += o.Refused
	s.Accounts = append(s.Accounts, o.Accounts...)
}

// ForISIN brings every account that has traded this paper into line.
func (m *Materializer) ForISIN(ctx context.Context, isin string) (Stats, error) {
	events, err := m.store.ByISIN(ctx, isin)
	if err != nil {
		return Stats{}, fmt.Errorf("corporateaction: read the events of %s: %w", isin, err)
	}
	holders, err := m.store.holders(ctx, isin)
	if err != nil {
		return Stats{}, err
	}
	// Grouped by account, not (account, instrument): a database older than
	// migration 0020 can hold one ISIN under two catalog rows, and the account's
	// journal must be folded once with both rows' events.
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

	// One account's failure is reported without costing the others their rows.
	var total Stats
	var failed []error
	for _, key := range order {
		stats, err := m.forAccount(ctx, key.spaceID, key.accountID, instruments[key], events)
		if err != nil {
			failed = append(failed, err)
			continue
		}
		total.add(stats)
	}
	return total, errors.Join(failed...)
}

// ForAccount brings one account into line for every paper in its journal the
// registry knows about; it runs after a hand entry (see AfterManualWrite).
func (m *Materializer) ForAccount(ctx context.Context, spaceID, accountID uuid.UUID) (Stats, error) {
	papers, err := m.store.eventPapersOfAccount(ctx, spaceID, accountID)
	if err != nil {
		return Stats{}, err
	}
	var total Stats
	var failed []error
	for _, paper := range papers {
		events, err := m.store.ByISIN(ctx, paper.isin)
		if err != nil {
			failed = append(failed, fmt.Errorf("corporateaction: read the events of %s: %w", paper.isin, err))
			continue
		}
		stats, err := m.forAccount(ctx, spaceID, accountID, paper.instrumentIDs, events)
		if err != nil {
			failed = append(failed, err)
			continue
		}
		total.add(stats)
	}
	return total, errors.Join(failed...)
}

// AfterManualWrite runs after a committed hand entry
// (operation.Service.OnManualWrite): the touched accounts are brought into line
// and their broker connections asked for a fresh check. Failures are logged and
// left to the daily sweep; the entry must not fail.
func (m *Materializer) AfterManualWrite(ctx context.Context, spaceID uuid.UUID, accountIDs []uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), materializeTimeout)
	defer cancel()

	var total Stats
	seen := make(map[uuid.UUID]bool, len(accountIDs))
	for _, accountID := range accountIDs {
		if seen[accountID] {
			continue
		}
		seen[accountID] = true
		stats, err := m.ForAccount(ctx, spaceID, accountID)
		total.add(stats)
		if err != nil {
			m.log.Error("corporateaction: a hand entry was recorded but the registry's rows were not brought into line",
				"account", accountID, "err", err)
		}
	}
	m.RequestRecheck(ctx, total)
}

// All sweeps the whole registry: the safety net for a trigger that failed after
// its write committed.
func (m *Materializer) All(ctx context.Context) (Stats, error) {
	isins, err := m.store.DistinctISINs(ctx)
	if err != nil {
		return Stats{}, err
	}
	// ForISIN returns what it did along with what it could not do.
	var total Stats
	var failed []error
	for _, isin := range isins {
		stats, err := m.ForISIN(ctx, isin)
		total.add(stats)
		if err != nil {
			failed = append(failed, err)
		}
	}
	return total, errors.Join(failed...)
}

// forAccount computes what the registry asks this account's journal to hold,
// against what it holds, and applies the difference.
func (m *Materializer) forAccount(ctx context.Context, spaceID, accountID uuid.UUID,
	instrumentIDs []uuid.UUID, events []Event,
) (Stats, error) {
	ours := make(map[uuid.UUID]bool, len(instrumentIDs))
	for _, id := range instrumentIDs {
		ours[id] = true
	}

	// The rows this package owns on the account are held out of the folded
	// journal, or a split would be decided on a quantity that already has it.
	//
	// Ownership is read off the row's external id, not its instrument: a
	// conversion's arriving leg sits on the produced paper, and the id names
	// the source paper on both legs (see externalIDFor), even after the event
	// is deleted.
	//
	// Read, compute and write happen under one lock on the account.
	build := func(journal []operation.Operation) (operation.ImportDelta, error) {
		owned := map[uuid.UUID]operation.Operation{}
		base := make([]operation.Operation, 0, len(journal))
		for _, o := range journal {
			if o.Source == operation.SourceRegistry && ownedByThisPaper(o, ours) {
				owned[o.ID] = o
				continue
			}
			base = append(base, o)
		}
		want, err := m.desired(ctx, base, accountID, instrumentIDs, events)
		if err != nil {
			return operation.ImportDelta{}, err
		}
		return diff(want, owned)
	}

	delta, _, refused, err := m.ops.BuildAndApplyImportDelta(ctx, spaceID, accountID, build)
	if err != nil {
		return Stats{}, fmt.Errorf("corporateaction: bring account %s into line with the registry: %w", accountID, err)
	}
	if len(delta.Add) == 0 && len(delta.Remove) == 0 {
		return Stats{}, nil
	}
	for _, r := range refused {
		// This program asking the journal for a split it cannot hold: logged
		// loudly, and the other accounts continue. No screen reports it yet.
		m.log.Error("corporateaction: the journal would not take a split the registry asks for",
			"account", accountID, "event", r.ExternalID, "err", r.Err)
	}
	return Stats{
		Added:    len(delta.Add) - len(refused),
		Removed:  len(delta.Remove),
		Refused:  len(refused),
		Accounts: []uuid.UUID{accountID},
	}, nil
}

// RequestRecheck asks for a fresh broker comparison for the accounts a run
// changed and reports how many were queued. The caller decides: the API handler
// before answering, the daily sweep and the exchange job for their own rows. A
// failure is logged and swallowed; the write already happened, and the worst case
// is a verdict stale until the hourly run.
func (m *Materializer) RequestRecheck(ctx context.Context, stats Stats) int {
	if m.recheck == nil || len(stats.Accounts) == 0 {
		return 0
	}
	queued, err := m.recheck.QueueRecheckForAccounts(ctx, stats.Accounts)
	if err != nil {
		m.log.Error("corporateaction: the journals were written but no fresh check could be queued",
			"accounts", len(stats.Accounts), "err", err)
	}
	return queued
}

// desired is the set of rows the registry asks this account to hold. Events
// are applied in date order over a growing working journal, so each acts on the
// holding the earlier ones left.
func (m *Materializer) desired(ctx context.Context, base []operation.Operation, accountID uuid.UUID,
	instrumentIDs []uuid.UUID, events []Event,
) ([]operation.Operation, error) {
	var want []operation.Operation
	working := slices.Clone(base)
	for _, e := range events {
		if !e.Kind.Materialized() {
			continue
		}
		// The produced paper, resolved once per event. Without a catalog row the
		// event is skipped; Store.NotCountedReason tells the reader why.
		var result *uuid.UUID
		if e.ResultISIN != "" {
			id, err := m.resultInstrument(ctx, e)
			if err != nil {
				return nil, err
			}
			if id == nil {
				m.log.Info("corporateaction: the paper this event produces is not in the catalog, so nothing is written for it",
					"event", e.ID, "isin", e.ISIN, "result_isin", e.ResultISIN)
				continue
			}
			result = id
		}
		for _, instrumentID := range instrumentIDs {
			held, err := heldAtStartOf(working, instrumentID, e.EffectiveOn)
			if err != nil {
				// The journal does not replay even without this event; deciding a
				// split against a position the engine will not compute would invent
				// one, so the paper is left alone and the failure reported.
				return nil, fmt.Errorf("corporateaction: account %s does not replay, so no event can be applied to it: %w",
					accountID, err)
			}
			if !held.IsPositive() {
				continue
			}
			if e.Source == SourceKnown && !heldInRoubles(working, instrumentID) {
				// The exchange replaced only receipts held through Russian
				// depositories, bought for roubles; one bought for currency at a
				// foreign broker stayed a receipt.
				continue
			}
			if e.Kind == KindSplit && hasForeignSplit(working, instrumentID, e.EffectiveOn) {
				// Another split of this paper on this day is already in the
				// journal; adding ours would multiply twice. No door writes one
				// today, so this guards older journals and future writers.
				m.log.Warn("corporateaction: a split of this paper on this date is already in the journal, leaving it alone",
					"account", accountID, "instrument", instrumentID, "on", e.EffectiveOn.Format(time.DateOnly))
				continue
			}
			rows, err := m.rowsFor(e, accountID, instrumentID, result, held, working)
			if err != nil {
				// The event cannot be expressed on this account (a holding too
				// small, a share that rounds to nothing); other accounts go on.
				m.log.Warn("corporateaction: this account's holding cannot take the event, leaving it alone",
					"account", accountID, "instrument", instrumentID, "event", e.ID, "err", err)
				continue
			}
			want = append(want, rows...)
			// Back into fold order for the next event.
			working = append(working, rows...)
			operation.SortJournal(working)
		}
	}
	return want, nil
}

// heldInRoubles reports whether the account's first entry on the paper, the
// one that fixes its position currency, is in roubles.
func heldInRoubles(journal []operation.Operation, instrumentID uuid.UUID) bool {
	for _, o := range journal {
		if o.InstrumentID != nil && *o.InstrumentID == instrumentID {
			return o.Currency == "RUB"
		}
	}
	return false
}

// resultInstrument is the catalog row of the paper an event produces, or nil.
// A missing row is not an error: the registry records facts before (or without)
// the paper being catalogued.
func (m *Materializer) resultInstrument(ctx context.Context, e Event) (*uuid.UUID, error) {
	inst, err := m.papers.ByISIN(ctx, e.ResultISIN)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("corporateaction: look up the paper %s produces: %w", e.ID, err)
	}
	return &inst.ID, nil
}

// rowsFor is the rows one event asks one account for on one catalog row: a
// split, or a pair built by operation.BuildExchange or BuildSpinoff. This side
// adds only the registry's names and the pair's group.
func (m *Materializer) rowsFor(e Event, accountID, instrumentID uuid.UUID, result *uuid.UUID,
	held heldPosition, working []operation.Operation,
) ([]operation.Operation, error) {
	if e.Kind == KindSplit {
		return []operation.Operation{splitRow(e, accountID, instrumentID, held)}, nil
	}
	if result == nil {
		return nil, fmt.Errorf("corporateaction: %s names no paper to produce", e.Kind)
	}

	var out, in operation.Operation
	var err error
	switch e.Kind {
	case KindConversion:
		// The whole holding converts: the registrar exchanged every unit.
		out, in, err = operation.BuildExchange(working, operation.ExchangeParams{
			AccountID:        accountID,
			FromInstrumentID: instrumentID,
			ToInstrumentID:   *result,
			Quantity:         held.quantity,
			ToQuantity:       held.quantity.Mul(e.Ratio()),
			OccurredOn:       e.EffectiveOn,
			Source:           operation.SourceRegistry,
			Note:             eventNote(e),
		})
	case KindSpinOff:
		out, in, err = operation.BuildSpinoff(working, operation.SpinoffParams{
			AccountID:        accountID,
			FromInstrumentID: instrumentID,
			ToInstrumentID:   *result,
			RatioFrom:        decimal.NewFromInt(e.RatioFrom),
			RatioTo:          decimal.NewFromInt(e.RatioTo),
			BasisShare:       *e.BasisShare,
			OccurredOn:       e.EffectiveOn,
			Source:           operation.SourceRegistry,
			Note:             eventNote(e),
		})
	default:
		return nil, fmt.Errorf("corporateaction: no rule for materializing %s", e.Kind)
	}
	if err != nil {
		return nil, err
	}

	// One group for both legs, derived rather than random. Groups are not
	// compared by sameRow, so this does not prevent rewrites; it makes the
	// same event recognisable across runs in logs and the database.
	group := groupFor(e, accountID, instrumentID)
	outID := externalIDFor(e, accountID, instrumentID)
	inID := outID + inLegSuffix
	out.TransferGroupID, out.ExternalID = &group, &outID
	in.TransferGroupID, in.ExternalID = &group, &inID
	return []operation.Operation{out, in}, nil
}

// splitRow is the split row one event asks one account for. Its currency is
// read from the position, since the engine refuses a split in any other
// (portfolio.Type.mustMatchPositionCurrency).
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

// eventNote is what a conversion's or spin-off's rows say on the screen. In
// Russian because it is stored data shown verbatim; it names the ratio as
// published and where the fact came from.
func eventNote(e Event) string {
	var what string
	switch e.Kind {
	case KindConversion:
		what = fmt.Sprintf("Конвертация %d:%d", e.RatioFrom, e.RatioTo)
	case KindSpinOff:
		what = fmt.Sprintf("Выделение %d:%d", e.RatioFrom, e.RatioTo)
	default:
		what = fmt.Sprintf("%s %d:%d", e.Kind, e.RatioFrom, e.RatioTo)
	}
	return what + " — из реестра корпоративных действий (" + sourceName(e.Source) + ")"
}

// sourceName is where an event came from, as a journal row says it.
func sourceName(source string) string {
	switch source {
	case SourceMOEX:
		return "Московская биржа"
	case SourceYahoo:
		return "Yahoo Finance"
	case SourceKnown:
		return "замена на Московской бирже"
	default:
		return "внесено вручную"
	}
}

// splitNote is what a split row says on the screen, like eventNote.
func splitNote(e Event) string {
	return fmt.Sprintf("Дробление %d:%d — из реестра корпоративных действий (%s)", e.RatioFrom, e.RatioTo, sourceName(e.Source))
}

// externalIDFor names the row an event produces on one account's holding of
// one catalog row. The instrument is part of it because an old database can hold
// one ISIN under two catalog rows. Deterministic, so a recomputed row is
// recognised as the stored one.
func externalIDFor(e Event, accountID, instrumentID uuid.UUID) string {
	return fmt.Sprintf("%s:%s:%s", e.ID, accountID, instrumentID)
}

// inLegSuffix tells the arriving leg from the departing one. Both are named
// after the source instrument, which is how the next run knows which paper's
// event the arriving leg belongs to (see ownedByThisPaper).
const inLegSuffix = ":in"

// ownedByThisPaper reports whether a registry row was written for an event of a
// catalog row in ours, reading the instrument from the external id
// ("event:account:instrument[:in]").
func ownedByThisPaper(o operation.Operation, ours map[uuid.UUID]bool) bool {
	if o.ExternalID == nil {
		return false
	}
	parts := strings.Split(*o.ExternalID, ":")
	if len(parts) < 3 {
		return false
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return false
	}
	return ours[id]
}

// nsCorporateAction is the UUID v5 namespace of materialized pair groups.
// Changing it renames every group ever written.
var nsCorporateAction = uuid.MustParse("2b6a3d55-3a7f-5e64-9b0f-4f4b0c3a1d7e")

// groupFor is the transfer group of one materialized pair, derived from the
// same parts as the external id.
func groupFor(e Event, accountID, instrumentID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(nsCorporateAction, []byte(externalIDFor(e, accountID, instrumentID)))
}

// heldPosition is a holding at a moment: quantity and cost currency.
type heldPosition struct {
	quantity decimal.Decimal
	currency string
}

// IsPositive reports whether anything is held at all.
func (h heldPosition) IsPositive() bool { return h.quantity.IsPositive() }

// heldAtStartOf reports what the account held where a registry row dated day
// folds: everything before the day plus the registry's own rows of it
// (operation.FoldedBefore).
func heldAtStartOf(journal []operation.Operation, instrumentID uuid.UUID, day time.Time) (heldPosition, error) {
	positions, err := portfolio.Compute(operation.FoldedBefore(journal, day, operation.SourceRegistry))
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

// diff turns what the registry asks for and what it wrote last time into a
// delta. Rows match by external id: an unchanged row stays as it is, a changed
// one is rewritten and inherits the replaced row's created_at so its place in the
// day does not move (operation.checkInheritedStamps). Rows nothing asks for any
// more are removed. The T-Invest rebuild has its own diff with bookkeeping this
// one does not need.
func diff(want []operation.Operation, owned map[uuid.UUID]operation.Operation) (operation.ImportDelta, error) {
	byName := make(map[string]operation.Operation, len(owned))
	for _, o := range owned {
		if o.ExternalID == nil || *o.ExternalID == "" {
			// A nameless registry row: left in owned to be removed.
			continue
		}
		byName[*o.ExternalID] = o
	}

	var delta operation.ImportDelta
	kept := map[uuid.UUID]bool{}
	// An event is compared whole: the journal refuses removing one leg of a
	// group, so a pair whose arriving leg alone changed must be rewritten as
	// a pair.
	for _, unit := range unitsOf(want) {
		storedRows := make([]operation.Operation, 0, len(unit))
		matched := true
		for _, w := range unit {
			stored, ok := byName[*w.ExternalID]
			if !ok || !sameRow(w, stored) {
				matched = false
			}
			if ok {
				storedRows = append(storedRows, stored)
			}
		}
		if matched && len(storedRows) == len(unit) {
			for _, stored := range storedRows {
				kept[stored.ID] = true
			}
			continue
		}
		// Any difference rewrites the whole event; the new rows inherit the old
		// stamps one for one (see operation.ImportDelta).
		for _, stored := range storedRows {
			delta.Remove = append(delta.Remove, stored.ID)
		}
		for i, w := range unit {
			if i < len(storedRows) {
				w.CreatedAt = storedRows[i].CreatedAt
			}
			delta.Add = append(delta.Add, w)
		}
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

// unitsOf groups desired rows into events: a split alone, a pair's legs
// together, by transfer group or external id.
func unitsOf(want []operation.Operation) [][]operation.Operation {
	var units [][]operation.Operation
	at := map[string]int{}
	for _, w := range want {
		key := *w.ExternalID
		if w.TransferGroupID != nil {
			key = w.TransferGroupID.String()
		}
		i, seen := at[key]
		if !seen {
			at[key] = len(units)
			units = append(units, []operation.Operation{w})
			continue
		}
		units[i] = append(units[i], w)
	}
	return units
}

// sameRow reports whether the stored row already says what this run says.
func sameRow(want, stored operation.Operation) bool {
	if want.AccountID != stored.AccountID || want.Type != stored.Type ||
		want.Currency != stored.Currency || want.Note != stored.Note ||
		want.Source != stored.Source || want.AmountMinor != stored.AmountMinor {
		return false
	}
	if !want.OccurredOn.Equal(stored.OccurredOn) {
		return false
	}
	if want.InstrumentID == nil || stored.InstrumentID == nil || *want.InstrumentID != *stored.InstrumentID {
		return false
	}
	if !sameQuantity(want.Quantity, stored.Quantity) {
		return false
	}
	if !sameRatio(want.SplitRatio, stored.SplitRatio) {
		return false
	}
	// The breakdown is compared piece by piece: a purchase backdated under a
	// conversion changes the parcels while counts and money stay the same.
	return sameLots(want.TransferLots, stored.TransferLots)
}

func sameQuantity(want, stored *decimal.Decimal) bool {
	if want == nil || stored == nil {
		return want == nil && stored == nil
	}
	return want.Equal(*stored)
}

func sameRatio(want, stored *decimal.Decimal) bool {
	if want == nil || stored == nil {
		return want == nil && stored == nil
	}
	return want.Equal(*stored)
}

func sameLots(want, stored []operation.ReleasedLot) bool {
	if len(want) != len(stored) {
		return false
	}
	for i := range want {
		if want[i].From != stored[i].From || !want[i].Quantity.Equal(stored[i].Quantity) || want[i].CostMinor != stored[i].CostMinor {
			return false
		}
		switch {
		case want[i].AcquiredOn == nil && stored[i].AcquiredOn == nil:
		case want[i].AcquiredOn == nil || stored[i].AcquiredOn == nil:
			return false
		case !want[i].AcquiredOn.Equal(*stored[i].AcquiredOn):
			return false
		}
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
