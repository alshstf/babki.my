package operation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/money"
	"babki.my/babki/internal/portfolio"
)

// ErrImportContract means the delta itself is wrong, as opposed to one broker
// operation in it being unrecordable.
//
// The difference is the whole reason this file has two kinds of failure. A
// candidate that cannot be applied is NEWS ABOUT THE BROKER'S DATA — an
// operation this program does not know how to record — and it must not stop the
// rest of a history from loading; it comes back in refused, with its real
// reason, to be shown to the owner. A delta that names one broker record twice,
// or asks to remove a row that is not there, or a row a person entered by hand,
// is instead NEWS ABOUT THE CALLER: it computed its difference wrongly, so the
// rest of that difference cannot be trusted either and none of it is written.
// Swallowing that as a refusal would leave the journal holding half of a
// mistaken decision, with nothing to tell anyone it happened.
var ErrImportContract = errors.New("import delta contradicts what the journal expects of an importer")

// ImportDelta is one importer's difference against the journal: the operations
// it wants recorded and the rows it wants gone, applied together or not at all.
//
// Every operation in Add must name a Source that is not a person's own entry
// and carry the ExternalID of the record it was projected from — that string is
// the only link back to the mirror, and the journal's unique index over
// (account, source, external id) is what keeps one broker record from becoming
// two journal rows.
//
// THE TWO LEGS OF A TRANSFER ARRIVE ALREADY PAIRED, sharing a TransferGroupID
// the caller computed. They are accepted or refused as one event: half a
// transfer would leave shares in an account nothing ever sent them from. What
// the caller must NOT supply is the parcel itself — TransferLots and the basis
// in AmountMinor are worked out here, from the source account's own journal, by
// the same release the manual transfer path uses. The caller cannot know them:
// a FIFO basis is a property of the history, not of the broker's message.
//
// AN OPERATION MAY CARRY A CreatedAt, AND EXACTLY ONE THING MAY BE MEANT BY IT:
// this row replaces one this same delta removes, and keeps its place. Within a
// date the journal folds by created_at, and that order is the order the FIFO
// queue breaks ties in — so a row restated by the broker and stamped afresh
// would move to the end of its own day and change which parcel a later sale
// consumes, which is a tax figure moving because a description was reworded.
// The stamp is therefore checked against the rows being removed (see
// checkInheritedStamps) rather than trusted: a caller free to invent one would
// be free to choose where in a day an operation folds. A zero CreatedAt is the
// ordinary case and is stamped here.
type ImportDelta struct {
	Add    []Operation
	Remove []uuid.UUID
}

// ImportRefusal is one candidate the journal would not take, named by the
// broker record behind it and carrying the REAL reason — an ErrValidation for
// an operation this program cannot record, an ErrInconsistent for one the
// engine will not replay. The caller shows it to the owner as an unparsed
// operation; a stand-in reason would be shown just as confidently.
type ImportRefusal struct {
	ExternalID string
	Err        error
}

// externalID is the string a refusal is named by. Every candidate has one by
// the time refusals can be produced (checkImportContract runs over the whole
// delta first), so the empty fallback is for messages about the delta itself.
func externalID(op Operation) string {
	if op.ExternalID == nil {
		return ""
	}
	return *op.ExternalID
}

// candidate is one event the delta proposes: a single operation, or the two
// legs of one transfer with the departing leg first.
type candidate struct {
	legs []Operation
	at   int // position in Add, so that same-day events keep the caller's order
}

