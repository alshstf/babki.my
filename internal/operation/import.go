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
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/money"
	"babki.my/babki/internal/portfolio"
)

// ErrImportContract means the delta itself is wrong, as opposed to one broker
// operation in it being unrecordable. An unrecordable candidate is news about the
// broker's data: it comes back in refused and the rest still loads. A delta that
// names a record twice, or removes a missing or hand-entered row, is news about
// the caller: its whole difference is suspect, so none of it is written.
var ErrImportContract = errors.New("import delta contradicts what the journal expects of an importer")

// ImportDelta is one importer's difference against the journal: operations to
// record and rows to remove, applied together or not at all.
//
// Every operation in Add names a non-manual Source and the ExternalID of the
// record it was projected from; the unique index over (account, source, external
// id) keeps one broker record from becoming two rows.
//
// Transfer legs arrive paired by a caller-computed TransferGroupID and are taken
// or refused as one event. The caller does not supply the parcel: TransferLots
// and the basis are released here from the source account's journal, as the hand
// transfer does.
//
// A non-zero CreatedAt means only one thing: this row replaces one the delta
// removes and keeps its place, so a reworded broker record does not move in the
// FIFO tie-break and change a tax figure. It is checked against the removed rows
// (checkInheritedStamps). OccurredAt, the source's instant, orders rows within a
// date (see foldsBefore).
type ImportDelta struct {
	Add    []Operation
	Remove []uuid.UUID
}

// ImportRefusal is one candidate the journal would not take, named by its broker
// record and carrying the real reason: ErrValidation for an operation this
// program cannot record, ErrInconsistent for one the engine will not replay.
type ImportRefusal struct {
	ExternalID string
	Err        error
}

// externalID is the string a refusal is named by; the empty fallback is for
// messages about the delta itself.
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

// ApplyImportDelta records an importer's difference in one transaction across
// every account it touches: the removals, then every insertion.
//
// A candidate the journal will not take is refused and the rest still applies;
// a broken contract, removals that leave an account unable to replay, or a write
// failure take the whole call down.
//
// What is judged is the journal the delta would leave, never a halfway state:
// a correction is a removal plus an insertion of the same record, and the removal
// alone may not replay.
//
//  1. All candidates are offered together over the journal the removals leave.
//     If the engine takes it, every candidate passes and none is blamed.
//  2. Otherwise the removals must replay on their own, or the delta is refused
//     whole.
//  3. Otherwise candidates are offered one at a time over that ground, to find
//     which ones the journal cannot hold.
//
// Candidates are offered in fold order (date, source instant, then the caller's
// order), and created_at is stated on each row in that same order so the stored
// journal reads back as checked (see Store.ApplyDelta); a replacement keeps the
// removed row's stamp. applied comes back in that order, as stored.
//
// All of it runs under the journal lock of every touched account, the lock a hand
// entry takes: the registry and explanations write into imported accounts too
// (#186).
func (s *Service) ApplyImportDelta(ctx context.Context, spaceID uuid.UUID, d ImportDelta) (
	applied []Operation, refused []ImportRefusal, err error,
) {
	return s.ApplyImportDeltaWith(ctx, spaceID, d, nil)
}

// AfterImport writes what an importer keeps about a delta, in the delta's own
// transaction: it commits with the operations or not at all.
type AfterImport func(ctx context.Context, q db.Executor, applied []Operation) error

// ApplyImportDeltaWith is ApplyImportDelta with after run inside the same
// transaction once the delta is written (after may be nil).
func (s *Service) ApplyImportDeltaWith(ctx context.Context, spaceID uuid.UUID, d ImportDelta, after AfterImport) (
	applied []Operation, refused []ImportRefusal, err error,
) {
	if len(d.Add) == 0 && len(d.Remove) == 0 {
		return nil, nil, nil
	}

	candidates, err := importCandidates(d.Add)
	if err != nil {
		return nil, nil, err
	}
	accountIDs, err := s.deltaAccounts(ctx, spaceID, candidates, d.Remove)
	if err != nil {
		return nil, nil, err
	}
	err = s.store.WithAccountsLocked(ctx, spaceID, accountIDs, func(st *Store) error {
		var err error
		applied, refused, err = (&Service{store: st}).applyImportDeltaLocked(ctx, spaceID, d, candidates)
		if err != nil || after == nil {
			return err
		}
		return after(ctx, st.db, applied)
	})
	if err != nil {
		return nil, nil, err
	}
	return applied, refused, nil
}

