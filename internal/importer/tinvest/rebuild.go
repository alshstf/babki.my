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

// The rebuild turns the mirror into journal entries, and can do it again over a
// changed mirror or a changed projection rule without the broker: the broker
// rewrites its history, and the rules will be corrected. It computes the whole
// desired journal from the whole mirror and diffs it; it never asks "what is new
// since last time".
//
// It runs per connection, not per link: a move between the owner's accounts is
// two rows under two links (see pairTransfers).
//
// Precondition: the accounts these links name are fed by this connection alone.
// The difference is per (account, source), so another connection's rows in one
// of them would be removed. Nothing enforces this yet; it belongs where links are
// created.

// nsTinvest is the UUID v5 namespace of the identifiers this importer
// invents. Changing it renames every transfer group and rewrites every paired
// transfer.
var nsTinvest = uuid.MustParse("55c16945-9170-4eff-a3c3-b8886b38f8fb")

// journalDelta is the write path the difference goes to: one transaction,
// with per-candidate refusals returned. *operation.Service satisfies it.
type journalDelta interface {
	ApplyImportDelta(ctx context.Context, spaceID uuid.UUID, d operation.ImportDelta) (
		[]operation.Operation, []operation.ImportRefusal, error)
}

// journalReader is the journal side of the difference: what this importer has
// already written into one account. *operation.Store satisfies it.
type journalReader interface {
	ListBySource(ctx context.Context, spaceID, accountID uuid.UUID, source string) ([]operation.Operation, error)
	StatedPurchases(ctx context.Context, spaceID uuid.UUID, accountIDs []uuid.UUID, source string) (
		map[operation.StatedKey][]operation.ReleasedLot, error)
}

// RebuildStats is what one rebuild changed.
//
//   - Added: entries written and kept.
//   - Removed: entries there before and gone now, including a half event this
//     rebuild took out (see apply).
//   - Withdrawn: entries this rebuild wrote and took back because the other half
//     of their event was refused. Not a change, but repeated work on every run
//     while the refusal stands, so reported.
//   - Unparsed: rows carrying a reason after the rebuild, the set
//     UnparsedByConnection lists.
type RebuildStats struct{ Added, Removed, Withdrawn, Unparsed int }

// projection is ProjectRow's signature, a field so a test can swap the rule
// and watch the journal follow.
type projection func(row MirrorRow, accountID uuid.UUID, resolved *Resolved, traded *TradedCurrency) ([]operation.Operation, Deferred, *UnparsedError)

// Rebuilder turns one connection's mirror into journal operations. One per
// sync run, since its Resolver caches passports for the run. Not safe for
// concurrent use; two rebuilds of one connection must not overlap either, as both
// would diff against the same journal. The scheduler serializes them.
type Rebuilder struct {
	store    *Store
	resolver *Resolver
	ops      journalDelta
	reader   journalReader
	log      *slog.Logger
	project  projection
	// faces measures repayments against outstanding face value
	// (fillFaceBefore); nil keeps the old rule.
	faces faceSchedule
}

// faceSchedule answers a bond's outstanding face per unit just before a
// repayment on day on, by ISIN.
type faceSchedule interface {
	FaceBeforeByISIN(ctx context.Context, isin string, on time.Time) (decimal.Decimal, string, bool, error)
}

// WithFaceSchedule gives the rebuild the exchange's repayment schedules.
func (r *Rebuilder) WithFaceSchedule(faces faceSchedule) *Rebuilder {
	r.faces = faces
	return r
}

func NewRebuilder(store *Store, resolver *Resolver, ops journalDelta, reader journalReader, log *slog.Logger) *Rebuilder {
	if log == nil {
		log = slog.Default()
	}
	return &Rebuilder{store: store, resolver: resolver, ops: ops, reader: reader, log: log, project: ProjectRow}
}

