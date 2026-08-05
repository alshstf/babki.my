package tinvest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
)

// The rebuild is what makes the mirror worth keeping: it turns the broker's
// operations into journal entries, and it can do it AGAIN — over a mirror that
// changed under it, or under a projection rule that changed above it — without
// asking the broker for anything.
//
// Both halves of that are needed, because both halves happen. The broker
// rewrites history after the fact (its own documentation says an operation's id
// is not to be relied on as a key, and old operations have had their
// identifiers rewritten), and the rules in projection.go are written against
// data this session has largely not seen live, so they will be corrected. Each
// is answered the same way: the mirror is the record of what the broker said,
// the projection is a pure function of it, and this file computes the
// difference between the journal as it is and the journal that function now
// asks for.
//
// WHAT IT IS NOT is an incremental writer. It never asks "what is new since
// last time" — that question cannot be answered against a source that rewrites
// its own past. It computes the whole desired journal from the whole mirror and
// diffs it, so an operation whose commission the broker corrected, an operation
// the broker withdrew, and a rule this program changed all reach the journal by
// the same one path.
//
// IT IS PER CONNECTION, NOT PER LINK, and that is a requirement rather than a
// convenience: a move of shares between two of the owner's own accounts is two
// mirror rows filed under two different links, and the only way to see that
// they are one event is to look at every link of the connection at once (see
// pairTransfers).
//
// PRECONDITION: the babki accounts these links name must be fed by this
// connection and nothing else. The difference is computed per (account,
// source), so an operation written into one of these accounts by ANOTHER
// connection looks to this rebuild like a journal row no mirror asks for, and
// it would be removed. Two links of the SAME connection on one account are
// fine: their rows are all in the same desired set.
//
// Nothing checks that precondition today, here or anywhere — Store.CreateLink
// files a link against any account of the space. It cannot be checked here,
// where the links of other connections are not visible, and it follows from the
// owner's own decision that an import goes into ACCOUNTS OF ITS OWN, so the
// place for it is the path that creates a link (task 11).

// nsTinvest is the UUID namespace this importer derives the identifiers it has
// to invent under (RFC 4122's name-based version 5). It is a constant and it
// must stay one: changing it renames every transfer group this importer has
// ever produced, and a rebuild would then remove and rewrite every paired
// transfer in the journal.
var nsTinvest = uuid.MustParse("55c16945-9170-4eff-a3c3-b8886b38f8fb")

// journalDelta is the write path this rebuild hands its difference to — one
// transaction for the whole difference, with the per-candidate refusals coming
// back rather than taking the rest down. A narrow local interface for the same
// reason marketdata declares its own (internal/marketdata/jobs.go): this
// package commits to the one method it calls. *operation.Service satisfies it.
type journalDelta interface {
	ApplyImportDelta(ctx context.Context, spaceID uuid.UUID, d operation.ImportDelta) (
		[]operation.Operation, []operation.ImportRefusal, error)
}

// journalReader is the journal side of the difference: what this importer has
// already written into one account. *operation.Store satisfies it.
type journalReader interface {
	ListBySource(ctx context.Context, spaceID, accountID uuid.UUID, source string) ([]operation.Operation, error)
}

// RebuildStats is what one rebuild changed.
//
//   - Added: journal entries written and KEPT. An entry written and withdrawn
//     again within the same rebuild (see apply, on the half event) is in neither
//     this nor Removed: it never survived the rebuild, and counting it in both
//     would describe work nobody asked for as a change to the journal.
//   - Removed: journal entries that were there before this rebuild and are not
//     there now.
//   - Unparsed: mirror rows of this connection that carry a reason once this
//     rebuild is done — the same set UnparsedByConnection lists, not merely the
//     rows this run newly marked.
type RebuildStats struct{ Added, Removed, Unparsed int }

// projection is ProjectRow's own signature. The Rebuilder holds one as a field
// so that a test can put another rule in its place and watch the journal follow
// — which is the property this whole file exists for, and one that cannot be
// demonstrated by calling the only rule there is.
type projection func(row MirrorRow, accountID uuid.UUID, resolved *Resolved) ([]operation.Operation, *UnparsedError)