// ApplyImportDelta records an importer's difference in ONE transaction: the
// removals, then every insertion, over as many of the space's accounts as the
// difference touches.
//
// WHAT IS REFUSED AND WHAT IS FATAL is the distinction ErrImportContract
// describes. A candidate the journal will not take comes back in refused and
// the rest of the delta still applies — a broker operation nobody has taught
// this program to record must not cost the owner the other four thousand. A
// delta that contradicts its own contract, or a removal that leaves an account
// unable to replay, or any failure of the write itself, takes the whole call
// down and writes nothing.
//
// WHAT IS JUDGED IS THE JOURNAL THIS DELTA WOULD LEAVE, never a state that
// only stands halfway through it, and that is the difference between an import
// that survives a broker correcting its own history and one that wedges on it.
// A correction is expressed as a removal and an insertion of the same record
// (see Store.ApplyDelta), so asking whether the removals alone replay asks what
// this account would be if a purchase were simply gone — and the answer, for
// any purchase a later sale rests on, is that it does not replay at all. That
// question is asked here only after the real one has failed:
//
//  1. the candidates are offered TOGETHER, over the journal the removals leave.
//     If the engine takes that whole journal, every candidate the per-operation
//     contract allows is accepted and nothing is isolated: no operation is at
//     fault, so none is blamed.
//  2. failing that, the removals have to replay on their own. If they do not,
//     the candidates did not put right whatever they broke, and the delta is
//     refused whole — blamed on the removals, or on a journal that would not
//     replay before this delta touched it, exactly as it was before this order
//     existed.
//  3. failing only step 1, the candidates are offered ONE AT A TIME over that
//     same ground, which is the only way to learn WHICH of them a journal
//     cannot hold: the engine answers about a whole journal, not about a row.
//
// Either way they are offered in the order the engine folds them — by date, and
// within a date in the order the caller listed them — so a sell listed before
// the buy that covers it is still judged against the position it actually had.
//
// The order they are checked in is then the order they are WRITTEN with:
// created_at is stated on every row rather than left to the statement's clock,
// because a delta's rows are written back to back in one batch and rows of the
// same date that happened to share an instant would leave every later read free
// to fold them in another order than the one that was checked (see
// Store.ApplyDelta). A row replacing one this delta removes keeps that row's
// stamp instead (see ImportDelta).
//
// applied comes back in that same order, as the database stored the rows.
func (s *Service) ApplyImportDelta(ctx context.Context, spaceID uuid.UUID, d ImportDelta) (
	applied []Operation, refused []ImportRefusal, err error,
) {
	if len(d.Add) == 0 && len(d.Remove) == 0 {
		return nil, nil, nil
	}

	candidates, err := importCandidates(d.Add)
	if err != nil {
		return nil, nil, err
	}
	removals, err := s.importRemovals(ctx, spaceID, d.Remove)
	if err != nil {
		return nil, nil, err
	}
	if err := checkInheritedStamps(candidates, removals); err != nil {
		return nil, nil, err
	}

	removeIDs := make(map[uuid.UUID]bool, len(removals))
	accounts := map[uuid.UUID]bool{}
	losing := map[uuid.UUID]bool{}
	for _, o := range removals {
		removeIDs[o.ID] = true
		accounts[o.AccountID] = true
		losing[o.AccountID] = true
	}
	for _, c := range candidates {
		for _, leg := range c.legs {
			accounts[leg.AccountID] = true
		}
	}

	// kept is every touched account's journal as the removals leave it — the
	// ground the candidates are judged against, and the ground the stored rows
	// are confirmed against inside the transaction. before is the same journal
	// as it stands NOW, held only for the accounts this delta takes rows from,
	// because the one thing it is read for is telling "the removals broke this"
	// apart from "this was already broken".
	//
	// youngest is tracked over the survivors: a row this delta is about to
	// delete will not be in any journal by the time the write commits, so its
	// created_at must not go on raising the floor new rows are numbered from
	// (see base below) — that would number them after a row that no longer
	// exists to share a date with anything. A stamp a replacement INHERITS is
	// counted in below, for the opposite half of the same reason: that row is
	// coming back, so nothing may be numbered on top of it.
	kept := make(map[uuid.UUID][]Operation, len(accounts))
	before := make(map[uuid.UUID][]Operation, len(losing))
	var youngest time.Time
	for accountID := range accounts {
		journal, err := s.store.ListForEngine(ctx, spaceID, accountID)
		if err != nil {
			return nil, nil, err
		}
		remaining := make([]Operation, 0, len(journal))
		for _, o := range journal {
			if removeIDs[o.ID] {
				continue
			}
			remaining = append(remaining, o)
			if o.CreatedAt.After(youngest) {
				youngest = o.CreatedAt
			}
		}
		if losing[accountID] {
			before[accountID] = journal
		}
		kept[accountID] = remaining
	}
	for _, c := range candidates {
		for _, leg := range c.legs {
			if leg.CreatedAt.After(youngest) {
				youngest = leg.CreatedAt
			}
		}
	}

	// One microsecond apart, from a base taken before any of them is checked:
	// distinct is what the read path needs (see Store.ApplyDelta), and
	// increasing in this order is what makes the order it reads back the order
	// that was checked here.
	// The base starts after the youngest row that SURVIVES this delta in the
	// touched journals, not merely at the current time, so that a candidate
	// always folds AFTER everything its date already holds — the same place
	// journalWith puts one.
	// The clock alone does not guarantee that: a delta spreads its rows a
	// microsecond apart, so a large one reaches milliseconds past the moment it
	// began, and a sync that follows it closely would otherwise start numbering
	// underneath rows the previous sync had just written. A journal ordered one
	// way when it is checked and another way when it is read is the fault this
	// whole path is built to avoid.
	//
	// IT CAN THEREFORE REACH A SHADE PAST THE PRESENT — one microsecond per row
	// stamped, so milliseconds for a delta of thousands. An operation a person
	// enters by hand into that window, on the same day and the same account, is
	// stamped by its own insert's clock and lands BENEATH the rows this delta
	// wrote, so it folds before them though it was entered after them. That much
	// is merely surprising: the hand-entry path stamps its candidate with
	// time.Now() when it checks it too (see journalWith), so check and read
	// agree. What is not merely surprising is the sliver where they disagree —
	// the check taken just under this delta's last stamp and the write's clock
	// just over it, which reads back in an order the check never saw. Both
	// windows are milliseconds wide, and the two paths do not share an account
	// in the shape this program is built for: an import writes into accounts of
	// its own (see the tinvest rebuild's precondition). That is why this is
	// written down rather than defended against.
	base := time.Now().UTC().Truncate(time.Microsecond)
	if !youngest.Before(base) {
		base = youngest.UTC().Truncate(time.Microsecond).Add(time.Microsecond)
	}

	accepted, refused, whole := offerTogether(candidates, kept, base)
	if !whole {
		// Step 2: the candidates could not carry the whole journal, so the
		// ground they stand on has to hold on its own. Until this runs, every
		// refusal below would name a cause that is not the cause: the first
		// candidate would be blamed for damage the removals did, or for damage
		// that was already there.
		for accountID := range accounts {
			if _, err := portfolio.Compute(kept[accountID]); err != nil {
				if journal, ok := before[accountID]; ok {
					if _, was := portfolio.Compute(journal); was == nil {
						return nil, nil, fmt.Errorf("%w: removing these operations leaves account %s unable to replay: %v",
							ErrInconsistent, accountID, err)
					}
				}
				return nil, nil, fmt.Errorf("%w: account %s does not replay even without this delta: %v",
					ErrInconsistent, accountID, err)
			}
		}
		accepted, refused = offerOneAtATime(candidates, kept, base)
	}

	if len(accepted) == 0 && len(removals) == 0 {
		return nil, refused, nil
	}

	stored, err := s.store.ApplyDelta(ctx, spaceID, accepted, d.Remove, func(stored []Operation) error {
		// The same fold again, over the rows the database actually kept: the
		// quantities are on the column's scale now, and a departing leg replays
		// the pieces the lot table gave back rather than the ones computed
		// above. One fold per touched account — including an account that only
		// lost rows, whose journal has to go on replaying without them.
		// Not wrapped in ErrInconsistent: everything here was checked and
		// accepted a moment ago, so reaching this is a bug in this program.
		byAccount := make(map[uuid.UUID][]Operation, len(accounts))
		for _, o := range stored {
			byAccount[o.AccountID] = append(byAccount[o.AccountID], o)
		}
		for accountID := range accounts {
			journal := append(slices.Clone(kept[accountID]), byAccount[accountID]...)
			sortJournal(journal)
			if _, err := portfolio.Compute(journal); err != nil {
				return fmt.Errorf("the delta as stored no longer replays on account %s: %v", accountID, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, mapImportWriteError(err)
	}
	return stored, refused, nil
}

// offerTogether is step 1 of ApplyImportDelta's order: every candidate written
// over the journal the removals leave, and the engine asked about the RESULT.
//
// ACCEPTING THEM WHOLESALE IS NOT A WEAKER TEST THAN OFFERING THEM ONE BY ONE.
// The engine folds a journal in order and stops at the first operation it
// cannot fold, so a journal it accepts is one every prefix of which it would
// have accepted too — a sale of shares nothing bought still fails when it is
// surrounded by four thousand others. What the two differ on is only which
// candidates are PRESENT while a given one is judged, and being judged among
// all of them is the truer question: that is the journal this delta leaves.
// (It is also the cheaper one — one fold per account rather than one per
// candidate — but that is a side effect and not the reason.)
//
// It reports whole == false the moment that result is unknowable or unusable —
// a parcel that cannot be released, an account whose final journal the engine
// refuses — and reports nothing about WHY. That is deliberate: a failure here
// says only that the difference is not good as a whole, which is not yet a
// statement about any one candidate, and inventing one would name the first
// operation the loop happened to reach. Whose fault it is, if it is anyone's,
// is what offerOneAtATime finds out afterwards.
//
// A candidate refused by normalizeCandidate is a different matter and is
// refused here as well as there. That check reads nothing but the operation
// itself — a quantity finer than the journal records, a split an import may not
// write — so its answer is the same whatever journal the row is offered
// against, and letting one such row take the whole difference down to the
// slower path would only reach the same refusal by a longer road.
func offerTogether(candidates []candidate, kept map[uuid.UUID][]Operation, base time.Time) (
	accepted []Operation, refused []ImportRefusal, whole bool,
) {
	pending := make(map[uuid.UUID][]Operation, len(kept))
	for accountID, journal := range kept {
		pending[accountID] = slices.Clone(journal)
	}
	seq := 0
	for _, c := range candidates {
		legs := stamped(c, base, &seq)
		if failed, err := normalizeCandidate(legs); err != nil {
			refused = append(refused, refusalsFor(legs, failed, err)...)
			continue
		}
		if _, err := releaseBasis(legs, pending); err != nil {
			return nil, nil, false
		}
		absorb(pending, legs)
		accepted = append(accepted, legs...)
	}
	for accountID := range pending {
		if _, err := portfolio.Compute(pending[accountID]); err != nil {
			return nil, nil, false
		}
	}
	return accepted, refused, true
}

// offerOneAtATime is step 3: the same candidates in the same order, each judged
// against the journal the ones already accepted leave, so that the operation a
// journal cannot hold is named on its own and the rest of a history still
// loads. It is reached only when the whole difference was refused together and
// the removals were shown to be blameless.
func offerOneAtATime(candidates []candidate, kept map[uuid.UUID][]Operation, base time.Time) (
	accepted []Operation, refused []ImportRefusal,
) {
	pending := make(map[uuid.UUID][]Operation, len(kept))
	for accountID, journal := range kept {
		pending[accountID] = slices.Clone(journal)
	}
	seq := 0
	for _, c := range candidates {
		legs := stamped(c, base, &seq)
		if failed, err := prepareCandidate(legs, pending); err != nil {
			refused = append(refused, refusalsFor(legs, failed, err)...)
			continue
		}
		absorb(pending, legs)
		accepted = append(accepted, legs...)
	}
	return accepted, refused
}

// stamped settles one candidate's created_at: the stamp a replacement inherited
// from the row it replaces, or the next of this delta's own sequence.
//
// seq advances only where a stamp is handed out, and the candidates are walked
// in one order, so the two passes above hand the same row the same stamp — the
// order that was checked has to be the order that is written whichever pass did
// the checking.
func stamped(c candidate, base time.Time, seq *int) []Operation {
	legs := slices.Clone(c.legs)
	for i := range legs {
		if !legs[i].CreatedAt.IsZero() {
			continue
		}
		legs[i].CreatedAt = base.Add(time.Duration(*seq) * time.Microsecond)
		*seq++
	}
	return legs
}

// absorb puts an accepted event into the journals it belongs to, each back in
// engine order, so the candidate after it is judged against a journal that
// already holds it.
func absorb(pending map[uuid.UUID][]Operation, legs []Operation) {
	for _, leg := range legs {
		pending[leg.AccountID] = append(pending[leg.AccountID], leg)
		sortJournal(pending[leg.AccountID])
	}
}

// refusalsFor names one event's refusal on every leg it has. failed is the leg
// the error is about; the other one is told the cause rather than a stand-in of
// its own, because this leg was fine and a transfer is one event.
func refusalsFor(legs []Operation, failed int, err error) []ImportRefusal {
	out := make([]ImportRefusal, 0, len(legs))
	for i, leg := range legs {
		reason := err
		if i != failed {
			reason = fmt.Errorf("the other leg of this transfer was refused: %w", err)
		}
		out = append(out, ImportRefusal{ExternalID: externalID(leg), Err: reason})
	}
	return out
}

// checkInheritedStamps holds a supplied created_at to a place the account's
// journal ALREADY HOLDS and this delta is about to vacate: it must be the stamp
// of a row being removed from that same account.
//
// Anything else is refused as the caller's own mistake rather than as a
// property of a broker record, because that is what it would be. A created_at
// decides where within a date an operation folds, a date's order is the order
// the FIFO queue breaks ties in, and the queue decides which parcel a sale
// consumes — so a caller free to state one is a caller free to set the realized
// profit of an account by choosing a number. A stamp already in that journal
// chooses nothing: putting a row back where one stood is the whole point.
//
// WHAT IT DOES NOT CHECK is that the stamp came from the very row this one
// replaces, and it cannot: which stored row a candidate replaces is the
// importer's own matching (by external id, see the tinvest rebuild's
// difference), and nothing here can see it. What it does keep out is the case
// that matters — a stamp conjured from nowhere, which is the only way to reach
// a position in a day that no row of this account occupies.
//
// The comparison is by UnixNano rather than by the time.Time itself: what the
// database gives back carries a location and no monotonic reading, and equality
// on time.Time is equality of those too. UnixNano is one number per instant
// over any date this program will meet, so two stamps of one moment cannot miss
// each other and two of different moments cannot collide.
func checkInheritedStamps(candidates []candidate, removals []Operation) error {
	type stamp struct {
		accountID uuid.UUID
		at        int64
	}
	inherited := make(map[stamp]bool, len(removals))
	for _, o := range removals {
		inherited[stamp{o.AccountID, o.CreatedAt.UnixNano()}] = true
	}
	for _, c := range candidates {
		for _, leg := range c.legs {
			if leg.CreatedAt.IsZero() {
				continue
			}
			if !inherited[stamp{leg.AccountID, leg.CreatedAt.UnixNano()}] {
				return fmt.Errorf(
					"%w: record %q states a created_at of %s, which no row this delta removes from account %s carries — a place in a day is inherited from the row being replaced, never chosen",
					ErrImportContract, externalID(leg), leg.CreatedAt.UTC().Format(time.RFC3339Nano), leg.AccountID)
			}
		}
	}
	return nil
}

// importCandidates groups the delta's operations into the events they describe
// and refuses everything about the delta that is the CALLER's mistake rather
// than a candidate's — see ErrImportContract.
func importCandidates(add []Operation) ([]candidate, error) {
	// The key the journal's unique index is built on. Naming the same broker
	// record twice in one delta means the difference was computed wrongly; the
	// database would refuse the second row anyway, and doing it here says so
	// before anything is written and names the record.
	type recordKey struct {
		accountID       uuid.UUID
		source, foreign string
	}
	seen := make(map[recordKey]bool, len(add))
	groups := make(map[uuid.UUID]int)
	out := make([]candidate, 0, len(add))
	for i, op := range add {
		if err := checkImportContract(op); err != nil {
			return nil, err
		}
		key := recordKey{op.AccountID, op.Source, *op.ExternalID}
		if seen[key] {
			return nil, fmt.Errorf("%w: record %q appears twice in one delta", ErrImportContract, *op.ExternalID)
		}
		seen[key] = true

		if op.TransferGroupID == nil {
			out = append(out, candidate{legs: []Operation{op}, at: i})
			continue
		}
		at, ok := groups[*op.TransferGroupID]
		if !ok {
			groups[*op.TransferGroupID] = len(out)
			out = append(out, candidate{legs: []Operation{op}, at: i})
			continue
		}
		out[at].legs = append(out[at].legs, op)
	}
	for i := range out {
		if out[i].legs[0].TransferGroupID == nil {
			continue
		}
		legs, err := pairedLegs(out[i].legs)
		if err != nil {
			return nil, err
		}
		out[i].legs = legs
	}
	// The order the engine folds them in. Stable, so that operations of one day
	// keep the order the caller listed them in — which is the only order there
	// is for events a broker reports with a date and no time.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].legs[0].OccurredOn.Equal(out[j].legs[0].OccurredOn) {
			return out[i].legs[0].OccurredOn.Before(out[j].legs[0].OccurredOn)
		}
		return out[i].at < out[j].at
	})
	return out, nil
}

// checkImportContract is what an importer promises about every operation it
// hands over, whatever the broker said. All of it is the caller's own doing —
// none of these can be a property of a broker record — so a failure is fatal to
// the delta rather than a refusal of the candidate.
func checkImportContract(op Operation) error {
	if op.Source == "" || op.Source == "manual" {
		return fmt.Errorf("%w: an imported operation must name the source it came from, and %q is not one",
			ErrImportContract, op.Source)
	}
	if op.ExternalID == nil || *op.ExternalID == "" {
		return fmt.Errorf("%w: an imported operation must carry the id of the record it was projected from",
			ErrImportContract)
	}
	if len(op.TransferLots) > 0 {
		return fmt.Errorf("%w: a transfer's FIFO breakdown is worked out from the journal here, not supplied",
			ErrImportContract)
	}
	if op.TransferGroupID != nil && op.Type != TypeTransferIn && op.Type != TypeTransferOut {
		return fmt.Errorf("%w: only a transfer's legs may share a transfer group, and %s is not one",
			ErrImportContract, op.Type)
	}
	// The basis of a parcel taken from this journal is this journal's to state.
	// A number supplied for it would be a guess, and one that is overwritten
	// here would be a guess nobody was told about; only a leg with no sibling —
	// shares arriving from a broker outside this program — declares its own.
	if op.AmountMinor != 0 && (op.Type == TypeTransferOut || (op.Type == TypeTransferIn && op.TransferGroupID != nil)) {
		return fmt.Errorf("%w: the cost basis of %s is released from the source account's journal, not supplied",
			ErrImportContract, op.Type)
	}
	return nil
}

// pairedLegs checks that a transfer group really is one transfer and returns
// its legs with the departing one first.
func pairedLegs(legs []Operation) ([]Operation, error) {
	group := *legs[0].TransferGroupID
	if len(legs) != 2 {
		return nil, fmt.Errorf("%w: transfer group %s has %d legs in this delta, and a pair is written whole or not at all",
			ErrImportContract, group, len(legs))
	}
	out, in := legs[0], legs[1]
	if out.Type == TypeTransferIn {
		out, in = in, out
	}
	if out.Type != TypeTransferOut || in.Type != TypeTransferIn {
		return nil, fmt.Errorf("%w: transfer group %s is %s and %s, want one leaving and one arriving",
			ErrImportContract, group, legs[0].Type, legs[1].Type)
	}
	if out.AccountID == in.AccountID {
		return nil, fmt.Errorf("%w: transfer group %s leaves and arrives at the same account",
			ErrImportContract, group)
	}
	// One parcel: the same instrument, the same day, the same quantity and the
	// same currency on both sides. The basis is deliberately not among them —
	// it is computed here for both legs (see checkImportContract).
	if out.InstrumentID == nil || in.InstrumentID == nil || *out.InstrumentID != *in.InstrumentID {
		return nil, fmt.Errorf("%w: transfer group %s moves one instrument out and another in", ErrImportContract, group)
	}
	if !out.OccurredOn.Equal(in.OccurredOn) {
		return nil, fmt.Errorf("%w: transfer group %s leaves on %s and arrives on %s",
			ErrImportContract, group, out.OccurredOn.Format("2006-01-02"), in.OccurredOn.Format("2006-01-02"))
	}
	if out.Quantity == nil || in.Quantity == nil || !out.Quantity.Equal(*in.Quantity) {
		return nil, fmt.Errorf("%w: transfer group %s does not move the same quantity on both legs", ErrImportContract, group)
	}
	if out.Currency != in.Currency {
		return nil, fmt.Errorf("%w: transfer group %s leaves in %s and arrives in %s",
			ErrImportContract, group, out.Currency, in.Currency)
	}
	return []Operation{out, in}, nil
}

// importRemovals loads the rows the delta asks to remove and refuses the two
// asks an importer may not make.
func (s *Service) importRemovals(ctx context.Context, spaceID uuid.UUID, ids []uuid.UUID) ([]Operation, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.store.ByIDs(ctx, spaceID, ids)
	if err != nil {
		return nil, err
	}
	if len(rows) != len(ids) {
		// Either an id that is not in this space, or the same id twice. Both
		// mean the difference was computed against a journal this is not.
		return nil, fmt.Errorf("%w: asked to remove %d operations, found %d in this space",
			ErrImportContract, len(ids), len(rows))
	}
	inDelta := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		inDelta[id] = true
	}
	groups := map[uuid.UUID]bool{}
	for _, o := range rows {
		if o.Source == "manual" {
			return nil, fmt.Errorf("%w: operation %s was entered by hand and is not an importer's to remove",
				ErrImportContract, o.ID)
		}
		if o.TransferGroupID != nil {
			groups[*o.TransferGroupID] = true
		}
	}
	// A pair is removed whole. The engine would not complain about a leg left
	// behind — a lone arrival is exactly what shares from another broker look
	// like — so nothing downstream could ever tell that the shares in the
	// destination account came from a transfer whose source half is gone.
	for group := range groups {
		siblings, err := s.store.ByTransferGroup(ctx, spaceID, group)
		if err != nil {
			return nil, err
		}
		for _, o := range siblings {
			if !inDelta[o.ID] {
				return nil, fmt.Errorf("%w: removing transfer group %s would leave its %s leg behind",
					ErrImportContract, group, o.Type)
			}
		}
	}
	return rows, nil
}

