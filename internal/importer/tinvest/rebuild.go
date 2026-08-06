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
//   - Added: journal entries written and KEPT.
//   - Removed: journal entries that were there before this rebuild and are not
//     there now — including a half event this rebuild took back out (see
//     apply), which was there before and is not now like any other removal.
//   - Withdrawn: journal entries this rebuild wrote and took back again within
//     itself, because the other half of their event was refused. They are in
//     neither Added nor Removed — the journal is where it was, so counting them
//     as a change would be untrue — but they are not nothing either: they are
//     work this run did and will do again on every run while the refusal
//     stands, and a summary of zeroes over an hour of that would be a summary
//     that hides it.
//   - Unparsed: mirror rows of this connection that carry a reason once this
//     rebuild is done — the same set UnparsedByConnection lists, not merely the
//     rows this run newly marked.
type RebuildStats struct{ Added, Removed, Withdrawn, Unparsed int }

// projection is ProjectRow's own signature. The Rebuilder holds one as a field
// so that a test can put another rule in its place and watch the journal follow
// — which is the property this whole file exists for, and one that cannot be
// demonstrated by calling the only rule there is.
type projection func(row MirrorRow, accountID uuid.UUID, resolved *Resolved) ([]operation.Operation, Deferred, *UnparsedError)

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
// write path's own), and the mirror's verdicts are written in another, so a
// crash between them leaves a journal that is right and a verdict that is stale
// — which the next rebuild corrects, because it computes everything from the
// mirror again and states every verdict afresh.
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
	// After the sort and before anything reads the entries: the number a
	// redemption is waiting for is the position built by the entries in front
	// of it, and that is only a number once they are in order.
	if err := r.closeRedemptions(p); err != nil {
		return RebuildStats{}, err
	}
	pairTransfers(p.want)

	delta, keptByRow, err := r.difference(ctx, conn.SpaceID, accountsOf(links), p.want)
	if err != nil {
		return RebuildStats{}, err
	}

	w, err := r.apply(ctx, conn.SpaceID, delta, keptByRow, p)
	if err != nil {
		return RebuildStats{}, err
	}
	if err := r.writeVerdicts(ctx, p); err != nil {
		return RebuildStats{}, err
	}

	stats := RebuildStats{
		Added:     w.added,
		Removed:   len(delta.Remove) + w.retracted,
		Withdrawn: w.withdrawn,
		Unparsed:  p.unparsed(),
	}
	r.log.Info("tinvest: rebuilt the projection",
		"connection", conn.ID, "links", len(links), "mirror_rows", len(p.stored),
		"added", stats.Added, "removed", stats.Removed, "withdrawn", stats.Withdrawn, "unparsed", stats.Unparsed)
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
	// pairable is whether the broker's own operation type for this row says the
	// move is between two accounts of this owner — the only kind of leg that
	// may be joined to another (see pairTransfers). It is read off the row
	// rather than off the journal entry because the journal entry does not know:
	// transfer_in is what shares arriving from a stranger's depositary and
	// shares arriving from the owner's other account both become.
	pairable bool
	// deferred is what this entry is still missing and the journal owes it (see
	// Deferred). It is carried per entry rather than per row because a row's
	// entries are not one thing: a redemption's sale waits for a count, and the
	// commission beside it does not.
	deferred Deferred
}