// Rebuild brings the journal into agreement with one connection's mirror.
//
// links must be every link of conn: a transfer paired with a missing one would be
// projected as a lone leg, and the write path refuses removing one leg of a pair.
//
// src is used only for passports of instruments neither the map nor the catalog
// knows.
//
// Not one transaction: the difference is applied in one and verdicts written in
// another; a crash between leaves a stale verdict the next rebuild corrects.
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
	// After the sort: a redemption's count is the position built by the
	// entries before it.
	if err := r.closeRedemptions(p); err != nil {
		return RebuildStats{}, err
	}
	// Restated before anything pairs or writes.
	if err := r.convertToPositionCurrency(ctx, p); err != nil {
		return RebuildStats{}, err
	}
	// After restating: the face is measured in the entry's currency.
	if err := r.fillFaceBefore(ctx, p); err != nil {
		return RebuildStats{}, err
	}
	r.settleBrokerFees(p)
	pairTransfers(p.want)
	// After pairing, which says which arrivals have no sibling.
	if err := r.applyStatedPurchases(ctx, conn.SpaceID, accountsOf(links), p.want); err != nil {
		return RebuildStats{}, err
	}

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

// desired is one entry the mirror asks for, with what later steps need of its
// row.
type desired struct {
	op    operation.Operation
	rowID uuid.UUID
	// at is the broker's instant, ordering entries within a day (see
	// sortDesired); op.OccurredOn is the Moscow day.
	at time.Time
	// leg keeps one row's entries in the order the projection built them.
	leg int
	// pairable: the row's broker type says the move is between the owner's
	// accounts, the only kind of leg that may be joined (pairTransfers). Read
	// off the row because transfer_in is the same either way.
	pairable bool
	// deferred is what this entry still owes (see Deferred), per entry: a
	// redemption's sale waits for a count, its commission does not.
	deferred Deferred
	// linkID and parentBrokerID settle a deferred broker fee: the row's
	// account and the trade it names (see settleBrokerFees).
	linkID         uuid.UUID
	parentBrokerID string
}

// projected is everything one pass over the mirror produced.
type projected struct {
	want []desired
	// verdicts is what this rebuild decided about the rows it ruled on: zero
	// for a row that became entries, a code and detail for one that did not. A
	// row not in here keeps its stored verdict (see projectAll).
	verdicts map[uuid.UUID]UnparsedVerdict
	// stored is every read row's verdict as in the database, so only changes
	// are written; Unparsed counts its reasons.
	stored map[uuid.UUID]UnparsedVerdict
	// rowOf maps an entry's external id to its mirror row, remembered rather
	// than parsed from the name.
	rowOf map[string]uuid.UUID
	// seen and feeBooked answer a deferred fee's questions about its trade:
	// is it in the mirror, and did it book its own commission. Keyed by broker
	// id within a link, the only scope those ids mean anything in. Filled
	// during the pass, read after.
	seen      map[brokerRef]bool
	feeBooked map[brokerRef]bool
	// explained: the owner accounted for the operation by hand, so it
	// produced no entries on purpose; a third answer for a deferred fee (see
	// settleBrokerFees).
	explained map[brokerRef]bool
	// projected: the operation became entries at all. A trade that did not
	// has its own unparsed row, which already covers its commission.
	projected map[brokerRef]bool
}

// brokerRef names a broker operation by its id within one link.
type brokerRef struct {
	linkID   uuid.UUID
	brokerID string
}

// unparsed is how many of the connection's rows carry a reason now.
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