// Rebuilder turns one connection's mirror into journal operations. Build one
// per sync run: the Resolver it carries caches the broker's instrument
// passports for the life of the run, and the run is what bounds that cache.
//
// NOT SAFE FOR CONCURRENT USE, for the reason Resolver is not. Two rebuilds of
// one connection must not overlap for a stronger reason than that: each reads
// the journal, decides a difference against it and then writes, so two of them
// would both compute their difference against the same journal and both apply
// it. Serializing runs per connection is the scheduler's job (task 10).
type Rebuilder struct {
	store    *Store
	resolver *Resolver
	ops      journalDelta
	reader   journalReader
	log      *slog.Logger
	project  projection
}

func NewRebuilder(store *Store, resolver *Resolver, ops journalDelta, reader journalReader, log *slog.Logger) *Rebuilder {
	if log == nil {
		log = slog.Default()
	}
	return &Rebuilder{store: store, resolver: resolver, ops: ops, reader: reader, log: log, project: ProjectRow}
}

// Rebuild brings the journal into agreement with the mirror of one connection.
//
// links must be every link of conn. A subset does not simply leave the missing
// accounts alone: a transfer already paired with one of them would be projected
// as a lone leg, and removing one leg of a stored pair is a thing the write path
// refuses outright — so such a rebuild fails rather than half-writing. (Shares
// moving to an account that is not linked AT ALL are a different case entirely,
// and a lone leg is the right answer there — see pairTransfers.)
//
// src is the broker, and it is used for ONE thing: the
// passport of an instrument neither this connection's map nor the shared
// catalog has seen before. A rebuild over a mirror whose instruments are all
// known makes no broker call at all.
//
// It is not one transaction. The journal's difference is applied in one (the
// write path's own), and the mirror's reasons are written in another, so a
// crash between them leaves a journal that is right and a reason that is stale
// — which the next rebuild corrects, because it computes everything from the
// mirror again and states every reason afresh.
func (r *Rebuilder) Rebuild(ctx context.Context, conn Connection, links []AccountLink, src passportSource) (RebuildStats, error) {
	for _, link := range links {
		if link.ConnectionID != conn.ID {
			return RebuildStats{}, fmt.Errorf("%w: link %s is under connection %s, not %s",
				ErrLinkNotInConnection, link.ID, link.ConnectionID, conn.ID)
		}
		if link.SpaceID != conn.SpaceID {
			return RebuildStats{}, fmt.Errorf("%w: link %s is in space %s and connection %s in space %s",
				ErrLinkOutsideSpace, link.ID, link.SpaceID, conn.ID, conn.SpaceID)
		}
	}

	p, err := r.projectAll(ctx, conn, links, src)
	if err != nil {
		return RebuildStats{}, err
	}
	sortDesired(p.want)
	pairTransfers(p.want)

	delta, err := r.difference(ctx, conn.SpaceID, accountsOf(links), p.want)
	if err != nil {
		return RebuildStats{}, err
	}

	added, err := r.apply(ctx, conn.SpaceID, delta, p)
	if err != nil {
		return RebuildStats{}, err
	}
	if err := r.writeReasons(ctx, p); err != nil {
		return RebuildStats{}, err
	}

	stats := RebuildStats{Added: added, Removed: len(delta.Remove), Unparsed: p.unparsed()}
	r.log.Info("tinvest: rebuilt the projection",
		"connection", conn.ID, "links", len(links), "mirror_rows", len(p.stored),
		"added", stats.Added, "removed", stats.Removed, "unparsed", stats.Unparsed)
	return stats, nil
}

// desired is one journal entry the mirror asks the journal to hold, with the
// little about its mirror row that the steps after the projection need.
type desired struct {
	op    operation.Operation
	rowID uuid.UUID
	// at is the broker's own instant, which orders the entries within a day
	// (see sortDesired). It is not the journal's date — that is op.OccurredOn,
	// the Moscow calendar day.
	at time.Time
	// leg is which entry of its row this is, 0-based, so that the two entries
	// of one row keep the order the projection built them in.
	leg int
	// description is the broker's own wording for the row, kept because a
	// transfer leg that finds its other half has to give back a note that is no
	// longer true (see pairTransfers).
	description string
}