// prepareCandidate brings one event onto the journal's own terms — quantities
// on the stored scale, the parcel of a departing leg released from the source
// account's history — and offers it to the engine. It reports WHICH leg the
// refusal is about, so that the other leg of a pair is not told a reason that
// is not about it.
//
// The three stages are separate functions because the two passes over the
// candidates need different amounts of it: offerTogether runs the first two and
// asks the engine once, about the journal they all leave, while this runs all
// three per event. Splitting them is what keeps the two passes from being two
// statements of one rule.
func prepareCandidate(legs []Operation, pending map[uuid.UUID][]Operation) (int, error) {
	if failed, err := normalizeCandidate(legs); err != nil {
		return failed, err
	}
	if failed, err := releaseBasis(legs, pending); err != nil {
		return failed, err
	}
	return replayCandidate(legs, pending)
}

// normalizeCandidate is everything about one event that can be settled from the
// event alone: quantities brought onto the scale the journal stores, and the
// import path's per-operation contract. It reads no journal, so its answer is
// the same wherever the event is offered.
func normalizeCandidate(legs []Operation) (int, error) {
	for i := range legs {
		if err := normalizeForStorage(&legs[i]); err != nil {
			return i, err
		}
		if err := validateImported(legs[i]); err != nil {
			return i, err
		}
	}
	return -1, nil
}