// errDryRun rolls back a delta that was only being checked.
var errDryRun = errors.New("operation: dry run")

// CheckImportDelta is ApplyImportDelta that writes nothing: the same judgement
// under the same locks, in a transaction rolled back at the end. It is the import
// preview.
func (s *Service) CheckImportDelta(ctx context.Context, spaceID uuid.UUID, d ImportDelta) (
	accepted []Operation, refused []ImportRefusal, err error,
) {
	if len(d.Add) == 0 && len(d.Remove) == 0 {
		return nil, nil, nil
	}
	candidates, err := importCandidates(d.Add)
	if err != nil {
		return nil, nil, err
	}
	accountIDs, err := s.deltaAccounts(ctx, spaceID, candidates, d.Remove)
	if err != nil {
		return nil, nil, err
	}
	err = s.store.WithAccountsLocked(ctx, spaceID, accountIDs, func(st *Store) error {
		var err error
		accepted, refused, err = (&Service{store: st}).applyImportDeltaLocked(ctx, spaceID, d, candidates)
		if err != nil {
			return err
		}
		return errDryRun
	})
	if err != nil && !errors.Is(err, errDryRun) {
		return nil, nil, err
	}
	return accepted, refused, nil
}

// BuildAndApplyImportDelta is ApplyImportDelta for a writer that must see the
// journal to know what to write (the registry, which works from the holding): it
// locks one account, hands build the journal read under the lock, and applies
// build's delta. The delta may touch that account only.
func (s *Service) BuildAndApplyImportDelta(ctx context.Context, spaceID, accountID uuid.UUID,
	build func(journal []Operation) (ImportDelta, error),
) (delta ImportDelta, applied []Operation, refused []ImportRefusal, err error) {
	err = s.store.WithAccountsLocked(ctx, spaceID, []uuid.UUID{accountID}, func(st *Store) error {
		journal, err := st.ListForEngine(ctx, spaceID, accountID)
		if err != nil {
			return err
		}
		if delta, err = build(journal); err != nil {
			return err
		}
		if len(delta.Add) == 0 && len(delta.Remove) == 0 {
			return nil
		}
		candidates, err := importCandidates(delta.Add)
		if err != nil {
			return err
		}
		locked := &Service{store: st}
		touched, err := locked.deltaAccounts(ctx, spaceID, candidates, delta.Remove)
		if err != nil {
			return err
		}
		for _, id := range touched {
			if id != accountID {
				return fmt.Errorf("%w: a delta built under the lock of account %s names account %s",
					ErrImportContract, accountID, id)
			}
		}
		applied, refused, err = locked.applyImportDeltaLocked(ctx, spaceID, delta, candidates)
		return err
	})
	if err != nil {
		return ImportDelta{}, nil, nil, err
	}
	return delta, applied, refused, nil
}

// deltaAccounts names the accounts to lock. It runs outside the lock safely:
// a candidate names its own account and a stored row never changes its. A row
// gone meanwhile is caught by importRemovals under the lock.
func (s *Service) deltaAccounts(ctx context.Context, spaceID uuid.UUID, candidates []candidate, remove []uuid.UUID) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	for _, c := range candidates {
		for _, leg := range c.legs {
			ids = append(ids, leg.AccountID)
		}
	}
	if len(remove) > 0 {
		rows, err := s.store.ByIDs(ctx, spaceID, remove)
		if err != nil {
			return nil, err
		}
		for _, o := range rows {
			ids = append(ids, o.AccountID)
		}
	}
	return ids, nil
}

