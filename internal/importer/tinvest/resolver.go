package tinvest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/platform/currency"
	"babki.my/babki/internal/platform/money"
)

// InstrumentRef is the broker's four identifiers for one instrument, taken
// off a single operation. All four drift independently over the
// instrument's lifetime — the task brief's own research into the API: an
// id may be reused or reassigned, and figi/instrument_uid on old operations
// have already been seen to change. Resolve is built to survive any one of
// them changing; this type exists so a caller hands it whichever set one
// operation happened to carry, rather than four loose strings whose order
// at the call site is easy to get wrong.
type InstrumentRef struct {
	InstrumentUID, FIGI, PositionUID, AssetUID string
}

// Resolved is what Resolve found: this instance's catalog id for the
// broker's instrument, and the catalog's own type for it. Type rides along
// so a caller that branches on it (a projection deciding how to book a
// trade) does not need a second round trip to get it.
type Resolved struct {
	InstrumentID uuid.UUID
	Type         instrument.Type
}

// ErrUnsupportedInstrumentType means this program cannot account for the
// broker's instrument_type at all — see brokerInstrumentTypes for the three
// it can (share, bond, etf). Futures, options, structured bonds, currency and
// everything else outside that set refuse rather than being filed as some
// generic "other" row: an operation against one becomes a visible unparsed
// entry instead of a catalog row nothing in this program can value (the
// task brief's decision 3).
var ErrUnsupportedInstrumentType = errors.New("tinvest: unsupported instrument type")

// ErrDifferentSecurity means the catalog row that carries the broker's ticker
// is a DIFFERENT security, so resolving to it would file the broker's trades
// against somebody else's paper.
//
// It is its own sentinel and not a flavour of ErrUnsupportedInstrumentType
// because the two say opposite things to the person reading the unparsed
// entry this refusal becomes: one means "this program does not account for
// this kind of asset at all", the other means "a row in your catalog and the
// broker's passport disagree about which paper this is, and one of them has
// to be corrected".
//
// THERE IS NOTHING ELSE THIS CASE COULD DO. The catalog holds at most one
// tradable row per ticker (migration 0011's partial unique index, the one
// behind instrument.ErrTickerTaken), so creating a second row for the same
// ticker is not available either — see refuseContradiction for the case that
// found this.
var ErrDifferentSecurity = errors.New("tinvest: the catalog row with this ticker is a different security")

// ErrIncompletePassport means the broker's own answer about an instrument
// does not carry what a catalog row requires, so no row can honestly be
// created from it — see checkPassport and the bond nominal in
// createInstrument.
var ErrIncompletePassport = errors.New("tinvest: broker passport is missing what a catalog row requires")

// instrumentCatalog is the subset of instrument.Store the resolver needs —
// the same locally declared interface pattern as marketdata's
// instrumentLister (internal/marketdata/jobs.go): committing to four methods
// rather than the whole store keeps this package free of anything it does
// not call. *instrument.Store satisfies it structurally.
//
// resolver_test.go wraps the real store rather than faking it from scratch:
// tinvest_instrument_map carries a real foreign key to instruments(id)
// (migration 0014), so an instrument id this interface hands back has to
// name an actual row there or (*Store).saveMap's own write fails — an
// in-memory fake that never writes one cannot satisfy that.
type instrumentCatalog interface {
	ByISIN(ctx context.Context, isin string) (instrument.Instrument, error)
	ByTickerTradable(ctx context.Context, ticker string) (instrument.Instrument, error)
	Create(ctx context.Context, inst instrument.Instrument) (instrument.Instrument, error)
	Update(ctx context.Context, id uuid.UUID, upd instrument.Update) (instrument.Instrument, error)
}