// releaseBasis fills in the parcel a departing leg moves — the pieces of the
// source account's FIFO queue and the basis they carry — and puts the same
// parcel on the arriving leg.
func releaseBasis(legs []Operation, pending map[uuid.UUID][]Operation) (int, error) {
	for i := range legs {
		if legs[i].Type != TypeTransferOut {
			continue
		}
		// The same release the manual transfer path makes, for the same
		// reasons: against the journal as it stood on the transfer's own date
		// rather than at the end (a backdated transfer is replayed at its
		// chronological place, where the queue's front is different), quantized
		// before the basis is summed (a piece too small to store merges into
		// its neighbour, and the amount must be the sum of the pieces actually
		// written), and the basis taken from those very pieces rather than
		// computed a second way.
		lots, err := portfolio.ReleasedLots(journalUpTo(pending[legs[i].AccountID], legs[i].OccurredOn),
			*legs[i].InstrumentID, *legs[i].Quantity)
		if err != nil {
			return i, fmt.Errorf("%w: %v", ErrInconsistent, err)
		}
		lots = quantizeLots(lots, *legs[i].Quantity)
		cost := portfolio.LotsCost(lots)
		if cost < 0 || cost > money.MaxAmountMinor {
			// A basis is not a cash movement and has no sign to speak of, so the
			// bound it has to fall inside is 0..max and not ±max. The manual
			// transfer path says it that way (see Service.CreateTransfer), and
			// one rule described two ways is one rule a reader has to guess at.
			return i, fmt.Errorf("%w: the basis this transfer would move is %d, outside 0..%d",
				family.ErrValidation, cost, money.MaxAmountMinor)
		}
		legs[i].TransferLots, legs[i].AmountMinor = lots, cost
		// The arriving leg is the same parcel: same pieces, same basis. It is
		// never released a second time — one release, two legs, so they cannot
		// come to describe two different parcels.
		for j := range legs {
			if legs[j].Type == TypeTransferIn {
				legs[j].TransferLots, legs[j].AmountMinor = lots, cost
			}
		}
	}
	return -1, nil
}