// projectAll projects every row of every link. A row that did not happen
// (cancelled or withdrawn) is skipped and its verdict left alone: a stored reason
// is still true of it, and UnparsedByConnection lists it with DisappearedAt.
func (r *Rebuilder) projectAll(ctx context.Context, conn Connection, links []AccountLink, src passportSource) (*projected, error) {
	p := &projected{
		verdicts:  map[uuid.UUID]UnparsedVerdict{},
		stored:    map[uuid.UUID]UnparsedVerdict{},
		rowOf:     map[string]uuid.UUID{},
		seen:      map[brokerRef]bool{},
		feeBooked: map[brokerRef]bool{},
		explained: map[brokerRef]bool{},
		projected: map[brokerRef]bool{},
	}
	// One run's resolutions, so a paper named a thousand times is looked up
	// once. Local, so a later run sees catalog changes.
	resolutions := map[InstrumentRef]Resolved{}

	for _, link := range links {
		rows, err := r.store.MirrorRowsByLink(ctx, link.ID)
		if err != nil {
			return nil, err
		}
		explained, err := r.store.ExplainedKeysByLink(ctx, link.ID)
		if err != nil {
			return nil, err
		}
		paidFor, withdrawn := pairFundRedemptions(rows)
		// Settlement days from the broker report (Р-3); empty until one is
		// read.
		settledDays, err := r.store.tradeSettlementsByLink(ctx, link.ID)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			p.stored[row.ID] = UnparsedVerdict{Reason: row.UnparsedReason, Detail: row.UnparsedDetail}
			if explained[row.ContentKey] {
				// The owner explained this row: it produces no entries and carries no
				// reason, which is how every unparsed count drops it; the screen shows
				// it from the explanations table. Checked before the state check, so a
				// row explained and later withdrawn does not get its old reason back.
				p.verdicts[row.ID] = UnparsedVerdict{}
				if row.State == stateExecuted && row.DisappearedAt == nil {
					// Recorded so a fee naming this trade finds it (settleBrokerFees).
					ref := brokerRef{link.ID, row.BrokerOperationID}
					p.seen[ref] = true
					p.explained[ref] = true
				}
				continue
			}
			if row.State != stateExecuted || row.DisappearedAt != nil {
				// Not recorded as seen: a cancelled order's commission never entered
				// the journal.
				continue
			}
			p.seen[brokerRef{link.ID, row.BrokerOperationID}] = true
			if withdrawn[row.ID] {
				// Units withdrawn for a paired fund redemption belong to that entry
				// (pairFundRedemptions); this row is read.
				p.verdicts[row.ID] = UnparsedVerdict{}
				continue
			}
			resolved, refusal, err := r.resolve(ctx, conn.ID, src, row, resolutions)
			if err != nil {
				return nil, err
			}
			// What a currency row trades, asked of the broker once per pair, so the
			// projection stays pure. Without an answer the trade stays unparsed.
			var traded *TradedCurrency
			if refusal == nil && row.InstrumentType == brokerCurrencyInstrumentType && row.InstrumentUID != "" {
				// What the operation says about the pair, for a delisted dollar or euro
				// pair the broker cannot answer about (two dozen in the owner's
				// history). The price is checked against the official rate (see
				// Resolver.currencyFromHint).
				hint := CurrencyHint{
					Ticker:     row.Ticker,
					Settlement: row.Currency,
					On:         row.OccurredAt,
				}
				// No price leaves the hint unprovable.
				if row.Price != nil {
					hint.PricePerUnit = *row.Price
				}
				resolvedCurrency, currencyErr := r.resolver.ResolveCurrency(ctx, src, row.InstrumentUID, hint)
				switch {
				case currencyErr == nil:
					traded = &resolvedCurrency
				case errors.Is(currencyErr, ErrIncompletePassport), errors.Is(currencyErr, ErrInstrumentNotFound):
					refusal = &UnparsedError{Reason: ReasonCurrencyTrade, Detail: currencyErr.Error()}
				default:
					return nil, currencyErr
				}
			}
			var (
				ops      []operation.Operation
				deferred Deferred
			)
			if w, paired := paidFor[row.ID]; refusal == nil && paired {
				ops, refusal = projectFundRedemption(row, w, link.AccountID, resolved)
				if refusal == nil {
					ops = withExternalIDs(row.ID, ops)
				}
			} else if refusal == nil {
				ops, deferred, refusal = r.project(row, link.AccountID, resolved, traded)
			}
			if refusal != nil {
				// The detail goes to the mirror too: "the engine refused this" on 134 of
				// the owner's rows helped nobody; the engine's own sentence does.
				p.verdicts[row.ID] = UnparsedVerdict{Reason: string(refusal.Reason), Detail: refusal.Detail}
				r.log.Debug("tinvest: a broker operation did not become journal entries",
					"mirror_row", row.ID, "op_type", row.OpType, "reason", refusal.Reason, "detail", refusal.Detail)
				continue
			}
			if len(ops) > 0 {
				p.projected[brokerRef{link.ID, row.BrokerOperationID}] = true
			}
			settled := settlementDay(row.Raw, settledDays)
			for i := range ops {
				if ops[i].FeeMinor != 0 {
					// This row booked its own commission, which is what a fee naming it
					// asks. Read off the entry: a commission in another currency becomes its
					// own entry (tradeCommission).
					p.feeBooked[brokerRef{link.ID, row.BrokerOperationID}] = true
				}
				if ops[i].ExternalID == nil || *ops[i].ExternalID == "" {
					return nil, fmt.Errorf("tinvest: the projection produced an entry with no external id for mirror row %s", row.ID)
				}
				p.rowOf[*ops[i].ExternalID] = row.ID
				// A deferral is about the first entry (see Deferred).
				owed := DeferredNothing
				if i == 0 {
					owed = deferred
				}
				// The broker's instant orders the day (operation.foldsBefore).
				at := row.OccurredAt
				ops[i].OccurredAt = &at
				settleOn(&ops[i], settled)
				p.want = append(p.want, desired{
					op: ops[i], rowID: row.ID, at: row.OccurredAt, leg: i,
					pairable: pairableLeg(row), deferred: owed,
					linkID: link.ID, parentBrokerID: row.ParentOperationID,
				})
			}
			// Every row here was read, so its verdict is cleared, detail included.
			// settleBrokerFees leaves a dropped duplicate fee's verdict alone too.
			p.verdicts[row.ID] = UnparsedVerdict{}
		}
	}
	return p, nil
}