// passportSource is the broker calls Resolve needs to identify an
// instrument neither this connection's map nor the shared catalog already
// knows. It is a parameter of Resolve rather than a field NewResolver takes,
// because it is bound to one connection's token (see *Client) while the
// resolver is not — one Resolver, and the passport cache it carries, serves
// every connection a sync run touches.
//
// BondNominalByUID is declared here even though the task brief's own
// interface sketch names InstrumentByUID alone: GetInstrumentBy (behind
// InstrumentByUID) does not carry a bond's nominal at all (see
// InstrumentBrief's doc comment in client.go — this was checked against the
// live API and the field simply is not in that response), and a bond cannot
// be created without one. Leaving it out of this interface would make
// creating a bond impossible to implement at all, so it is added here as a
// deliberate, necessary correction rather than an oversight. *Client
// satisfies both methods already.
type passportSource interface {
	InstrumentByUID(ctx context.Context, uid string) (InstrumentBrief, error)
	BondNominalByUID(ctx context.Context, uid string) (MoneyValue, error)
}

// brokerInstrumentTypes maps the broker's own instrument_type strings (see
// InstrumentBrief.InstrumentType) to this catalog's Type. Only the types an
// imported operation can be booked against are listed. Absence from this map
// IS the refusal (ErrUnsupportedInstrumentType) — there is deliberately no
// second list of excluded types to keep in step with this one.
//
// CURRENCY IS NOT HERE, AND THAT IS THE POINT rather than an omission. A
// currency trade needs no instrument from this resolver either way. It does
// not reach the journal at all today — ProjectRow refuses it with
// ReasonCurrencyTrade, because the mirror names neither the traded currency
// nor its nominal per unit — and the journal type it would reach if that
// changed is "conversion", which the engine skips whole, moving on to the next
// operation before it ever asks which instrument the row names (engine.go,
// `if o.Type == TypeConversion { continue }`). So this resolver must never be
// called for a currency, under the rule that stands now and under the one that
// would replace it.
//
// Listing currency here would have made a wrong call silent and expensive
// instead. Neither lookup below can find a currency row: ByTickerTradable
// covers share/bond/etf and would not return one even when the catalog holds
// it, and an ISIN identifies a security rather than a currency, so the exact
// lookup has nothing to match on either. Creation is then the only outcome
// left — and unlike a share, a fund or a bond, a duplicate currency row is not
// stopped by anything, because the unique ticker index covers exactly those
// three types and leaves currency outside it (migration 0011). Measured rather
// than reasoned about: with currency accepted, two connections resolving one
// currency left TWO rows for it in the instance-wide catalog. Refusing loudly
// is the difference between one visible unparsed entry and a catalog that
// grows a duplicate per connection per currency.
var brokerInstrumentTypes = map[string]instrument.Type{
	"share": instrument.TypeShare,
	"bond":  instrument.TypeBond,
	"etf":   instrument.TypeETF,
}

// minorScale is this codebase's fixed decimal-to-minor-units factor: every
// money figure this program stores is two decimal digits scaled up to a
// whole number, the same convention the frontend hardcodes
// (web/src/lib/money.ts's parseAmount, `wholeAbs * 100 + fracPadded`) and
// the one instrument/http_face_value_test.go's own fixture exercises (a
// 1 000 RUB bond nominal stored as face_value_minor 100000). No currency in
// this catalog carries a different number of minor-unit digits — there is
// no such table anywhere in this codebase — so a bond nominal quoted in any
// currency scales the same way.
var minorScale = decimal.New(1, 2)

// Resolver turns a broker instrument reference into this instance's catalog
// id, creating a catalog row the first time this exact instrument is seen
// anywhere in the instance. Build one per sync run with NewResolver and
// reuse it across every operation the run resolves — see passports.
//
// NOT SAFE FOR CONCURRENT USE: passports is a plain map, and a sync run
// resolves one operation at a time.
type Resolver struct {
	store     *Store
	catalog   instrumentCatalog
	log       *slog.Logger
	passports map[string]InstrumentBrief
}

// NewResolver builds a Resolver. store owns the per-connection instrument
// map (tinvest_instrument_map); catalog is this instance's shared
// instrument catalog.
func NewResolver(store *Store, catalog instrumentCatalog, log *slog.Logger) *Resolver {
	if log == nil {
		log = slog.Default()
	}
	return &Resolver{store: store, catalog: catalog, log: log, passports: map[string]InstrumentBrief{}}
}