// projected is everything one pass over the mirror produced.
type projected struct {
	want []desired
	// reasons is the verdict for the mirror rows this rebuild RULED ON: the
	// empty string for a row that became journal entries, a code for one that
	// could not. A row that is not in here was not ruled on and keeps whatever
	// it carries — see projectAll.
	reasons map[uuid.UUID]string
	// stored is every mirror row's reason as it stands in the database, for
	// every row read. It is what makes "write only what changed" possible, and
	// what Unparsed is counted over.
	stored map[uuid.UUID]string
	// rowOf maps a journal entry's external id back to the mirror row it came
	// from. The mapping is REMEMBERED rather than parsed back out of the name:
	// the name's shape is the projection's business, and a second reader of it
	// here would be a second implementation of one rule.
	rowOf map[string]uuid.UUID
}

// unparsed is how many of the connection's mirror rows carry a reason now.
func (p *projected) unparsed() int {
	n := 0
	for rowID, was := range p.stored {
		reason, ruled := p.reasons[rowID]
		if !ruled {
			reason = was
		}
		if reason != "" {
			n++
		}
	}
	return n
}

// projectAll runs the projection over every mirror row of every link.
//
// A ROW THAT DID NOT HAPPEN IS SKIPPED AND LEFT ALONE. An order the broker
// cancelled, or one it has stopped reporting, produces no journal entries —
// ProjectRow says so itself, and the skip here is the caller-side half its
// documentation asks for, which saves resolving an instrument for an operation
// that never took place. Such a row's REASON IS NOT TOUCHED either way. If it
// carries one, that reason is a true statement about the row — this program
// could not read it — and it stays on the owner's list with the row's own
// DisappearedAt beside it, which is what UnparsedByConnection deliberately
// keeps them for. If it carries none, there is nothing to say.
func (r *Rebuilder) projectAll(ctx context.Context, conn Connection, links []AccountLink, src passportSource) (*projected, error) {
	p := &projected{
		reasons: map[uuid.UUID]string{},
		stored:  map[uuid.UUID]string{},
		rowOf:   map[string]uuid.UUID{},
	}
	// One run's answers about instruments. The Resolver caches the broker's
	// passports; this caches the resolutions themselves, so a history that
	// names one paper a thousand times costs one lookup instead of a thousand.
	// It is local to the call rather than a field: a Rebuilder outliving a run
	// would otherwise answer from a catalog that has since changed.
	resolutions := map[InstrumentRef]Resolved{}

	for _, link := range links {
		rows, err := r.store.MirrorRowsByLink(ctx, link.ID)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			p.stored[row.ID] = row.UnparsedReason
			if row.State != stateExecuted || row.DisappearedAt != nil {
				continue
			}
			resolved, refusal, err := r.resolve(ctx, conn.ID, src, row, resolutions)
			if err != nil {
				return nil, err
			}
			var ops []operation.Operation
			if refusal == nil {
				ops, refusal = r.project(row, link.AccountID, resolved)
			}
			if refusal != nil {
				p.reasons[row.ID] = string(refusal.Reason)
				r.log.Debug("tinvest: a broker operation did not become journal entries",
					"mirror_row", row.ID, "op_type", row.OpType, "reason", refusal.Reason, "detail", refusal.Detail)
				continue
			}
			for i := range ops {
				if ops[i].ExternalID == nil || *ops[i].ExternalID == "" {
					return nil, fmt.Errorf("tinvest: the projection produced an entry with no external id for mirror row %s", row.ID)
				}
				p.rowOf[*ops[i].ExternalID] = row.ID
				p.want = append(p.want, desired{
					op: ops[i], rowID: row.ID, at: row.OccurredAt, leg: i, description: row.Description,
				})
			}
			// Every row that reaches here is one the projection READ: one that
			// became journal entries, or one it turns into nothing on purpose
			// (a BROKER_FEE, which is the same money as a trade's commission
			// field). Neither is a row this program failed to read, so whatever
			// reason either carried goes.
			p.reasons[row.ID] = ""
		}
	}
	return p, nil
}