// resolve finds the catalog instrument a row's security means, or why not.
//
// It skips the resolver for asset kinds the operation names and the resolver does
// not support, so the projection gives the truer reason (a currency trade, not
// "unsupported asset kind"; see ReasonCurrencyTrade). A row naming no type is
// still resolved: brokerInstrumentTypes is a passport vocabulary, and the broker
// warns that old operations can lack it.
//
// A resolver sentinel becomes the row's reason, including the broker's "no such
// instrument" for a delisted paper; a database or network failure is fatal, not
// blamed on the operation.
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
		// What the operation called the paper and paid in; used only when the
		// broker has forgotten the instrument (Resolver.resolveOne).
		Ticker:   row.Ticker,
		Currency: row.Currency,
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

// sortDesired puts entries in fold order: the broker's instant, then mirror
// row id, then leg. Not first-seen order: a first import shares one first_seen_at,
// leaving rows in random uuid order, and a sale could precede its purchase. The
// same mirror always sorts the same way, so an unchanged rebuild does not
// renumber.
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

// errHoldingUnreadable stops a rebuild that would close a redemption with a
// count taken through an entry whose effect it cannot state. A sentinel so tests
// can tell it apart.
var errHoldingUnreadable = errors.New("tinvest: a position this rebuild cannot count")

// holdingKey is one security on one account.
type holdingKey struct{ account, instrument uuid.UUID }

// holding is the units the entries so far put on one account, and whether
// that count is trustworthy.
type holding struct {
	units decimal.Decimal
	// unreadable: an entry changed the count in a way unitsMoved cannot
	// state; it stays set.
	unreadable bool
}

// closeRedemptions gives each entry projected without a quantity the position
// held at that point, or refuses its row. The broker reports a bond's full
// redemption as a payment only (see projectRedemption). The running total walks
// sortDesired's order, the order the engine folds in, so it is the number the
// engine will see, and the same on every rebuild. The file's precondition
// applies: a hand-entered purchase in one of these accounts is not counted.
func (r *Rebuilder) closeRedemptions(p *projected) error {
	held := map[holdingKey]holding{}
	// A redemption with nothing to redeem is unparsed, and leaves no entry,
	// not even its commission.
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
				// Unreachable from broker data; a future shape moving units another way
				// would otherwise close a count from an unknown base.
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
			// The whole position, copied.
			qty := h.units
			d.op.Quantity = &qty
		}
		// The filled-in sale is counted by the same rule as every entry.
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
			// And its name, so nothing looks it up.
			delete(p.rowOf, *d.op.ExternalID)
			continue
		}
		kept = append(kept, d)
	}
	p.want = kept
	return nil
}