// Resolve turns one broker instrument reference into this instance's
// catalog id and type, going as far as it has to and no further:
//
//  1. tinvest_instrument_map, by instrument_uid then by figi — this
//     connection's own memory of what it has already resolved. The common
//     case once a connection has synced once: no broker call, no catalog
//     call, one query.
//  2. failing that, the broker's passport (InstrumentByUID), cached on this
//     Resolver for the life of the run — the expensive step, and the one
//     every earlier step exists to avoid paying for twice (see passports).
//  3. the passport's instrument_type, checked against
//     brokerInstrumentTypes; anything outside share/bond/etf refuses with
//     ErrUnsupportedInstrumentType before any catalog call.
//  4. the catalog, by ISIN (exact) — the catalog is shared across the whole
//     instance, so a human may have entered this instrument by hand
//     already, or another connection may have resolved it first.
//  5. the catalog, by ticker among tradable instruments — a MUCH weaker match
//     than an ISIN, so the row it finds is accepted only if nothing already
//     in hand contradicts it (see refuseContradiction). An accepted row that
//     is missing an ISIN or a FIGI the passport has gets that gap backfilled
//     (see backfillIdentifiers): the catalog is shared, and leaving it empty
//     would cost the next lookup a hit it should have had.
//  6. creation, if nothing above found a row — see createInstrument.
//
// Every path that reaches a resolution — a map hit as much as a freshly
// created row — ends by writing the map (see (*Store).saveMap) with ref's
// CURRENT four identifiers, UNLESS ref carries no instrument_uid at all (see
// the guard in the body below): a hit refreshes the row with what THIS call
// sees, so a drift in any one of the four identifiers, not only the one
// this call happened to match on, is captured rather than left to make the
// row stale. That write leaves alone whatever a resolution does not carry —
// see (*Store).saveMap, which is also the reason a resolution that changes
// nothing writes nothing at all.
func (r *Resolver) Resolve(ctx context.Context, connectionID uuid.UUID, src passportSource, ref InstrumentRef) (Resolved, error) {
	resolved, isin, ticker, err := r.resolveOne(ctx, connectionID, src, ref)
	if err != nil {
		return Resolved{}, err
	}

	// A ref with no instrument_uid at all cannot be written to the map: the
	// table's uniqueness is per (connection_id, instrument_uid), and writing
	// under the empty string would let two unrelated instruments' figi-only
	// refs collide on the same row the next time either was resolved. Such a
	// ref is resolved fresh on every call instead of remembered.
	if ref.InstrumentUID != "" {
		if err := r.store.saveMap(ctx, connectionID, resolved.InstrumentID, ref, isin, ticker); err != nil {
			return Resolved{}, err
		}
	}
	return resolved, nil
}

// resolveOne is steps 1-6 of Resolve's own doc comment: find or create the
// instrument, without touching the map. It also returns the ISIN/ticker the
// resolution settled on, since Resolve's final write needs them and a map
// hit already has them from the same query that found the id (see
// mapMatch) — reading the catalog a second time just to learn them again
// would be exactly the extra round trip checking the map first exists to
// avoid.
func (r *Resolver) resolveOne(ctx context.Context, connectionID uuid.UUID, src passportSource, ref InstrumentRef) (Resolved, string, string, error) {
	m, ok, err := r.lookupMap(ctx, connectionID, ref)
	if err != nil {
		return Resolved{}, "", "", err
	}
	if ok {
		return Resolved{InstrumentID: m.InstrumentID, Type: m.Type}, m.ISIN, m.Ticker, nil
	}

	brief, err := r.passport(ctx, src, ref.InstrumentUID)
	if err != nil {
		return Resolved{}, "", "", err
	}

	typ, ok := brokerInstrumentTypes[brief.InstrumentType]
	if !ok {
		// Debug, not Error, even though this IS a refusal. It is a refusal by
		// design and the owner already sees it: the projection turns it into a
		// visible unparsed entry with this reason on it. A history that trades
		// futures would otherwise write one Error line per operation about a
		// state nobody can act on from a log — the same reasoning marketdata
		// gives for its own routine "the catalog does not hold this" line
		// (internal/marketdata/jobs.go).
		r.log.Debug("tinvest: the broker's instrument type is not one this program accounts for",
			"instrument_type", brief.InstrumentType, "ticker", brief.Ticker, "instrument_uid", brief.UID)
		return Resolved{}, "", "", fmt.Errorf("%w: %s", ErrUnsupportedInstrumentType, brief.InstrumentType)
	}

	inst, err := r.findOrCreate(ctx, src, typ, brief)
	if err != nil {
		return Resolved{}, "", "", err
	}
	return Resolved{InstrumentID: inst.ID, Type: inst.Type}, inst.ISIN, inst.Ticker, nil
}