// projected is everything one pass over the mirror produced.
type projected struct {
	want []desired
	// verdicts is what this rebuild decided about the mirror rows it RULED ON:
	// the zero verdict for a row that became journal entries, a code — and,
	// where the refuser had more to say than its code, that too — for one that
	// could not. A row that is not in here was not ruled on and keeps whatever
	// it carries, detail included, because that pair is still a true statement
	// about it — see projectAll.
	verdicts map[uuid.UUID]UnparsedVerdict
	// stored is every mirror row's verdict as it stands in the database, for
	// every row read. It is what makes "write only what changed" possible, and
	// its Reason is what Unparsed is counted over.
	stored map[uuid.UUID]UnparsedVerdict
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
		verdict, ruled := p.verdicts[rowID]
		if !ruled {
			verdict = was
		}
		if verdict.Reason != "" {
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
		verdicts: map[uuid.UUID]UnparsedVerdict{},
		stored:   map[uuid.UUID]UnparsedVerdict{},
		rowOf:    map[string]uuid.UUID{},
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
			p.stored[row.ID] = UnparsedVerdict{Reason: row.UnparsedReason, Detail: row.UnparsedDetail}
			if row.State != stateExecuted || row.DisappearedAt != nil {
				continue
			}
			resolved, refusal, err := r.resolve(ctx, conn.ID, src, row, resolutions)
			if err != nil {
				return nil, err
			}
			var (
				ops      []operation.Operation
				deferred Deferred
			)
			if refusal == nil {
				ops, deferred, refusal = r.project(row, link.AccountID, resolved)
			}
			if refusal != nil {
				// The detail goes to the mirror as well as to the log. It is the
				// difference between "the engine refused this" — which 134 of the
				// owner's rows said, and which no one could act on — and the
				// engine's own sentence about the one row in front of the reader.
				// A log line answers that only for whoever still has the log.
				p.verdicts[row.ID] = UnparsedVerdict{Reason: string(refusal.Reason), Detail: refusal.Detail}
				r.log.Debug("tinvest: a broker operation did not become journal entries",
					"mirror_row", row.ID, "op_type", row.OpType, "reason", refusal.Reason, "detail", refusal.Detail)
				continue
			}
			for i := range ops {
				if ops[i].ExternalID == nil || *ops[i].ExternalID == "" {
					return nil, fmt.Errorf("tinvest: the projection produced an entry with no external id for mirror row %s", row.ID)
				}
				p.rowOf[*ops[i].ExternalID] = row.ID
				// A deferral is about the FIRST entry — the projection's own
				// convention, stated at Deferred and not re-derived here.
				owed := DeferredNothing
				if i == 0 {
					owed = deferred
				}
				p.want = append(p.want, desired{
					op: ops[i], rowID: row.ID, at: row.OccurredAt, leg: i,
					pairable: pairableLeg(row), deferred: owed,
				})
			}
			// Every row that reaches here is one the projection READ: one that
			// became journal entries, or one it turns into nothing on purpose
			// (a BROKER_FEE, which is the same money as a trade's commission
			// field). Neither is a row this program failed to read, so whatever
			// reason either carried goes — and the detail with it, in the same
			// verdict, since a detail outliving its code would explain a refusal
			// that is no longer being made.
			p.verdicts[row.ID] = UnparsedVerdict{}
		}
	}
	return p, nil
}