// applyImportDeltaLocked is ApplyImportDelta's body. s.store is bound to the
// transaction that holds the accounts' journal locks.
func (s *Service) applyImportDeltaLocked(ctx context.Context, spaceID uuid.UUID, d ImportDelta, candidates []candidate) (
	applied []Operation, refused []ImportRefusal, err error,
) {
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

	// kept is each touched account's journal as the removals leave it: what
	// candidates are judged against and stored rows are confirmed against.
	// before is the current journal of accounts that lose rows, kept only to
	// tell "the removals broke this" from "this was already broken".
	//
	// youngest counts survivors only: a row about to be deleted must not raise
	// the floor new stamps start from. An inherited stamp is counted, since
	// that row is coming back.
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

	// New rows are stamped one microsecond apart from a base after the
	// youngest surviving row, so they are distinct, read back in the checked
	// order, and fold after everything already on their date (as journalWith
	// places a candidate) even when a previous sync ran ahead of the clock.
	//
	// So a large delta can stamp slightly into the future, and a hand entry
	// made in that window on the same day can land beneath its rows. The lock
	// keeps the writes apart; it does not order their stamps.
	base := time.Now().UTC().Truncate(time.Microsecond)
	if !youngest.Before(base) {
		base = youngest.UTC().Truncate(time.Microsecond).Add(time.Microsecond)
	}

	accepted, refused, whole := offerTogether(candidates, kept, base)
	if !whole {
		// Step 2: the removals must replay on their own, or the first candidate
		// below would be blamed for damage that is not its own.
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
		// The same fold over the rows as stored, per touched account. Not
		// wrapped in ErrInconsistent: everything was accepted a moment ago, so a
		// failure here is this program's bug.
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

// offerTogether is step 1: every candidate written over the journal the
// removals leave, and the engine asked about the result. That is not weaker than
// one at a time: the engine stops at the first operation it cannot fold, so an
// accepted journal has every prefix accepted too.
//
// whole is false whenever the result is unusable, with no blame assigned:
// offerOneAtATime finds out whose fault it is. A candidate normalizeCandidate
// refuses is refused here directly, since that check reads no journal.
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

// offerOneAtATime is step 3: the same candidates in the same order, each against
// the journal the accepted ones leave, so the one a journal cannot hold is named
// and the rest still loads. Reached only when the whole was refused and the
// removals were shown blameless.
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

// stamped settles a candidate's created_at: the inherited stamp of the row it
// replaces, or the next in this delta's sequence. Both passes walk candidates in
// the same order, so a row gets the same stamp whichever pass checked it.
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

// refusalsFor names one event's refusal on every leg. failed is the leg the
// error is about; the other leg is told that cause, since a transfer is one
// event.
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

// checkInheritedStamps requires a supplied created_at to be the stamp of a row
// being removed from the same account. A free stamp would let a caller choose
// where in a day a row folds, and so which parcel a sale consumes. It cannot check
// that the stamp came from the very row being replaced; that matching is the
// importer's. Stamps compare by UnixNano, since a time.Time from the database
// carries a location.
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

// importCandidates groups the delta's operations into events and refuses
// everything that is the caller's mistake (see ErrImportContract).
func importCandidates(add []Operation) ([]candidate, error) {
	// The unique index's key. A record named twice means the difference is
	// wrong; refusing here names the record before anything is written.
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
	// Fold order: date, then source instant (rows without one last), then the
	// caller's order, which is the only order for events with no time.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].legs[0], out[j].legs[0]
		if !a.OccurredOn.Equal(b.OccurredOn) {
			return a.OccurredOn.Before(b.OccurredOn)
		}
		if before, decided := byInstant(a, b); decided {
			return before
		}
		return out[i].at < out[j].at
	})
	return out, nil
}

// checkImportContract is what an importer promises about every operation it
// hands over. A failure is the caller's doing, so it is fatal to the delta.
func checkImportContract(op Operation) error {
	if op.Source == "" || op.Source == "manual" {
		return fmt.Errorf("%w: an imported operation must name the source it came from, and %q is not one",
			ErrImportContract, op.Source)
	}
	if op.ExternalID == nil || *op.ExternalID == "" {
		return fmt.Errorf("%w: an imported operation must carry the id of the record it was projected from",
			ErrImportContract)
	}
	if len(op.TransferLots) > 0 && !carriesRegistryBreakdown(op) && !arrivesFromOutside(op) {
		return fmt.Errorf("%w: a transfer's FIFO breakdown is worked out from the journal here, not supplied",
			ErrImportContract)
	}
	if op.TransferGroupID != nil && !isTransferLeg(op.Type) && !isCorporatePairLeg(op.Type) {
		return fmt.Errorf("%w: only the legs of a transfer or of a corporate action may share a group, and %s is neither",
			ErrImportContract, op.Type)
	}
	if isCorporatePairLeg(op.Type) {
		if op.Source != SourceRegistry {
			return fmt.Errorf("%w: %s is the corporate-actions registry's to write, and %q is not it",
				ErrImportContract, op.Type, op.Source)
		}
		if op.TransferGroupID == nil {
			return fmt.Errorf("%w: %s is one leg of a pair and must name the group it shares with the other",
				ErrImportContract, op.Type)
		}
		if len(op.TransferLots) == 0 {
			return fmt.Errorf("%w: %s must arrive with the breakdown of the parcels behind it",
				ErrImportContract, op.Type)
		}
	}
	// The basis of a parcel taken from this journal is computed here; only a
	// lone arriving leg (shares from another broker) declares its own.
	// Corporate-action legs carry theirs: a conversion's size and a spin-off's
	// share come from the registry, and the engine checks the pieces on every
	// fold.
	if op.AmountMinor != 0 && (op.Type == TypeTransferOut || (op.Type == TypeTransferIn && op.TransferGroupID != nil)) {
		return fmt.Errorf("%w: the cost basis of %s is released from the source account's journal, not supplied",
			ErrImportContract, op.Type)
	}
	return nil
}