// lookupMap is step 1: instrument_uid first, figi second. Both are guarded
// against an empty ref field by (*Store).mapByInstrumentUID/mapByFIGI
// themselves, so this only decides the ORDER and stops at the first hit.
func (r *Resolver) lookupMap(ctx context.Context, connectionID uuid.UUID, ref InstrumentRef) (mapMatch, bool, error) {
	m, err := r.store.mapByInstrumentUID(ctx, connectionID, ref.InstrumentUID)
	switch {
	case err == nil:
		return m, true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return mapMatch{}, false, err
	}

	m, err = r.store.mapByFIGI(ctx, connectionID, ref.FIGI)
	switch {
	case err == nil:
		return m, true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return mapMatch{}, false, err
	}

	return mapMatch{}, false, nil
}

// passport returns the broker's instrument passport for uid, calling src at
// most once per uid for the life of this Resolver (decision 5 of the task
// brief: a run's history can carry the same instrument hundreds of times,
// and a hundred identical requests would spend the broker's per-minute
// limit on nothing new).
func (r *Resolver) passport(ctx context.Context, src passportSource, uid string) (InstrumentBrief, error) {
	if brief, ok := r.passports[uid]; ok {
		return brief, nil
	}
	brief, err := src.InstrumentByUID(ctx, uid)
	if err != nil {
		return InstrumentBrief{}, fmt.Errorf("tinvest: instrument passport %s: %w", uid, err)
	}
	r.passports[uid] = brief
	return brief, nil
}

// findOrCreate is steps 4-6 of Resolve's doc comment: the catalog by ISIN,
// then by ticker (checked against the passport, then backfilled), then
// creation.
func (r *Resolver) findOrCreate(ctx context.Context, src passportSource, typ instrument.Type, brief InstrumentBrief) (instrument.Instrument, error) {
	if brief.ISIN != "" {
		inst, err := r.catalog.ByISIN(ctx, brief.ISIN)
		switch {
		case err == nil:
			// The catalog carries no uniqueness on isin, so this is the OLDEST
			// row bearing it (see instrument.Store.ByISIN) and there may be
			// more. Naming the row that was chosen is the whole value of this
			// line to somebody looking at a duplicate later; claiming here how
			// many rows there were would be a sentence this code has no way to
			// know the truth of.
			r.log.Debug("tinvest: instrument matched a catalog row by isin",
				"isin", brief.ISIN, "instrument_id", inst.ID, "instrument_uid", brief.UID)
			return inst, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return instrument.Instrument{}, fmt.Errorf("tinvest: catalog by isin: %w", err)
		}
	}

	if brief.Ticker != "" {
		inst, err := r.catalog.ByTickerTradable(ctx, brief.Ticker)
		switch {
		case err == nil:
			if err := r.refuseContradiction(inst, typ, brief); err != nil {
				return instrument.Instrument{}, err
			}
			return r.backfillIdentifiers(ctx, inst, brief)
		case !errors.Is(err, pgx.ErrNoRows):
			return instrument.Instrument{}, fmt.Errorf("tinvest: catalog by ticker: %w", err)
		}
	}

	return r.createInstrument(ctx, src, typ, brief)
}