// resolve finds the catalog instrument a mirror row's security means, or says
// why there is none.
//
// IT SKIPS THE RESOLVER FOR A KIND OF ASSET THE OPERATION ITSELF NAMES AND THE
// RESOLVER DOES NOT ACCOUNT FOR, and the reason is not thrift, though it saves
// a broker call per futures contract. For those the PROJECTION has the truer
// word, and it is the projection's word the owner reads: a currency trade is
// refused as a currency trade — a thing this journal has a type for and lacks
// the data to build — rather than as "an asset kind this program does not
// account for", which is the only thing the resolver could say about it.
// Handing the projection no resolution and letting it name the fault is what
// keeps those two statements from collapsing into one (see ReasonCurrencyTrade,
// and instrumentRefusal, which reads this same map for the same question).
//
// A ROW THAT NAMES NO TYPE AT ALL IS STILL RESOLVED, and the difference
// matters: brokerInstrumentTypes is a set of PASSPORT types, and what an
// operation row carries is not a passport. The broker's own documentation
// warns that history after a corporate action can be incomplete and that
// identifiers on old operations have been rewritten, so a row with an empty
// instrument_type and a perfectly resolvable instrument_uid is a real shape —
// and refusing it would tell the owner "the broker calls this instrument type
// """, which the passport is about to disprove. Silence is not a claim about
// the asset, so nothing is saved by acting on it. The currency case that this
// skip exists for loses nothing either: a currency trade is refused by the
// projection on the OPERATION's own type before it ever looks at a resolved
// instrument (see ProjectRow).
//
// A RESOLVER REFUSAL IS A REASON; ANYTHING ELSE IS FATAL. The four sentinels
// below are statements about the data, and they become the row's visible
// reason — including the broker's own "no such instrument", which is the
// answer a delisted paper gives for ever and would otherwise stop this
// connection from ever syncing again. A database failure, or a broker that
// could not be reached at all, is not: recording it as "the security was not
// matched" would blame the operation for the network, and the mark would sit
// there until something happened to rebuild the row again.
func (r *Rebuilder) resolve(ctx context.Context, connID uuid.UUID, src passportSource, row MirrorRow,
	resolutions map[InstrumentRef]Resolved,
) (*Resolved, *UnparsedError, error) {
	if !namesSecurity(row) {
		return nil, nil, nil
	}
	if _, ok := brokerInstrumentTypes[row.InstrumentType]; !ok && row.InstrumentType != "" {
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
	case errors.Is(err, ErrDifferentSecurity), errors.Is(err, ErrIncompletePassport),
		errors.Is(err, ErrInstrumentNotFound):
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

// errHoldingUnreadable is a rebuild that stopped because it would otherwise
// have closed a redemption with a count taken through an entry whose effect on
// a position it cannot state. It is unexported because nothing outside this
// package can act on it differently — the sync run fails and says so — and it
// is a sentinel rather than a phrase because a test must be able to tell this
// failure from any other without reading its wording.
var errHoldingUnreadable = errors.New("tinvest: a position this rebuild cannot count")

// holdingKey is one position as this file counts it: one security on one
// account.
type holdingKey struct{ account, instrument uuid.UUID }

// holding is how many units of one security the entries so far have put on one
// account, and whether that number can be trusted at all.
type holding struct {
	units decimal.Decimal
	// unreadable: one of the entries changed the count in a way unitsMoved
	// cannot state, so the total below it means nothing. Once set it stays
	// set — everything after such an entry is counted from an unknown base.
	unreadable bool
}

// closeRedemptions gives every entry that was projected without a quantity the
// one the journal holds for it, or takes its row off the list with a reason.
//
// WHAT IT IS FOR: the broker reports a bond's full redemption as a payment and
// nothing else — no quantity, no price (see projectRedemption, and the live
// run that found 23 of them). The count of bonds it retired is the position
// the account holds at that moment, which is a fact about the whole journal
// and not about the row, so the projection names what it needs and this is
// where the number comes from.
//
// THE ORDER IS THE JOURNAL'S OWN. sortDesired has already put the entries in
// the order the write path files them — the broker's instant, then the mirror
// row's id, then the leg — and the journal folds a day in the order its rows
// were filed (operation.importCandidates, and the engine's own walk). So the
// running total below is the same number the engine will have in front of it
// when it reaches the sale, computed from the same entries in the same order.
// It also makes the number a function of the mirror alone: the same mirror
// sorts the same way and yields the same count on every rebuild, which is what
// keeps a rebuild that changed nothing from rewriting the sale.
//
// WHERE THAT AGREEMENT ENDS, said rather than glossed over: a journal row keeps
// the stamp it was first written with, so an operation the broker only reported
// after its neighbours were already in the journal sits at the END of its day
// there, while this walk puts it where its instant says. The two orders can
// therefore differ inside ONE day, and only for such a late arrival. Nothing is
// silent when they do: if this walk counted a purchase the journal has after
// the sale, the sale is bigger than the position the engine has and the engine
// refuses it, which the owner reads on the row.
//
// PRECONDITION, the same one the head of this file states: the accounts these
// links feed are fed by this import and by nothing else. A purchase entered by
// hand into one of them is not in this set — it is not even read, since the
// difference below reads only what this source wrote — so the sale built here
// would close the part of the position this import knows about and leave the
// hand-entered bonds in the journal after the bond had been redeemed. Nothing
// checks it here, for the reason given there.
func (r *Rebuilder) closeRedemptions(p *projected) error {
	held := map[holdingKey]holding{}
	// The rows whose entries have to go: a redemption with nothing to redeem is
	// an unparsed row, and an unparsed row leaves NO journal entry — not even
	// the commission beside it, which alone would be a fee for a sale that is
	// not there.
	refused := map[uuid.UUID]bool{}
	for i := range p.want {
		d := &p.want[i]
		if d.op.InstrumentID == nil {
			continue
		}
		key := holdingKey{account: d.op.AccountID, instrument: *d.op.InstrumentID}
		if d.deferred == DeferredRedeemedQuantity {
			h := held[key]
			if h.unreadable {
				// Not reachable from any broker data: every entry this
				// projection builds is one unitsMoved reads. It is reachable
				// from a change to this program — a shape that moves units
				// another way — and then a redemption of that security would
				// otherwise close a number counted from an unknown base. The
				// whole rebuild stops instead, the same answer this file gives
				// an entry with no external id.
				return fmt.Errorf(
					"%w: mirror row %s is a full redemption waiting for the position on account %s, "+
						"and an entry before it changes that position in a way this rebuild cannot read",
					errHoldingUnreadable, d.rowID, d.op.AccountID)
			}
			if !h.units.IsPositive() {
				p.verdicts[d.rowID] = UnparsedVerdict{
					Reason: string(ReasonRedemptionNothingHeld),
					Detail: fmt.Sprintf(
						"a full bond redemption with nothing to close: this connection has put %s units of the security on the account by %s",
						h.units, d.op.OccurredOn.Format("2006-01-02")),
				}
				r.log.Debug("tinvest: a full redemption found nothing to redeem",
					"mirror_row", d.rowID, "account", d.op.AccountID, "held", h.units)
				refused[d.rowID] = true
				continue
			}
			// The whole position, which is what a FULL redemption retires. The
			// copy is so the entry does not point at this walk's own running
			// total.
			qty := h.units
			d.op.Quantity = &qty
		}
		// The sale just completed goes through this the same as any other: one
		// place decides what an entry does to a position, so a filled-in
		// redemption cannot be counted by a rule of its own.
		moved, readable := unitsMoved(d.op)
		h := held[key]
		if readable {
			h.units = h.units.Add(moved)
		} else {
			h.unreadable = true
		}
		held[key] = h
	}
	if len(refused) == 0 {
		return nil
	}
	kept := p.want[:0]
	for _, d := range p.want {
		if refused[d.rowID] {
			// The name goes too: nothing may offer this entry, so nothing may
			// look it up as one the journal answered about.
			delete(p.rowOf, *d.op.ExternalID)
			continue
		}
		kept = append(kept, d)
	}
	p.want = kept
	return nil
}

// unitsMoved is what one journal entry does to the number of units of a
// security an account holds, and whether this file can say at all.
//
// IT IS A SECOND STATEMENT OF WHAT THE ENGINE DOES, which this codebase avoids
// where it can and cannot avoid here: portfolio exposes no answer to ask. What
// keeps the two from drifting is a test that folds a journal through the ENGINE
// and compares the position it hands back with the sum of this — see
// TestUnitsMovedAgreesWithTheEngine.
//
// A TYPE IT CANNOT READ IS NOT A ZERO. A split multiplies a position instead of
// adding to it, and the rounding it does that with is the engine's rule rather
// than this file's; copying it here would be a second implementation of one
// rule, and calling it "moves nothing" would leave a redemption after a split
// closing a pre-split number of bonds. No shape in this package produces one
// today. The honest answer is "cannot say", and the caller treats a position
// built through such an entry as unusable rather than as a number.
//
// The types that move nothing are listed rather than defaulted for the same
// reason: a dividend, a coupon, a tax, a fee and an amortization can all carry
// an instrument and none of them moves a unit — the engine's own arms say so,
// and an amortization says it loudest (it deliberately carries no quantity at
// all, see projectAmortization).
func unitsMoved(op operation.Operation) (decimal.Decimal, bool) {
	switch op.Type {
	case operation.TypeBuy, operation.TypeTransferIn:
		if op.Quantity == nil {
			return decimal.Zero, false
		}
		return *op.Quantity, true
	case operation.TypeSell, operation.TypeTransferOut:
		if op.Quantity == nil {
			return decimal.Zero, false
		}
		return op.Quantity.Neg(), true
	case operation.TypeDividend, operation.TypeCoupon, operation.TypeTax,
		operation.TypeFee, operation.TypeAmortization:
		return decimal.Zero, true
	}
	return decimal.Zero, false
}

// transferPair is the description of one parcel, as far as recognizing the two
// halves of a move goes: one paper, one number of units, one day, one currency.
//
// IT IS EVERY EQUALITY THE WRITE PATH REQUIRES OF A PAIR, and it is that list
// because of what the write path does when one of them fails. The check there
// (operation.pairedLegs) asks for exactly these four — the same instrument,
// the same day, the same quantity, the same currency — and for three that are not
// equalities and are settled elsewhere here: two accounts that DIFFER (the
// loop in pairTransfers checks it), one leg leaving against one arriving (that
// is how the two sides are collected there), and exactly two legs in the group
// (a group is named after the two legs it joins, so no third leg can carry it
// — see transferGroupID).
//
// A pair this code proposes and pairedLegs then refuses is not a refused
// CANDIDATE: it is a violation of the delta's contract, and the write path
// answers it by refusing the whole difference (ErrImportContract). Every other
// operation of the connection would be lost with it, on this rebuild and on
// every rebuild after, with no unparsed row anywhere naming a cause. So a leg
// that cannot be paired must fail to match HERE, where the answer is two lone
// legs — which is what the day does for a departure timestamped at 23:59 and an
// arrival after midnight.
//
// THE CURRENCY CANNOT SEPARATE TWO LEGS OF ONE PAPER, and is in this key
// anyway. A securities transfer takes its currency from the resolved
// instrument rather than from the zero payment beside it (see
// projectSecuritiesTransfer), and the instrument is already compared, so two
// legs equal on paper are equal on currency too. It stays because this key's
// job is to be pairedLegs' list of equalities and not a shorter one that
// happens to imply it today: the day a leg's currency comes from somewhere
// else again, the pairing has to notice by itself rather than hand the write
// path a pair it refuses whole.
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
	currency   string
}

// pairableLeg reports whether a mirror row's own operation type says the move
// it describes is between two accounts of this owner — TRANS_IIS_BS and
// TRANS_BS_BS, which is what transferBetweenOwnAccounts holds and the only
// place that list is read from, so the two cannot come apart. Everything else
// this journal turns into a transfer leg is a move with the outside world by
// the name of its own type (see pairTransfers).
func pairableLeg(row MirrorRow) bool {
	return brokerOpTypes[row.OpType].transfer == transferBetweenOwnAccounts
}

// pairTransfers finds the moves between two accounts of ONE connection and
// makes each of them a single event of the journal.
//
// ONLY A MOVE BETWEEN THE OWNER'S OWN ACCOUNTS IS EVER PAIRED, by the broker's
// own operation type (see pairableLeg). Shares leaving for a depositary
// outside this program and shares arriving from one are, by the names of those
// types, transactions with the outside world; two of them that happen to agree
// on paper, count, day and currency are two unrelated parcels, and joining
// them would invent an event nobody reported. What the invention costs is
// specific rather than theoretical: the arriving account would be handed a
// cost basis and acquisition dates released from ANOTHER account's queue — a
// tax basis that is not its own — and the honest mark saying the cost of those
// shares is unknown would be wiped off in the bargain.
//
// If the broker turns out to report a move between two of the owner's own
// accounts under those outside-world types as well, such a move stays two lone
// legs, each with the honest mark. That is the safe side of the error, and
// task 14 asks the question against a live account.
//
// TWO LEGS ARE ONE MOVE when one leaves and one arrives, both of a pairable
// type, on the same paper, the same number of units, the same day and the same
// currency, on two DIFFERENT accounts. The broker does not say so: it reports a
// departure on one account and an arrival on the other, with nothing tying them
// together (the operation ids that might have are the ones its own
// documentation says not to rely on), and its type does not say WHICH move
// either — TRANS_BS_BS appears on both sides. So within that type the legs are
// matched on what they SAY.
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
// WHAT AMBIGUITY IS LEFT, now that the outside world is out: the same paper,
// the same count and the same day moving twice between the same two accounts.
// Then more than two legs match one key, and the pairing is by the order
// sortDesired put them in — the broker's own instant, then the mirror row's id.
// It is deterministic, which is what matters: two parcels alike in paper,
// count, day, currency and pair of accounts are indistinguishable in the
// journal too, so no assignment of them is more true than another. The map
// below is walked in whatever order Go hands it out, and that changes nothing:
// two different parcels never compete for one leg, so each key is settled on
// its own.
//
// NOTHING HERE HAS TO TAKE A NOTE BACK, and the absence is worth a sentence
// because it used to. The mark saying a parcel's cost is unknown is put only on
// shares arriving from another broker (see noteBasisUnknown) — a leg this
// function now never pairs — so a paired arrival carries the broker's own
// description and nothing else, which is what it carried before it was paired.
func pairTransfers(want []desired) {
	type sides struct{ out, in []int }
	byParcel := map[transferPair]*sides{}
	for i := range want {
		if !want[i].pairable {
			continue
		}
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
			currency:   op.Currency,
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
//
// IT ALSO REPORTS WHAT IT LEFT IN PLACE, per mirror row. Leaving a matching
// entry alone is right until the OTHER entry of its row is refused, and then
// the row's two entries are the two halves of one event with only one half in
// the journal. apply is what puts that right, and it can only do so if it is
// told which stored rows this difference decided not to touch — which is
// knowable here and nowhere else.
func (r *Rebuilder) difference(ctx context.Context, spaceID uuid.UUID, accounts []uuid.UUID, want []desired) (
	operation.ImportDelta, map[uuid.UUID][]uuid.UUID, error,
) {
	var stored []operation.Operation
	for _, accountID := range accounts {
		rows, err := r.reader.ListBySource(ctx, spaceID, accountID, Source)
		if err != nil {
			return operation.ImportDelta{}, nil, fmt.Errorf("tinvest: read the imported journal of account %s: %w", accountID, err)
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
	keptByRow := map[uuid.UUID][]uuid.UUID{}
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
				keptByRow[d.rowID] = append(keptByRow[d.rowID], byName[*d.op.ExternalID].ID)
			}
			continue
		}
		for _, d := range unit {
			if s, ok := byName[*d.op.ExternalID]; ok {
				drop(s)
				// THE REPLACEMENT KEEPS THE ROW'S PLACE IN ITS DAY. A rewrite is
				// a removal and an insertion, and an insertion stamped afresh is
				// the youngest row of its date — so an operation the broker
				// merely reworded would move to the end of the day it happened
				// on. The journal folds a day in stamp order, that order is how
				// the FIFO queue breaks ties between parcels bought the same
				// day, and the queue decides which parcel a later sale consumes.
				// A description nobody asked about would then move the realized
				// profit of the account, and the tax figure with it. The write
				// path takes the stamp only because this row is one it is
				// removing in the same breath (see operation.ImportDelta).
				//
				// Only from a row of the SAME account, which is what the write
				// path will accept: a place in a day belongs to the journal it
				// is a place in. The two agree today — an external id names a
				// mirror row, a mirror row belongs to one link, and a link feeds
				// one account — so this guard costs a comparison and never
				// fires. It is here because the alternative to it firing is
				// ErrImportContract taking the whole difference down.
				if s.AccountID == d.op.AccountID {
					d.op.CreatedAt = s.CreatedAt
				}
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

	return operation.ImportDelta{Add: add, Remove: remove}, keptByRow, nil
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

// written is what one rebuild's difference actually did to the journal, told
// apart because the three are three different pieces of news.
type written struct {
	// added: entries written and still there when the rebuild finished.
	added int
	// withdrawn: entries this same rebuild wrote and took back again, because
	// the other half of their event was refused. They changed nothing about the
	// journal in the end — and they are still work that happened, and that will
	// happen again on every run while the refusal stands, so a run that did it
	// must not report itself as a run that did nothing.
	withdrawn int
	// retracted: entries that were in the journal BEFORE this rebuild and were
	// taken back for the same reason. Unlike the ones above, these did change
	// the journal, so they belong in RebuildStats.Removed.
	retracted int
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
// AND SO IS THE HALF THIS REBUILD NEVER OFFERED. The two entries of one row are
// two units of the difference and need not both move: when only one of them
// changes, the other matches what the journal holds and is left in place (see
// difference, which reports what it left). If the changed one is then refused,
// that untouched half is the very same lie, and it is the one a withdrawal
// looking only at what it just wrote would leave sitting there for good. Both
// halves go.
//
// The cost is a row written and withdrawn again on every rebuild for as long as
// the refusal stands — a refused row is offered afresh each time, since the
// journal's answer is about the journal as it is and changes when the missing
// history arrives. It is accepted deliberately: only the two shapes that
// produce two entries can split at all (a dividend paid to a card, and a trade
// whose commission was charged in another currency), and only when the journal
// takes one of them and refuses the other. The alternative is the half event.
//
// IF THE WITHDRAWAL ITSELF FAILS, the rebuild fails with part of an event in
// the journal — and the next rebuild takes it back rather than living with it.
// That half now MATCHES what the projection asks for, so it is left in place;
// its sibling is offered again and refused again; the row is refused again; and
// the rule above withdraws the half that was left in place. The lie is
// therefore bounded by the interval between runs, not permanent.
func (r *Rebuilder) apply(ctx context.Context, spaceID uuid.UUID, delta operation.ImportDelta,
	keptByRow map[uuid.UUID][]uuid.UUID, p *projected,
) (written, error) {
	if len(delta.Add) == 0 && len(delta.Remove) == 0 {
		return written{}, nil
	}
	applied, refused, err := r.ops.ApplyImportDelta(ctx, spaceID, delta)
	if err != nil {
		r.log.Error("tinvest: the journal would not take this rebuild's difference",
			"space", spaceID, "add", len(delta.Add), "remove", len(delta.Remove), "err", err)
		return written{}, fmt.Errorf("tinvest: apply the rebuilt projection: %w", err)
	}

	refusedRows := make(map[uuid.UUID]bool, len(refused))
	for _, ref := range refused {
		rowID, ok := p.rowOf[ref.ExternalID]
		if !ok {
			return written{}, fmt.Errorf("tinvest: the journal refused %q, which this rebuild never offered: %v",
				ref.ExternalID, ref.Err)
		}
		refusedRows[rowID] = true
		// The journal's OWN sentence, kept rather than reduced to the code
		// beside it: "engine_refused" is true of a sale with nothing behind it,
		// of an amount the journal will not hold and of a transfer whose other
		// leg failed alike, and the owner staring at one of 134 such rows has no
		// way to tell which. What goes in is an error this program's own journal
		// wrote about its own journal — no broker token, no request, nothing
		// that ever met a credential (see operation.ImportRefusal).
		p.verdicts[rowID] = UnparsedVerdict{Reason: string(ReasonEngineRefused), Detail: ref.Err.Error()}
		r.log.Warn("tinvest: the journal refused an operation the projection built",
			"mirror_row", rowID, "external_id", ref.ExternalID, "err", ref.Err)
	}

	var orphans []uuid.UUID
	for _, o := range applied {
		if o.ExternalID != nil && refusedRows[p.rowOf[*o.ExternalID]] {
			orphans = append(orphans, o.ID)
		}
	}
	fresh := len(orphans)
	for rowID := range refusedRows {
		orphans = append(orphans, keptByRow[rowID]...)
	}
	if len(orphans) > 0 {
		r.log.Warn("tinvest: withdrawing the entries of an operation the journal took only half of",
			"space", spaceID, "written_and_taken_back", fresh, "already_in_the_journal", len(orphans)-fresh)
		if _, _, err := r.ops.ApplyImportDelta(ctx, spaceID, operation.ImportDelta{Remove: orphans}); err != nil {
			r.log.Error("tinvest: withdrawing half of a written event failed, the journal holds part of one",
				"space", spaceID, "entries", len(orphans), "err", err)
			return written{}, fmt.Errorf("tinvest: withdraw a half-written event: %w", err)
		}
	}
	return written{added: len(applied) - fresh, withdrawn: fresh, retracted: len(orphans) - fresh}, nil
}

// writeVerdicts records the projection's verdicts on the mirror, and writes only
// the ones that changed.
//
// Only what changed, because the whole history is stated afresh on every
// rebuild and an unconditional write would rewrite every row of a decade-old
// account every hour to say what it already said. The comparison is against the
// value read at the start of this same rebuild, which is sound because the two
// columns have exactly one writer — this, through SetUnparsedVerdicts — and runs
// of one connection do not overlap.
//
// CHANGED MEANS EITHER HALF CHANGED. A row whose code stands and whose detail
// now names a different security is a row whose stored answer is wrong, and
// comparing codes alone would leave that stale sentence under it for ever.
func (r *Rebuilder) writeVerdicts(ctx context.Context, p *projected) error {
	changed := map[uuid.UUID]UnparsedVerdict{}
	for rowID, verdict := range p.verdicts {
		if p.stored[rowID] != verdict {
			changed[rowID] = verdict
		}
	}
	if len(changed) == 0 {
		return nil
	}
	if err := r.store.SetUnparsedVerdicts(ctx, changed); err != nil {
		r.log.Error("tinvest: recording why operations could not be read failed", "rows", len(changed), "err", err)
		return err
	}
	return nil
}
