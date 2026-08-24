package corporateaction

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
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

// rechecker is asked for a fresh comparison against the broker for the accounts
// a materialization changed. Declared here and narrow, and satisfied by
// *tinvest.Rechecker structurally, so this package does not import the importer
// — the dependency runs the other way, if at all: a registry knows nothing about
// brokers, and a broker's account is one of the places a registry's facts land.
//
// WHY IT IS NEEDED AT ALL: a verdict is a sentence about the journal at the
// moment it was struck, and this package changes journals underneath it. See
// tinvest.Rechecker for the live case that produced a wrong sentence.
//
// Nil is a legitimate value and means nothing is asked — an instance with no
// importer wired, and every test that is not about this.
type rechecker interface {
	QueueRecheckForAccounts(ctx context.Context, accountIDs []uuid.UUID) (int, error)
}

// catalog is how a conversion or a spin-off finds the paper it PRODUCES. The
// registry names it by ISIN, because the fact outlives any catalog row; the
// journal names it by instrument id, because a journal row points at a row of
// the catalog. This is the one lookup between the two.
//
// *instrument.Store satisfies it. Narrow, and declared here, for the same reason
// the two above are: what this package wants of the catalog is one question.
type catalog interface {
	ByISIN(ctx context.Context, isin string) (instrument.Instrument, error)
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
	papers  catalog
	recheck rechecker
	log     *slog.Logger
}

// NewMaterializer wires the registry to the journal. recheck may be nil (see
// the rechecker interface); papers may not — every conversion and spin-off needs
// it to find the paper it produces.
func NewMaterializer(store *Store, journal journalReader, ops journalWriter,
	papers catalog, recheck rechecker, log *slog.Logger,
) *Materializer {
	if log == nil {
		log = slog.Default()
	}
	return &Materializer{store: store, journal: journal, ops: ops, papers: papers, recheck: recheck, log: log}
}

// Stats is what one run did, for the log line and for the tests to read.
//
// Accounts are the accounts whose journals actually CHANGED — not the accounts
// looked at. That distinction is the whole value of the field: a sweep walks
// every holder of every paper in the registry and almost always writes nothing,
// and asking for a fresh broker comparison of all of them would turn a no-op
// sweep into a broker read per connection, every day, for ever.
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

	// The rows this materialization owns on this account. They are recomputed
	// from scratch below, so they are held out of the journal the recomputation
	// folds — otherwise the quantity a split is decided on would already have
	// that split in it.
	//
	// OWNERSHIP IS READ OFF THE ROW'S NAME, NOT OFF ITS INSTRUMENT COLUMN, and
	// that is what makes a pair expressible at all. A conversion writes its
	// arriving leg onto the paper it PRODUCES — the T shares, not the receipts —
	// so a rule keyed on "is this instrument one of ours" would leave that leg
	// unowned by the run that wrote it (never removed when the event changes) and
	// owned by the run for the produced paper (removed as unwanted the moment it
	// looked). Both readings are wrong and they are wrong in opposite directions.
	// The external id names the SOURCE instrument on both legs (see
	// externalIDFor), so a row says for itself which paper's event it belongs to,
	// and a deleted event's rows are still collected — the name outlives the
	// event.
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
	return Stats{
		Added:    len(delta.Add) - len(refused),
		Removed:  len(delta.Remove),
		Refused:  len(refused),
		Accounts: []uuid.UUID{accountID},
	}, nil
}

// RequestRecheck asks for a fresh comparison against the broker for the
// accounts a run changed, and reports how many were queued.
//
// IT IS THE CALLER'S CALL AND NOT AN AUTOMATIC TAIL OF EVERY RUN, because the
// three callers want different things from it. The API handler wants it before
// it answers, so the owner who has just recorded a split does not read a stale
// verdict on the very next screen. The daily sweep wants it too, for the rows a
// trigger missed. The exchange job wants it for the splits it learns. Nothing
// wants it twice, and a materialization that wrote nothing wants it not at all
// — which is what Stats.Accounts being empty then means.
//
// A FAILURE IS LOGGED AND SWALLOWED. Everything this reports on has already
// been written; the worst a failure costs is a verdict that stays stale until
// the hourly run, which is where the program stood before any of this existed.
// Turning it into the caller's error would make a successful write look like a
// failed one.
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

// desired is the set of journal rows the registry asks this account to hold.
//
// Events are applied in date order and the working journal grows as it goes,
// because each one acts on the holding the ones before it left: a paper that
// split ten for one in 2021 and two for one in 2024 is held in the 2021 answer
// when the 2024 event asks whether anything is held at all.
func (m *Materializer) desired(ctx context.Context, base []operation.Operation, accountID uuid.UUID,
	instrumentIDs []uuid.UUID, events []Event,
) ([]operation.Operation, error) {
	var want []operation.Operation
	working := base
	for _, e := range events {
		if !e.Kind.Materialized() {
			continue
		}
		// The paper a conversion or a spin-off produces, resolved once per event
		// rather than once per holding. An event whose result the catalog has no
		// row for produces NOTHING and says so where a person can read it (see
		// Store.NotCountedReason); here it is simply skipped, because a journal
		// row cannot point at a paper that is not in the catalog.
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
			if e.Kind == KindSplit && hasForeignSplit(working, instrumentID, e.EffectiveOn) {
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
			rows, err := m.rowsFor(e, accountID, instrumentID, result, held, working)
			if err != nil {
				// The event cannot be expressed against THIS account's journal —
				// a holding too small to leave a unit behind after the ratio, a
				// share of the basis that rounds to nothing. It is news about
				// this account and this event, not about the registry, so the
				// other accounts go on being brought into line.
				m.log.Warn("corporateaction: this account's holding cannot take the event, leaving it alone",
					"account", accountID, "instrument", instrumentID, "event", e.ID, "err", err)
				continue
			}
			want = append(want, rows...)
			working = append(working, rows...)
		}
	}
	return want, nil
}