// refuseContradiction is the guard on every route that reaches a catalog row
// by TICKER — step 5 above and the re-lookup after a lost ticker race, which
// are the only two, and both of them lead on to backfillIdentifiers.
//
// A ticker is not an identity. Different exchanges assign the same one to
// unrelated companies, and this program's catalog is where those meet — the
// case this rule was written from is the owner's own: AT&T entered by hand
// under ticker "T" (a foreign share, no ISIN recorded — exactly the row
// backfilling exists for), while Т-Технологии trade on MOEX under ticker "T"
// too, with ISIN RU000A107UL4. Without this check the lookup by ISIN misses,
// the lookup by ticker hands back AT&T, and backfillIdentifiers then stamps
// Т-Технологии's ISIN and figi onto AT&T's row — permanently, for every space
// in the instance, with every one of Т-Технологии's trades booked against AT&T
// from then on. If AT&T's ISIN happened to be recorded already, the backfill
// would write nothing and the wrong answer would simply be silent.
//
// WHAT IT COMPARES IS WHAT THIS MOMENT ALREADY HOLDS. Two authoritative
// answers about which paper this is are both in hand here — the broker's
// passport and the catalog row — and the defect was that nothing put them
// next to each other. Two identifiers can settle it:
//
//   - Two non-empty ISINs that differ. An ISIN names one security; two
//     different ones cannot name the same one. Either side missing decides
//     nothing (that is the ordinary case this whole ticker path exists for),
//     so silence is never read as agreement.
//   - Two types that differ. The catalog's type is what every valuation
//     branches on (a bond is priced as a percentage of face, a share is not),
//     so a bond's trades filed against an ETF row are mispriced even when the
//     ticker genuinely is the same string.
//
// It refuses rather than creating a row of its own because it cannot create
// one: at most one tradable row per ticker may exist (migration 0011). The
// refusal becomes a visible unparsed entry naming both sides, which is a
// question only the owner can answer.
func (r *Resolver) refuseContradiction(inst instrument.Instrument, typ instrument.Type, brief InstrumentBrief) error {
	if inst.ISIN != "" && brief.ISIN != "" && inst.ISIN != brief.ISIN {
		r.log.Error("tinvest: refusing to resolve an instrument to a catalog row with a different isin",
			"ticker", brief.Ticker, "instrument_id", inst.ID, "catalog_isin", inst.ISIN,
			"broker_isin", brief.ISIN, "instrument_uid", brief.UID)
		return fmt.Errorf("%w: ticker %s is %s in the catalog and %s at the broker",
			ErrDifferentSecurity, brief.Ticker, inst.ISIN, brief.ISIN)
	}
	if inst.Type != typ {
		r.log.Error("tinvest: refusing to resolve an instrument to a catalog row of another type",
			"ticker", brief.Ticker, "instrument_id", inst.ID, "catalog_type", inst.Type,
			"broker_type", typ, "instrument_uid", brief.UID)
		return fmt.Errorf("%w: ticker %s is a %s in the catalog and a %s at the broker",
			ErrDifferentSecurity, brief.Ticker, inst.Type, typ)
	}
	return nil
}

// backfillIdentifiers fills an ISIN or a FIGI the catalog row is missing
// with what the passport carries, and leaves either field alone if the row
// already has one (the task brief's decision 2: the catalog is shared
// across the whole instance, and an empty ISIN there costs the next
// connection's exact lookup a hit it should have had). A row that already
// has both is returned unchanged, with no write at all.
func (r *Resolver) backfillIdentifiers(ctx context.Context, inst instrument.Instrument, brief InstrumentBrief) (instrument.Instrument, error) {
	var upd instrument.Update
	dirty := false
	if inst.ISIN == "" && brief.ISIN != "" {
		v := brief.ISIN
		upd.ISIN = &v
		dirty = true
	}
	if inst.FIGI == "" && brief.FIGI != "" {
		v := brief.FIGI
		upd.FIGI = &v
		dirty = true
	}
	if !dirty {
		return inst, nil
	}
	updated, err := r.catalog.Update(ctx, inst.ID, upd)
	if err != nil {
		r.log.Error("tinvest: backfilling a catalog row's identifiers failed",
			"instrument_id", inst.ID, "ticker", inst.Ticker, "err", err)
		return instrument.Instrument{}, fmt.Errorf("tinvest: backfill instrument identifiers: %w", err)
	}
	// Info rather than Debug because this writes to the catalog every space in
	// the instance shares, and it is the one write here that a person did not
	// ask for by hand.
	r.log.Info("tinvest: filled in a catalog row's missing identifiers from the broker",
		"instrument_id", inst.ID, "ticker", inst.Ticker,
		"isin", updated.ISIN, "figi", updated.FIGI, "instrument_uid", brief.UID)
	return updated, nil
}