// settleBrokerFees decides, for each fee charged as its own operation, whether
// the journal already has that money. The test is whether the named trade booked
// a commission (in its fee or in an entry of its own); comparing amounts would
// misjudge equal or differently rounded charges.
//
//   - The trade booked one: the fee is a duplicate and is dropped; its row stays
//     read.
//   - The trade booked none, or the fee names none: the fee is the only record
//     and stays (11,34 ₽ on one of the owner's 311).
//   - The trade is itself unparsed: its row already reports the money (79 of the
//     owner's currency trades would otherwise each get a second row).
//   - The trade is not in this mirror: the fee is unparsed rather than guessed.
//
// Entries are dropped in place to keep sortDesired's order.
func (r *Rebuilder) settleBrokerFees(p *projected) {
	kept := p.want[:0]
	for _, d := range p.want {
		if d.deferred != DeferredBrokerFeeVerdict {
			kept = append(kept, d)
			continue
		}
		ref := brokerRef{d.linkID, d.parentBrokerID}
		switch {
		case d.parentBrokerID == "":
			// Names no trade: nothing to duplicate.
			kept = append(kept, d)
		case p.feeBooked[ref]:
			r.log.Debug("tinvest: a broker fee repeats the commission its trade already booked",
				"mirror_row", d.rowID, "parent", d.parentBrokerID)
			delete(p.rowOf, *d.op.ExternalID)
		case p.explained[ref]:
			// The trade is explained by hand and this fee is not; dropping it would
			// lose the money silently, since an explained row reports nothing. The
			// owner decides.
			p.verdicts[d.rowID] = UnparsedVerdict{
				Reason: string(ReasonBrokerFeeParentExplained),
				Detail: fmt.Sprintf("the operation this commission names (%s) is accounted for by a manual entry, and this commission is not part of it unless it was entered there too", d.parentBrokerID),
			}
			delete(p.rowOf, *d.op.ExternalID)
		case p.seen[ref] && !p.projected[ref]:
			// The trade is on the unparsed list with its own reason, which covers
			// its commission.
			r.log.Debug("tinvest: a broker fee belongs to a trade that is itself unparsed, so it goes with it",
				"mirror_row", d.rowID, "parent", d.parentBrokerID)
			delete(p.rowOf, *d.op.ExternalID)
		case p.seen[ref]:
			kept = append(kept, d)
		default:
			p.verdicts[d.rowID] = UnparsedVerdict{
				Reason: string(ReasonBrokerFeeParentMissing),
				Detail: fmt.Sprintf("the trade this commission names (%s) is not among the operations this connection imported, so whether the trade already carries it cannot be told", d.parentBrokerID),
			}
			delete(p.rowOf, *d.op.ExternalID)
		}
	}
	p.want = kept
}