// isTransferLeg and isCorporatePairLeg name the two kinds of pair this path
// knows, so the five places that ask share one list.
func isTransferLeg(t Type) bool {
	return t == TypeTransferIn || t == TypeTransferOut
}

// isCorporatePairLeg reports a leg of a registry-materialized conversion or
// spin-off: a pair on one account, each leg with its own breakdown.
func isCorporatePairLeg(t Type) bool {
	switch t {
	case TypeExchangeOut, TypeExchangeIn, TypeSpinoffOut, TypeSpinoffIn:
		return true
	}
	return false
}

// carriesRegistryBreakdown reports whether op may arrive with its breakdown
// already worked out: only the registry's four leg types. Everything else has its
// parcel computed here, because a supplied parcel is a supplied cost basis, a tax
// figure; the registry's is its own record, and the engine checks it on every
// fold.
func carriesRegistryBreakdown(op Operation) bool {
	return op.Source == SourceRegistry && isCorporatePairLeg(op.Type)
}

// arrivesFromOutside reports a transfer_in with no sibling: shares from a broker
// this program does not hold. Its basis and breakdown are what the owner stated
// (see Service.StatePurchases); there is no source journal to compute them from.
func arrivesFromOutside(op Operation) bool {
	return op.Type == TypeTransferIn && op.TransferGroupID == nil
}