// replayCandidate offers one event to the engine, account by account, over the
// journal each of them already holds.
func replayCandidate(legs []Operation, pending map[uuid.UUID][]Operation) (int, error) {
	// Each touched account folds its own journal with this event in it. A pair's
	// legs live on two accounts and each is checked against its own history.
	for i := range legs {
		if slices.ContainsFunc(legs[:i], func(o Operation) bool { return o.AccountID == legs[i].AccountID }) {
			continue // already folded with every leg on that account
		}
		journal := slices.Clone(pending[legs[i].AccountID])
		for _, leg := range legs {
			if leg.AccountID == legs[i].AccountID {
				journal = append(journal, leg)
			}
		}
		sortJournal(journal)
		if _, err := portfolio.Compute(journal); err != nil {
			return i, fmt.Errorf("%w: %v", ErrInconsistent, err)
		}
	}
	return -1, nil
}

// validateImported is the import path's per-operation contract, and it differs
// from the hand-entry path's (validate) on exactly two points. Everything else
// is the same rule, from the same helpers, so that the two cannot drift.
//
//   - A TRANSFER LEG MAY STAND ALONE. Shares that arrive from a broker this
//     program knows nothing about have no second leg in existence, and shares
//     that leave for one have none either; refusing them would make the whole
//     history unimportable. A person entering a transfer by hand still cannot
//     write one leg — both of their accounts are in here, so both legs are
//     knowable, and the pair endpoint is how they say so.
//
//   - A SPLIT IS NEVER IMPORTED. Corporate actions do not arrive as operations
//     at all, so an importer cannot know a split happened; a split row with a
//     broker's name on it would be this program's invention wearing the
//     broker's authority.
//
//   - A TAX MAY BE MONEY GIVEN BACK. A broker's tax CORRECTION arrives with a
//     positive amount, and it is an ordinary event: of the nine on the owner's
//     own account seven are positive. The hand-entry path keeps refusing one,
//     and should: there a positive tax is a sign somebody typed wrong, and the
//     refusal catches it at the moment it can still be fixed. Here the sign is
//     the broker's own statement about money that actually moved, and refusing
//     it left seven real credits out of the journal for as long as the account
//     existed. Nothing downstream needs teaching: a tax is folded into income
//     by its signed amount, so a refund restores exactly what the withholding
//     took (see portfolio.Compute), and the account's withheld total nets it
//     off (see taxWithheldFromAccount).
//
//     A ZERO stays refused on both paths and by the same rule below: money that
//     did not move is not a correction of anything.
//
// What it does NOT check is the delta's own contract — that an operation names
// a source and the record it came from, and does not arrive with a parcel it
// could not have computed. Those are checkImportContract's, run over the whole
// delta before any of this, because they are the caller's mistakes and are
// fatal rather than refusals (see ErrImportContract). By the time an operation
// reaches here they hold.
func validateImported(o Operation) error {
	if err := validateFields(o); err != nil {
		return err
	}
	switch o.Type {
	case TypeSplit:
		return fmt.Errorf("%w: an import does not record splits — corporate actions do not arrive as operations",
			family.ErrValidation)
	case TypeTax:
		if o.AmountMinor == 0 {
			return fmt.Errorf("%w: tax amount_minor must not be 0", family.ErrValidation)
		}
		return nil
	case TypeTransferIn, TypeTransferOut:
		if o.InstrumentID == nil {
			return fmt.Errorf("%w: %s requires an instrument", family.ErrValidation, o.Type)
		}
		if o.Quantity == nil || !o.Quantity.IsPositive() {
			return fmt.Errorf("%w: %s requires positive quantity", family.ErrValidation, o.Type)
		}
		// A transfer's amount is a cost basis, not a cash movement, so it has no
		// sign to speak of and cannot be below zero. A leg whose basis this
		// method computes carries 0 here and is given the released one after
		// (see prepareCandidate); a leg standing alone carries the one its
		// caller declared, which is all anyone will ever know about it.
		if o.AmountMinor < 0 {
			return fmt.Errorf("%w: %s amount_minor is a cost basis and must be >= 0", family.ErrValidation, o.Type)
		}
		return nil
	}
	return validateByType(o)
}

// mapImportWriteError translates the one constraint an import can trip that
// means something specific: a broker record already in the journal. It is the
// same mistake as naming one twice in a single delta — the difference was
// computed against a journal this is not — so it is the same error, whichever
// of the two ways it is found.
func mapImportWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == "operations_dedup_idx" {
		return fmt.Errorf("%w: a record in this delta is already in the journal", ErrImportContract)
	}
	return mapWriteError(err)
}