// resolve finds the catalog instrument a mirror row's security means, or says
// why there is none.
//
// IT ASKS THE RESOLVER ONLY ABOUT KINDS OF ASSET THE RESOLVER ACCOUNTS FOR, and
// the reason is not thrift, though it saves a broker call per unknown paper.
// For everything else the PROJECTION has the truer word, and it is the
// projection's word the owner reads: a currency trade is refused as a currency
// trade — a thing this journal has a type for and lacks the data to build —
// rather than as "an asset kind this program does not account for", which is
// the only thing the resolver could say about it. Handing the projection no
// resolution and letting it name the fault is what keeps those two statements
// from collapsing into one (see ReasonCurrencyTrade, and instrumentRefusal,
// which reads this same map for the same question).
//
// A RESOLVER REFUSAL IS A REASON; ANYTHING ELSE IS FATAL. The three sentinels
// below are statements about the data, and they become the row's visible
// reason. A database or broker failure is not: recording it as "the security
// was not matched" would blame the operation for the network, and the mark
// would sit there until something happened to rebuild the row again.
func (r *Rebuilder) resolve(ctx context.Context, connID uuid.UUID, src passportSource, row MirrorRow,
	resolutions map[InstrumentRef]Resolved,
) (*Resolved, *UnparsedError, error) {
	if !namesSecurity(row) {
		return nil, nil, nil
	}
	if _, ok := brokerInstrumentTypes[row.InstrumentType]; !ok {
		return nil, nil, nil
	}
	ref := InstrumentRef{
		InstrumentUID: row.InstrumentUID, FIGI: row.FIGI,
		PositionUID: row.PositionUID, AssetUID: row.AssetUID,
	}
	if known, ok := resolutions[ref]; ok {
		return &known, nil, nil
	}
	resolved, err := r.resolver.Resolve(ctx, connID, src, ref)
	switch {
	case err == nil:
		resolutions[ref] = resolved
		return &resolved, nil, nil
	case errors.Is(err, ErrUnsupportedInstrumentType):
		return nil, &UnparsedError{Reason: ReasonUnsupportedType, Detail: err.Error()}, nil
	case errors.Is(err, ErrDifferentSecurity), errors.Is(err, ErrIncompletePassport):
		return nil, &UnparsedError{Reason: ReasonInstrumentUnresolved, Detail: err.Error()}, nil
	}
	return nil, nil, err
}

// sortDesired puts the entries in the order the journal will fold them.
//
// THE BROKER'S INSTANT IS THE ORDER, not the order the rows were first seen.
// Both are stable, and only one is right: a whole first import shares one
// first_seen_at, leaving the rows behind it ordered by a random uuid, so a sale
// could be offered to the engine before the purchase that covers it and be
// refused for a reason that is not true. The journal keeps a DAY, and the write
// path folds one day's operations in the order they are listed here (see
// operation.importCandidates), so this is the only place the time of day
// survives to say anything.
//
// The mirror row's id breaks a tie between two operations of the same instant,
// and the leg keeps the two entries of one row in the order the projection
// built them (income before the withdrawal that follows it; a trade before its
// commission). Both are properties of stored data, so the order is the same on
// every rebuild — which is what keeps a rebuild that changed nothing from
// renumbering the journal.
func sortDesired(want []desired) {
	sort.Slice(want, func(i, j int) bool {
		if !want[i].at.Equal(want[j].at) {
			return want[i].at.Before(want[j].at)
		}
		if c := bytes.Compare(want[i].rowID[:], want[j].rowID[:]); c != 0 {
			return c < 0
		}
		return want[i].leg < want[j].leg
	})
}