// unitsMoved is what one entry does to the units an account holds, and
// whether that can be said. A second statement of the engine's rule, since
// portfolio exposes none; TestUnitsMovedAgreesWithTheEngine keeps them
// together. A split multiplies with the engine's rounding and is "cannot say",
// not zero; nothing here produces one. Types that move nothing are listed, not
// defaulted.
func unitsMoved(op operation.Operation) (decimal.Decimal, bool) {
	switch op.Type {
	case operation.TypeBuy, operation.TypeTransferIn:
		if op.Quantity == nil {
			return decimal.Zero, false
		}
		return *op.Quantity, true
	case operation.TypeSell, operation.TypeRedemption, operation.TypeTransferOut:
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

// transferPair is what makes two legs one parcel: paper, units, day and
// currency, exactly operation.pairedLegs' equalities (it also needs different
// accounts, one out and one in, and two legs, settled in pairTransfers and
// transferGroupID). A pair the write path refused would break the delta's
// contract and fail the whole difference, so a doubtful pair must fail to match
// here and stay two lone legs. Currency is implied by the paper today and kept
// anyway. The quantity is its canonical String(), since decimals equal in value
// can differ in form.
type transferPair struct {
	instrument uuid.UUID
	quantity   string
	day        string
	currency   string
}

// pairableLeg: the row's type is TRANS_IIS_BS or TRANS_BS_BS, read from
// transferBetweenOwnAccounts.
func pairableLeg(row MirrorRow) bool {
	return brokerOpTypes[row.OpType].transfer == transferBetweenOwnAccounts
}

// pairTransfers joins the two legs of a move between two accounts of one
// connection into one event.
//
// Only moves between the owner's own accounts (by broker type, pairableLeg) are
// paired. Two outside-world legs that happen to agree are two parcels; joining
// them would hand the arriving account another account's basis and dates and wipe
// the "cost unknown" mark. If the broker reports own-account moves under
// outside-world types, they stay lone legs with the mark, the safe error. No
// TRANS_* operation exists on the owner's account, so this rests on the docs.
//
// Two legs pair when one leaves and one arrives, both pairable, same paper, units,
// day and currency, on different accounts; the broker links them by nothing else
// reliable. An unmatched leg stays alone, which is ordinary. The group is derived
// from the legs' names, so an unchanged rebuild changes nothing. Two identical
// moves on one day pair in sortDesired order, which is deterministic and no less
// true than any other assignment.
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

// transferGroupID names the event from the two legs' names, sorted. "|"
// cannot appear in an external id (see withExternalIDs).
func transferGroupID(a, b string) uuid.UUID {
	pair := []string{a, b}
	sort.Strings(pair)
	return uuid.NewSHA1(nsTinvest, []byte(strings.Join(pair, "|")))
}

// accountsOf is the links' babki accounts, each once, in order.
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

// difference is what must change for the journal to say what the mirror says.
// Rows match by external id and then by value: the mirror holds the broker's
// latest word, so a changed row is removed and written again, which also lets the
// engine judge the new values. A transfer is diffed as one event, both legs moving
// together, since the journal refuses half a pair either way; a matching leg
// carries the same derived group, so its sibling is in the same unit. It also
// reports which stored rows it left in place per mirror row, which apply needs
// when the row's other entry is refused.
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
		// No name: not this projection's row; treated as unwanted.
		if o.ExternalID == nil || *o.ExternalID == "" {
			continue
		}
		if _, taken := byName[*o.ExternalID]; !taken {
			byName[*o.ExternalID] = o
		}
	}

	var remove []uuid.UUID
	// No id twice in the removal list, or the write path refuses the whole
	// delta (operation.importRemovals).
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
				// The replacement inherits the removed row's stamp, so a reworded
				// operation keeps its place in the day; within an instant the stamp
				// breaks FIFO ties and so decides realized profit (see
				// operation.ImportDelta). Only from the same account, which always
				// holds today; otherwise ErrImportContract would fail the delta.
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

// unitsOf groups entries into events accepted or refused whole: a transfer's
// two legs, otherwise one each, in sortDesired order.
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

// sameJournalRow reports whether the stored row already says what the
// projection says. Every settable column is compared, even ones no rule sets yet,
// or the mirror could not correct it. Excluded: the external id (the match key),
// a basis the journal owns (journalOwnsBasis, as checkImportContract), and
// TransferLots, which the write path releases.
func sameJournalRow(want, stored operation.Operation) bool {
	if want.AccountID != stored.AccountID ||
		want.Type != stored.Type ||
		want.Currency != stored.Currency ||
		want.Note != stored.Note ||
		want.Source != stored.Source ||
		want.FeeMinor != stored.FeeMinor {
		return false
	}
	if !want.OccurredOn.Equal(stored.OccurredOn) || !sameTime(want.OccurredAt, stored.OccurredAt) {
		return false
	}
	if !sameMinor(want.FaceBeforeMinor, stored.FaceBeforeMinor) {
		return false
	}
	if !sameTime(want.SettledOn, stored.SettledOn) {
		return false
	}
	// Compared like every column; also fills the mode into older rows.
	if !sameString(want.TradingMode, stored.TradingMode) {
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
	// An arrival's stated breakdown: equal bases can come from different
	// days.
	if want.Type == operation.TypeTransferIn && !sameLots(want.TransferLots, stored.TransferLots) {
		return false
	}
	return want.AmountMinor == stored.AmountMinor
}

// sameLots compares two breakdowns piece by piece.
func sameLots(a, b []operation.ReleasedLot) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].From != b[i].From || !a[i].Quantity.Equal(b[i].Quantity) || a[i].CostMinor != b[i].CostMinor || !sameTime(a[i].AcquiredOn, b[i].AcquiredOn) {
			return false
		}
	}
	return true
}