// pairedLegs checks that a group really is one event and returns its legs with
// the departing one first.
func pairedLegs(legs []Operation) ([]Operation, error) {
	group := *legs[0].TransferGroupID
	if len(legs) != 2 {
		return nil, fmt.Errorf("%w: transfer group %s has %d legs in this delta, and a pair is written whole or not at all",
			ErrImportContract, group, len(legs))
	}
	if isCorporatePairLeg(legs[0].Type) || isCorporatePairLeg(legs[1].Type) {
		return corporatePairLegs(group, legs)
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
	// One parcel: same instrument, day, quantity and currency. The basis is
	// computed here for both legs.
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

// corporatePairLegs is pairedLegs for a conversion or spin-off: one account and
// two papers, the mirror of a transfer. Quantities are not compared (a conversion
// restates the count; a spin-off's out leg has none), but the basis must match:
// both legs describe one sum of money.
func corporatePairLegs(group uuid.UUID, legs []Operation) ([]Operation, error) {
	out, in := legs[0], legs[1]
	if in.Type == TypeExchangeOut || in.Type == TypeSpinoffOut {
		out, in = in, out
	}
	switch {
	case out.Type == TypeExchangeOut && in.Type == TypeExchangeIn:
	case out.Type == TypeSpinoffOut && in.Type == TypeSpinoffIn:
	default:
		return nil, fmt.Errorf("%w: group %s is %s and %s, want the two legs of one conversion or one spin-off",
			ErrImportContract, group, legs[0].Type, legs[1].Type)
	}
	if out.AccountID != in.AccountID {
		return nil, fmt.Errorf("%w: group %s spans two accounts, and a corporate action happens on one",
			ErrImportContract, group)
	}
	if out.InstrumentID == nil || in.InstrumentID == nil || *out.InstrumentID == *in.InstrumentID {
		return nil, fmt.Errorf("%w: group %s names one paper on both legs, and a corporate action turns one into another",
			ErrImportContract, group)
	}
	if !out.OccurredOn.Equal(in.OccurredOn) {
		return nil, fmt.Errorf("%w: group %s leaves on %s and arrives on %s",
			ErrImportContract, group, out.OccurredOn.Format("2006-01-02"), in.OccurredOn.Format("2006-01-02"))
	}
	if out.Currency != in.Currency {
		return nil, fmt.Errorf("%w: group %s leaves in %s and arrives in %s",
			ErrImportContract, group, out.Currency, in.Currency)
	}
	if out.AmountMinor != in.AmountMinor {
		return nil, fmt.Errorf("%w: group %s gives up %d minor units and receives %d: one parcel of money, two figures",
			ErrImportContract, group, out.AmountMinor, in.AmountMinor)
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
		// An id outside the space or named twice: the difference was computed
		// against another journal.
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
	// A pair is removed whole: a lone arrival looks like shares from another
	// broker, so nothing could tell its source half is gone.
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

// prepareCandidate brings one event onto the journal's terms (stored scale, the
// departing leg's parcel released) and offers it to the engine, reporting which
// leg a refusal is about. offerTogether runs the first two stages and folds once;
// this runs all three per event.
func prepareCandidate(legs []Operation, pending map[uuid.UUID][]Operation) (int, error) {
	if failed, err := normalizeCandidate(legs); err != nil {
		return failed, err
	}
	if failed, err := releaseBasis(legs, pending); err != nil {
		return failed, err
	}
	return replayCandidate(legs, pending)
}

// normalizeCandidate settles what one event alone decides: quantities on the
// stored scale and the per-operation import contract. It reads no journal.
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

// releaseBasis fills in the parcel a departing leg moves and puts the same
// parcel on the arriving leg.
func releaseBasis(legs []Operation, pending map[uuid.UUID][]Operation) (int, error) {
	for i := range legs {
		// Corporate-action legs arrive with their parcels; see
		// carriesRegistryBreakdown.
		if legs[i].Type != TypeTransferOut {
			continue
		}
		// As in CreateTransfer: released at the leg's own place in the journal
		// (its instant can be mid-day), quantized before the basis is summed, and
		// the basis taken from those pieces.
		lots, err := portfolio.ReleasedLots(foldedAhead(pending[legs[i].AccountID], legs[i]),
			*legs[i].InstrumentID, *legs[i].Quantity)
		if err != nil {
			return i, fmt.Errorf("%w: %v", ErrInconsistent, err)
		}
		lots = quantizeLots(lots, *legs[i].Quantity)
		cost := portfolio.LotsCost(lots)
		if cost < 0 || cost > money.MaxAmountMinor {
			// A basis has no sign, so its range is 0..max, as CreateTransfer says.
			return i, fmt.Errorf("%w: the basis this transfer would move is %d, outside 0..%d",
				family.ErrValidation, cost, money.MaxAmountMinor)
		}
		legs[i].TransferLots, legs[i].AmountMinor = lots, cost
		// The arriving leg gets the same parcel; it is never released twice.
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
	// Each leg is checked against its own account's journal.
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

// validateImported is the import path's per-operation contract. It shares
// validateFields and validateByType with validate and differs in three ways:
//
//   - A transfer leg may stand alone: shares arriving from or leaving for a
//     broker this program does not know have no second leg.
//   - A split is never imported: no broker reports one. Only the registry writes
//     splits through this path.
//   - A tax may be positive: a broker's tax correction returns money (seven of
//     nine on the owner's account). Hand entry still refuses it as a likely typo.
//     A zero is refused on both paths.
//
// The delta-level contract is checkImportContract's, run before this.
func validateImported(o Operation) error {
	if err := validateFields(o); err != nil {
		return err
	}
	switch o.Type {
	case TypeSplit:
		// Only the registry writes splits; validateByType holds the rest of the
		// rule.
		if o.Source != SourceRegistry {
			return fmt.Errorf("%w: an import does not record splits — corporate actions do not arrive as operations",
				family.ErrValidation)
		}
		return validateByType(o)
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
		// A transfer's amount is a basis and cannot be negative. A leg whose
		// basis is computed here carries 0 until prepareCandidate fills it in.
		if o.AmountMinor < 0 {
			return fmt.Errorf("%w: %s amount_minor is a cost basis and must be >= 0", family.ErrValidation, o.Type)
		}
		return nil
	}
	return validateByType(o)
}

// mapImportWriteError maps a duplicate broker record to ErrImportContract, the
// same mistake as naming one twice in a delta.
func mapImportWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == "operations_dedup_idx" {
		return fmt.Errorf("%w: a record in this delta is already in the journal", ErrImportContract)
	}
	return mapWriteError(err)
}