// transferPair is the description of one parcel, as far as recognizing the two
// halves of a move goes: one paper, one number of units, one day.
//
// THE DAY IS PART OF IT BECAUSE THE JOURNAL REQUIRES IT: a pair whose legs fall
// on two dates is refused by the write path outright (operation.pairedLegs), so
// legs a day apart cannot be one event however alike they are otherwise, and
// each of them stays alone. The broker timestamping a departure at 23:59 and
// its arrival after midnight is what that would look like.
//
// The quantity is a string because a decimal is not a comparable value: two
// decimals equal in value can differ in representation. String() is the
// canonical form — checked, not assumed: a 5 truncated to ten places and a
// parsed "5.0000000000" both print "5" — and the two legs are built by one and
// the same code (journalQuantity) besides.
type transferPair struct {
	instrument uuid.UUID
	quantity   string
	day        string
}

// pairTransfers finds the moves between two accounts of ONE connection and
// makes each of them a single event of the journal.
//
// TWO LEGS ARE ONE MOVE when one leaves and one arrives, on the same paper, the
// same number of units and the same day, on two DIFFERENT accounts. The broker
// does not say so: it reports a departure on one account and an arrival on the
// other, with nothing tying them together (the operation ids that might have
// are the ones its own documentation says not to rely on), and its type does not
// say it either — TRANS_BS_BS appears on both sides, and shares can equally
// leave under OUTPUT_SECURITIES and arrive under INPUT_SECURITIES. So the legs
// are matched on what they SAY, whatever type produced them.
//
// A LEG THAT FINDS NOBODY STAYS ALONE, and that is the ordinary case rather
// than a failure: shares that came from a broker this program knows nothing
// about, or left for one, have no second leg in existence anywhere.
//
// THE GROUP IS DERIVED FROM THE TWO LEGS' OWN NAMES and from nothing else, so a
// rebuild that changed nothing produces the same group and the journal is left
// alone. A group drawn at random would make every rebuild rewrite every
// transfer in the history.
//
// WHERE MORE THAN TWO LEGS COULD MATCH — the same paper, count and day moving
// twice — the pairing is by the order sortDesired put them in, which is the
// broker's own instant and then the mirror row's id. It is deterministic, which
// is what matters: two parcels alike in paper, count, day and pair of accounts
// are indistinguishable in the journal too, so no assignment of them is more
// true than another. The map below is walked in whatever order Go hands it out,
// and that changes nothing: two different parcels never compete for one leg, so
// each key is settled on its own.
//
// A PAIRED ARRIVAL GIVES BACK THE NOTE SAYING ITS COST IS UNKNOWN. The
// projection puts that mark on shares arriving from outside (see
// noteBasisUnknown), which is what an arrival looks like until this function
// finds its other half; once paired, the write path releases the parcel's basis
// from the source account's own history and the cost is known exactly. The note
// is rebuilt from the broker's description rather than edited, so nothing here
// depends on the shape of the mark.
func pairTransfers(want []desired) {
	type sides struct{ out, in []int }
	byParcel := map[transferPair]*sides{}
	for i := range want {
		op := want[i].op
		if op.Type != operation.TypeTransferOut && op.Type != operation.TypeTransferIn {
			continue
		}
		if op.InstrumentID == nil || op.Quantity == nil {
			continue
		}
		key := transferPair{
			instrument: *op.InstrumentID,
			quantity:   op.Quantity.String(),
			day:        op.OccurredOn.Format("2006-01-02"),
		}
		s := byParcel[key]
		if s == nil {
			s = &sides{}
			byParcel[key] = s
		}
		if op.Type == operation.TypeTransferOut {
			s.out = append(s.out, i)
		} else {
			s.in = append(s.in, i)
		}
	}

	for _, s := range byParcel {
		taken := make([]bool, len(s.in))
		for _, out := range s.out {
			for k, in := range s.in {
				if taken[k] || want[in].op.AccountID == want[out].op.AccountID {
					continue
				}
				taken[k] = true
				group := transferGroupID(*want[out].op.ExternalID, *want[in].op.ExternalID)
				outGroup, inGroup := group, group
				want[out].op.TransferGroupID = &outGroup
				want[in].op.TransferGroupID = &inGroup
				want[in].op.Note = want[in].description
				break
			}
		}
	}
}