// checkPassport is what a catalog row requires and a passport is not
// guaranteed to carry.
//
// THIS DOOR HAS NO OTHER GUARD. instrument.Store.Create runs no validation of
// its own: name and currency are NOT NULL columns with no CHECK behind them
// (migration 0004), so the empty string satisfies both, and every rule about
// them — a name that is there, a currency shaped like an ISO-4217 code — lives
// in the catalog's HTTP handler (instrument.Handler.handleCreate), which an
// importer does not go through. A passport that arrived with an empty name
// would therefore have created a nameless catalog row, indistinguishable on a
// screen from a real one and impossible to search for; an empty currency would
// have created a row whose every figure is published without a currency on it
// — the exact state migration 0012 and #93 were about, one door further along.
//
// The shape is checked, not the register (see the currency package): "XYZ"
// passes here as it does everywhere else in this program.
func checkPassport(brief InstrumentBrief) error {
	if brief.Name == "" {
		return fmt.Errorf("%w: instrument %s has no name", ErrIncompletePassport, brief.UID)
	}
	if !currency.Valid(brief.Currency) {
		return fmt.Errorf("%w: instrument %s carries currency %q, which is not an ISO-4217 code",
			ErrIncompletePassport, brief.UID, brief.Currency)
	}
	return nil
}

// createInstrument is Resolve's last resort: the broker knows an instrument
// neither this connection's map nor the shared catalog has seen before.
//
// A bond additionally needs its face value, which GetInstrumentBy (brief)
// does not carry at all — see InstrumentBrief's own doc comment — so this
// makes the one further broker call that does, BondNominalByUID, before
// creating the row.
//
// Frozen is always false and is NEVER derived from anything the broker
// sends: the API carries no field meaning "frozen by sanctions" (only a
// depositary-halt flag for a different reason entirely, per the task
// brief), so nothing here could set it truthfully. The owner marks freezing
// by hand.
func (r *Resolver) createInstrument(ctx context.Context, src passportSource, typ instrument.Type, brief InstrumentBrief) (instrument.Instrument, error) {
	if err := checkPassport(brief); err != nil {
		r.log.Error("tinvest: refusing to create a catalog row from an incomplete passport",
			"instrument_uid", brief.UID, "ticker", brief.Ticker, "err", err)
		return instrument.Instrument{}, err
	}

	inst := instrument.Instrument{
		Type:     typ,
		Name:     brief.Name,
		Ticker:   brief.Ticker,
		ISIN:     brief.ISIN,
		FIGI:     brief.FIGI,
		Currency: brief.Currency,
		Frozen:   false,
	}

	if typ == instrument.TypeBond {
		nominal, err := src.BondNominalByUID(ctx, brief.UID)
		if err != nil {
			r.log.Error("tinvest: fetching a bond's nominal failed, the instrument cannot be created",
				"instrument_uid", brief.UID, "ticker", brief.Ticker, "err", err)
			return instrument.Instrument{}, fmt.Errorf("tinvest: bond nominal %s: %w", brief.UID, err)
		}
		faceMinor, err := r.faceValue(nominal, brief)
		if err != nil {
			return instrument.Instrument{}, err
		}
		faceCurrency := nominal.Currency
		inst.FaceValueMinor = &faceMinor
		inst.FaceCurrency = &faceCurrency
	}

	created, err := r.catalog.Create(ctx, inst)
	if errors.Is(err, instrument.ErrTickerTaken) {
		// Someone else created this ticker between findOrCreate's lookup and
		// this insert — another connection's sync running concurrently, or a
		// person entering the same instrument by hand in between (the task
		// brief's decision 4). The row that won the race is the one to use;
		// this does not retry the insert, which would only lose the race
		// again.
		r.log.Warn("tinvest: another writer created this ticker first, taking their catalog row",
			"ticker", brief.Ticker, "instrument_uid", brief.UID)
		found, ferr := r.catalog.ByTickerTradable(ctx, brief.Ticker)
		if ferr != nil {
			return instrument.Instrument{}, fmt.Errorf(
				"tinvest: instrument create lost the ticker race and the re-lookup failed: %w", ferr)
		}
		// The row that won the race is a row reached by TICKER, which is the
		// weak match refuseContradiction exists for, and it leads to the same
		// backfill. The race is in fact the likelier way to meet a stranger
		// here than step 5 is: whoever won may have been a person typing an
		// unrelated paper under this ticker a second earlier.
		if err := r.refuseContradiction(found, typ, brief); err != nil {
			return instrument.Instrument{}, err
		}
		return r.backfillIdentifiers(ctx, found, brief)
	}
	if err != nil {
		r.log.Error("tinvest: creating a catalog row failed",
			"instrument_uid", brief.UID, "ticker", brief.Ticker, "err", err)
		return instrument.Instrument{}, fmt.Errorf("tinvest: create instrument: %w", err)
	}
	// Info, and deliberately the loudest line in this file: this is the one
	// place an import adds a row to the catalog the whole instance shares,
	// without anybody having asked for that row by hand.
	r.log.Info("tinvest: created a catalog row for an instrument the broker knows",
		"instrument_id", created.ID, "type", created.Type, "ticker", created.Ticker,
		"isin", created.ISIN, "figi", created.FIGI, "currency", created.Currency,
		"instrument_uid", brief.UID)
	return created, nil
}