// applyStatedPurchases restores the owner's stated purchases for arrivals from
// another broker (operation.Service.StatePurchases), which the broker's record
// lacks. A statement that no longer adds up to the reported shares is not applied;
// the shares count as bought for nothing until restated.
func (r *Rebuilder) applyStatedPurchases(ctx context.Context, spaceID uuid.UUID, accounts []uuid.UUID, want []desired) error {
	stated, err := r.reader.StatedPurchases(ctx, spaceID, accounts, Source)
	if err != nil {
		return fmt.Errorf("tinvest: read the purchases stated for arrivals: %w", err)
	}
	if len(stated) == 0 {
		return nil
	}
	for i := range want {
		op := &want[i].op
		if op.Type != operation.TypeTransferIn || op.TransferGroupID != nil || op.ExternalID == nil || op.Quantity == nil {
			continue
		}
		pieces, ok := stated[operation.StatedKey{AccountID: op.AccountID, ExternalID: *op.ExternalID}]
		if !ok {
			continue
		}
		var cost int64
		total := decimal.Zero
		for _, pc := range pieces {
			cost += pc.CostMinor
			total = total.Add(pc.Quantity)
		}
		if !total.Equal(*op.Quantity) {
			r.log.Warn("tinvest: the purchases stated for an arrival no longer add up to what the broker reports, leaving them off",
				"account", op.AccountID, "operation", *op.ExternalID, "stated", total.String(), "broker", op.Quantity.String())
			continue
		}
		op.AmountMinor, op.TransferLots = cost, pieces
	}
	return nil
}

// journalOwnsBasis: the write path computes this entry's amount, the same
// condition operation.checkImportContract uses.
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

// sameString compares optional strings; nil and "" differ (migration 0026).
func sameString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// sameMinor compares two optional amounts.
func sameMinor(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// sameTime compares optional moments by the moment named.
func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// sameNumber compares by value: NUMERIC(30,10) returns 100 as
// 100.0000000000.
func sameNumber(a, b *decimal.Decimal) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// written is what the difference did to the journal, in three parts.
type written struct {
	// added: written and still there.
	added int
	// withdrawn: written and taken back this run; no net change, but repeated
	// work while the refusal stands.
	withdrawn int
	// retracted: entries from before this rebuild taken back the same way;
	// these count in RebuildStats.Removed.
	retracted int
}

// apply hands the difference to the journal and turns refusals into reasons.
//
// An event is written whole or not at all. The write path judges candidates one
// by one, and a row's two entries (a dividend to a card and its withdrawal, a
// trade and its other-currency commission) are two candidates; half of one is a
// lie, so the accepted half is withdrawn afterwards. So is a half this rebuild
// left untouched because it already matched (see difference).
//
// The cost is a write and withdrawal every run while the refusal stands, only for
// those two shapes. If the withdrawal fails the rebuild fails, and the next one
// takes the half back.
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
		// The journal's own sentence, not just the code: "engine_refused" covers
		// many faults. It is this program's text about its own journal, nothing
		// from the broker or any credential.
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

// writeVerdicts writes only the verdicts that changed, code or detail, compared
// with what this rebuild read at its start; sound because SetUnparsedVerdicts is
// the only writer and runs of a connection do not overlap.
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