// transferGroupID names the event two legs share, out of the two legs' own
// names, sorted so that either leg can be named first and the answer is the
// same.
//
// The separator is "|", which an external id cannot contain: it is a uuid and a
// small number joined by "/" (see withExternalIDs).
func transferGroupID(a, b string) uuid.UUID {
	pair := []string{a, b}
	sort.Strings(pair)
	return uuid.NewSHA1(nsTinvest, []byte(strings.Join(pair, "|")))
}

// accountsOf is the babki accounts these links feed, each once and in the order
// the links were given.
func accountsOf(links []AccountLink) []uuid.UUID {
	seen := make(map[uuid.UUID]bool, len(links))
	out := make([]uuid.UUID, 0, len(links))
	for _, link := range links {
		if seen[link.AccountID] {
			continue
		}
		seen[link.AccountID] = true
		out = append(out, link.AccountID)
	}
	return out
}

// difference is what has to change for the journal to say what the mirror says.
//
// IT IS BY EXTERNAL ID AND THEN BY VALUE. The id says which journal row a
// broker operation became; the values say whether that row still describes it.
// A mirror row holds the broker's LATEST word — a commission it corrected, a
// description it reworded — so a difference that stopped at "this id is already
// there" would leave the old number in the journal for good. A row whose values
// moved is removed and written again rather than updated in place: the write
// path takes a difference, not an edit, and a rewritten row is also how the
// engine's own replay gets to judge the new values.
//
// A TRANSFER IS DIFFED AS ONE EVENT, both legs together, because that is how
// the journal takes it and how it gives it back: it refuses to write one leg of
// a pair alone and refuses to remove one leg of a pair alone
// (operation.pairedLegs, operation.importRemovals). So if either leg of a pair
// has to move, both are removed and both are written again.
//
// THAT IS THE WHOLE MECHANISM, and no separate rule follows a stored group
// around, which is worth writing down because the absence looks like an
// omission. A stored leg is left in place only when the entry that names it
// matches it — and a matching entry carries the same transfer group, whose name
// is derived from the two legs' own names (see transferGroupID), so its sibling
// is the other member of the very unit being compared. A leg whose sibling has
// to move cannot therefore be a leg that matched. If that ever stops holding,
// the write path refuses the whole delta and nothing is written; it does not
// write half a pair.
func (r *Rebuilder) difference(ctx context.Context, spaceID uuid.UUID, accounts []uuid.UUID, want []desired) (operation.ImportDelta, error) {
	var stored []operation.Operation
	for _, accountID := range accounts {
		rows, err := r.reader.ListBySource(ctx, spaceID, accountID, Source)
		if err != nil {
			return operation.ImportDelta{}, fmt.Errorf("tinvest: read the imported journal of account %s: %w", accountID, err)
		}
		stored = append(stored, rows...)
	}

	byName := make(map[string]operation.Operation, len(stored))
	for _, o := range stored {
		// A row with no name is not one this importer's projection wrote — every
		// entry it hands over carries the id of the record it came from, and the
		// write path refuses one that does not. It is treated as a row nothing
		// asks for, which is what the loop over leftovers below does with it.
		if o.ExternalID == nil || *o.ExternalID == "" {
			continue
		}
		if _, taken := byName[*o.ExternalID]; !taken {
			byName[*o.ExternalID] = o
		}
	}

	var remove []uuid.UUID
	// dropped is what keeps one id out of the removal list twice — which the
	// write path reads as a difference computed against a journal this is not,
	// and refuses whole (operation.importRemovals counts what it found against
	// what it was asked for).
	dropped := map[uuid.UUID]bool{}
	drop := func(o operation.Operation) {
		if dropped[o.ID] {
			return
		}
		dropped[o.ID] = true
		remove = append(remove, o.ID)
	}

	var add []operation.Operation
	kept := map[uuid.UUID]bool{}
	for _, unit := range unitsOf(want) {
		unchanged := true
		for _, d := range unit {
			s, ok := byName[*d.op.ExternalID]
			if !ok || !sameJournalRow(d.op, s) {
				unchanged = false
				break
			}
		}
		if unchanged {
			for _, d := range unit {
				kept[byName[*d.op.ExternalID].ID] = true
			}
			continue
		}
		for _, d := range unit {
			if s, ok := byName[*d.op.ExternalID]; ok {
				drop(s)
			}
			add = append(add, d.op)
		}
	}
	for _, o := range stored {
		if kept[o.ID] || dropped[o.ID] {
			continue
		}
		drop(o)
	}

	return operation.ImportDelta{Add: add, Remove: remove}, nil
}