// resultInstrument is the catalog row of the paper an event produces, or nil
// when the catalog has none.
//
// A MISSING ROW IS NOT AN ERROR. The registry records what happened to a paper
// whether or not anybody here holds the result — the exchange job writes splits
// of papers nobody in this instance has ever traded — and a conversion recorded
// before its new paper is catalogued is exactly the order things happen in when
// somebody enters a fact they have just learned. What it is instead is a visible
// answer on the screen, so nobody is left wondering why a recorded event moved
// nothing.
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

// rowsFor is the journal rows one event asks one account for on one catalog row
// of the paper: a single split, or the two legs of a conversion or a spin-off.
//
// THE PAIRS ARE BUILT BY THE OPERATION PACKAGE'S OWN BUILDERS and not restated
// here (see operation.BuildExchange and operation.BuildSpinoff). What is added
// on this side is only what makes them the REGISTRY's rows: the names that let
// the next run recognise them, and the group that keeps the two legs one event.
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
		// THE WHOLE HOLDING CONVERTS. A conversion is not a trade somebody sizes
		// — the registrar exchanged every unit anybody held, and an account that
		// kept some of the old paper back is a state that never existed. So the
		// count is the holding at the start of the day and the arriving count is
		// that holding through the registry's ratio.
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

	// One group for the two legs, DERIVED from the event and the holding rather
	// than drawn fresh: the next run recomputes these rows and must arrive at the
	// same group, or every run would remove a pair and write an identical one
	// under a new group for ever.
	group := groupFor(e, accountID, instrumentID)
	outID := externalIDFor(e, accountID, instrumentID)
	inID := outID + inLegSuffix
	out.TransferGroupID, out.ExternalID = &group, &outID
	in.TransferGroupID, in.ExternalID = &group, &inID
	return []operation.Operation{out, in}, nil
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
// eventNote is what a conversion's or a spin-off's rows say about themselves,
// in the same shape and for the same reasons as splitNote below.
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
	if e.Source == SourceMOEX {
		return what + " — из реестра корпоративных действий (Московская биржа)"
	}
	return what + " — из реестра корпоративных действий (внесено вручную)"
}

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

// inLegSuffix distinguishes the arriving leg of a pair from the departing one.
//
// BOTH LEGS ARE NAMED AFTER THE SOURCE INSTRUMENT, and the suffix is what keeps
// them apart under the journal's unique index over (account, source, external
// id). Naming the arriving leg after the paper it lands on would have read more
// naturally and would have broken ownership: a row's name is how the next run
// decides whose event it belongs to (see ownedByThisPaper), and the arriving leg
// belongs to the event of the paper it CAME FROM.
const inLegSuffix = ":in"

// ownedByThisPaper reports whether a registry row was written for an event of
// one of the catalog rows named in ours.
//
// It reads the row's external id rather than its instrument column, for the
// reason forAccount states: the arriving leg of a pair sits on a paper that is
// not this event's own. The id is "event:account:instrument" with an optional
// ":in", so the instrument is the third field either way.
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

// nsCorporateAction is the UUID namespace the transfer groups of materialized
// pairs are derived under (RFC 4122's name-based version 5). It is a constant
// and must stay one: changing it renames every group this package has ever
// written, and the next run would then remove every pair and write it again
// under new names.
var nsCorporateAction = uuid.MustParse("2b6a3d55-3a7f-5e64-9b0f-4f4b0c3a1d7e")

// groupFor is the transfer group the two legs of one materialized pair share.
// Derived from the same three things the external id is, so that recomputing the
// pair arrives at the same group rather than at a fresh one.
func groupFor(e Event, accountID, instrumentID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(nsCorporateAction, []byte(externalIDFor(e, accountID, instrumentID)))
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
	// AN EVENT IS COMPARED WHOLE, not leg by leg. A pair's two rows are one fact,
	// and the journal refuses a delta that removes one leg of a group and leaves
	// the other (see operation.importRemovals) — so a conversion whose arriving
	// count changed while its departing leg happened to stay identical would
	// otherwise produce exactly that refusal, and the run would fail rather than
	// correct itself. Grouping by the departing leg's name is what makes "the
	// same event as before" a single question with a single answer.
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
		// Any difference at all rewrites the whole event. The rows that come back
		// inherit the stamps of the rows they replace, one for one in the order
		// both were built in, so a corrected ratio does not also move where the
		// event folds within its day (see operation.ImportDelta).
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

// unitsOf groups the desired rows into the events they describe: a split on its
// own, a pair's two legs together, in the order they were built.
//
// The grouping is by transfer group where there is one and by external id
// otherwise, so it does not depend on the order rows happen to arrive in.
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

// sameRow reports whether the stored row already says what this run says, for
// any of the kinds this package writes.
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
	// THE BREAKDOWN IS COMPARED PIECE BY PIECE, and it is the field that actually
	// changes when the journal underneath moves: a purchase backdated under an
	// existing conversion leaves the counts and the money identical while the
	// parcels behind them are different ones. Without this the run would report
	// nothing to do and the pair would go on naming lots that no longer describe
	// the position.
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
		if !want[i].Quantity.Equal(stored[i].Quantity) || want[i].CostMinor != stored[i].CostMinor {
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