// faceValue turns a bond's nominal into the minor units the catalog stores,
// refusing anything the catalog cannot hold BEFORE the insert rather than
// letting the database say it.
//
// Every rule here is one that migration 0012's CHECK constraint or the
// catalog's HTTP door already states, and neither of those speaks to this
// caller. The constraint refuses the insert with a raw
// "instruments_face_value_sound" violation naming a constraint and no
// instrument (seen, not assumed: with these checks removed, that is verbatim
// what a zero nominal produces here). The upper bound is worse than that,
// because the database does not carry it at all — it is stated only at the
// HTTP door, which this path never goes through, so an unbounded face value
// simply inserts and the account's positions screen answers 500 from then on,
// the failure instrument/http.go's own sweep describes. What this adds is the
// sentence naming which bond and what was wrong with its nominal.
//
// A zero nominal is the case that makes this more than tidiness: it is what a
// broker answering "no nominal for this instrument" looks like once it has
// been parsed into a MoneyValue, and it would otherwise reach the database as
// a plain constraint failure in the middle of a sync.
func (r *Resolver) faceValue(nominal MoneyValue, brief InstrumentBrief) (int64, error) {
	refuse := func(what string) error {
		err := fmt.Errorf("%w: bond %s has a nominal of %s in %q, %s",
			ErrIncompletePassport, brief.UID, nominal.Decimal().String(), nominal.Currency, what)
		r.log.Error("tinvest: refusing to create a bond whose nominal the catalog cannot hold",
			"instrument_uid", brief.UID, "ticker", brief.Ticker, "err", err)
		return err
	}
	if !nominal.Decimal().IsPositive() {
		return 0, refuse("and a face value has to be positive")
	}
	if !currency.Valid(nominal.Currency) {
		return 0, refuse("and a face value's currency has to be an ISO-4217 code")
	}
	faceMinor, err := money.Minor(nominal.Decimal().Mul(minorScale))
	if err != nil {
		return 0, fmt.Errorf("tinvest: bond nominal %s: %w", brief.UID, err)
	}
	if faceMinor > money.MaxAmountMinor {
		return 0, refuse(fmt.Sprintf("and a face value has to be at most %d minor units", money.MaxAmountMinor))
	}
	return faceMinor, nil
}