// unitsOf groups the desired entries into the events the journal accepts or
// refuses whole: the two legs of a transfer are one, everything else is one on
// its own. The order of the units, and of the entries inside them, is the order
// sortDesired settled.
func unitsOf(want []desired) [][]desired {
	out := make([][]desired, 0, len(want))
	at := map[uuid.UUID]int{}
	for _, d := range want {
		if d.op.TransferGroupID == nil {
			out = append(out, []desired{d})
			continue
		}
		if i, ok := at[*d.op.TransferGroupID]; ok {
			out[i] = append(out[i], d)
			continue
		}
		at[*d.op.TransferGroupID] = len(out)
		out = append(out, []desired{d})
	}
	return out
}

// sameJournalRow reports whether the journal row already says what the
// projection now says.
//
// EVERY COLUMN THE PROJECTION COULD SET IS COMPARED, including the ones no rule
// sets today (a settlement day, a split ratio): a field that stopped being
// compared would be a field the mirror could no longer correct, and silently.
// The external id is not among them because it is what the two rows were
// matched BY.
//
// THE ONE EXCLUSION IS A BASIS THE JOURNAL OWNS. The amount of a departing leg,
// and of an arriving leg that has a sibling, is not the projection's to state —
// operation.checkImportContract refuses one outright, because a FIFO basis is a
// property of the account's history rather than of the broker's message, and
// the write path fills it in from that history. So the zero handed over is
// compared against a figure the journal computed, and comparing them would
// report a difference on every rebuild for ever. The condition is
// checkImportContract's own, and it is read off the DESIRED row on purpose:
// the type and the group have already been compared equal by the time it is
// asked, so the two rows agree about which shape this is.
//
// TransferLots are not compared for the same reason and one more: they are the
// parcel the write path released, and the projection never has them.
func sameJournalRow(want, stored operation.Operation) bool {
	if want.AccountID != stored.AccountID ||
		want.Type != stored.Type ||
		want.Currency != stored.Currency ||
		want.Note != stored.Note ||
		want.Source != stored.Source ||
		want.FeeMinor != stored.FeeMinor {
		return false
	}
	if !want.OccurredOn.Equal(stored.OccurredOn) {
		return false
	}
	if !sameDay(want.SettledOn, stored.SettledOn) {
		return false
	}
	if !sameID(want.InstrumentID, stored.InstrumentID) || !sameID(want.TransferGroupID, stored.TransferGroupID) {
		return false
	}
	if !sameNumber(want.Quantity, stored.Quantity) ||
		!sameNumber(want.Price, stored.Price) ||
		!sameNumber(want.SplitRatio, stored.SplitRatio) {
		return false
	}
	if journalOwnsBasis(want) {
		return true
	}
	return want.AmountMinor == stored.AmountMinor
}

// journalOwnsBasis reports whether the amount of this entry is the write path's
// to compute rather than the projection's to state — the same condition
// operation.checkImportContract refuses a supplied amount on.
func journalOwnsBasis(op operation.Operation) bool {
	return op.Type == operation.TypeTransferOut ||
		(op.Type == operation.TypeTransferIn && op.TransferGroupID != nil)
}

func sameID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameDay(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// sameNumber compares by VALUE, not by representation: the column is
// NUMERIC(30,10) and gives back a number carrying its scale, so the 100 that
// went in comes back as 100.0000000000 and is the same number.
func sameNumber(a, b *decimal.Decimal) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// apply hands the difference to the journal and turns what it would not take
// into reasons the owner can read.
//
// AN EVENT IS WRITTEN WHOLE OR NOT AT ALL. The write path decides one candidate
// at a time, and the two entries of ONE mirror row — a dividend paid to a card
// and the same money leaving the account, a trade and the commission charged in
// another currency — are two candidates to it, so it can take one and refuse
// the other. Half an event in the journal is a lie: a fee for a trade that is
// not there, money leaving for a dividend that never arrived. Which half a
// journal will take is not knowable without offering it, so the entry that was
// taken is withdrawn afterwards.
//
// The cost of that is a row written and withdrawn again on every rebuild for as
// long as the refusal stands — a refused row is offered afresh each time, since
// the journal's answer is about the journal as it is and changes when the
// missing history arrives. It is accepted deliberately: only the two shapes
// that produce two entries can split at all (a dividend paid to a card, and a
// trade whose commission was charged in another currency), and only when the
// journal takes one of them and refuses the other. The alternative is the half
// event.
func (r *Rebuilder) apply(ctx context.Context, spaceID uuid.UUID, delta operation.ImportDelta, p *projected) (int, error) {
	if len(delta.Add) == 0 && len(delta.Remove) == 0 {
		return 0, nil
	}
	applied, refused, err := r.ops.ApplyImportDelta(ctx, spaceID, delta)
	if err != nil {
		r.log.Error("tinvest: the journal would not take this rebuild's difference",
			"space", spaceID, "add", len(delta.Add), "remove", len(delta.Remove), "err", err)
		return 0, fmt.Errorf("tinvest: apply the rebuilt projection: %w", err)
	}

	refusedRows := make(map[uuid.UUID]bool, len(refused))
	for _, ref := range refused {
		rowID, ok := p.rowOf[ref.ExternalID]
		if !ok {
			return 0, fmt.Errorf("tinvest: the journal refused %q, which this rebuild never offered: %v",
				ref.ExternalID, ref.Err)
		}
		refusedRows[rowID] = true
		p.reasons[rowID] = string(ReasonEngineRefused)
		r.log.Warn("tinvest: the journal refused an operation the projection built",
			"mirror_row", rowID, "external_id", ref.ExternalID, "err", ref.Err)
	}

	var orphans []uuid.UUID
	for _, o := range applied {
		if o.ExternalID != nil && refusedRows[p.rowOf[*o.ExternalID]] {
			orphans = append(orphans, o.ID)
		}
	}
	if len(orphans) > 0 {
		r.log.Warn("tinvest: withdrawing the entries of an operation the journal took only half of",
			"entries", len(orphans))
		if _, _, err := r.ops.ApplyImportDelta(ctx, spaceID, operation.ImportDelta{Remove: orphans}); err != nil {
			r.log.Error("tinvest: withdrawing half of a written event failed, the journal holds part of one",
				"space", spaceID, "entries", len(orphans), "err", err)
			return 0, fmt.Errorf("tinvest: withdraw a half-written event: %w", err)
		}
	}
	return len(applied) - len(orphans), nil
}

// writeReasons records the projection's verdicts on the mirror, and writes only
// the ones that changed.
//
// Only what changed, because the whole history is stated afresh on every
// rebuild and an unconditional write would rewrite every row of a decade-old
// account every hour to say what it already said. The comparison is against the
// value read at the start of this same rebuild, which is sound because the
// column has exactly one writer — this, through SetUnparsedReasons — and runs
// of one connection do not overlap.
func (r *Rebuilder) writeReasons(ctx context.Context, p *projected) error {
	changed := map[uuid.UUID]string{}
	for rowID, reason := range p.reasons {
		if p.stored[rowID] != reason {
			changed[rowID] = reason
		}
	}
	if len(changed) == 0 {
		return nil
	}
	if err := r.store.SetUnparsedReasons(ctx, changed); err != nil {
		r.log.Error("tinvest: recording why operations could not be read failed", "rows", len(changed), "err", err)
		return err
	}
	return nil
}
