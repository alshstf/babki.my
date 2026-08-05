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

// ErrUnsupportedInstrumentType means the broker's instrument_type is not one
// this catalog can hold — see brokerInstrumentTypes for the four it can
// (share, bond, etf, currency). Futures, options, structured bonds and
// everything else outside that set refuse rather than being filed as some
// generic "other" row: an operation against one becomes a visible unparsed
// entry instead of a catalog row nothing in this program can value (the
// task brief's decision 3).
var ErrUnsupportedInstrumentType = errors.New("tinvest: unsupported instrument type")

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
// InstrumentBrief.InstrumentType) to this catalog's Type. Only the four
// types this catalog can value are listed. Absence from this map IS the
// refusal (ErrUnsupportedInstrumentType) — there is deliberately no second
// list of excluded types to keep in step with this one.
var brokerInstrumentTypes = map[string]instrument.Type{
	"share":    instrument.TypeShare,
	"bond":     instrument.TypeBond,
	"etf":      instrument.TypeETF,
	"currency": instrument.TypeCurrency,
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
//     brokerInstrumentTypes; anything outside share/bond/etf/currency
//     refuses with ErrUnsupportedInstrumentType before any catalog call.
//  4. the catalog, by ISIN (exact) — the catalog is shared across the whole
//     instance, so a human may have entered this instrument by hand
//     already, or another connection may have resolved it first.
//  5. the catalog, by ticker among tradable instruments — and if the row
//     found this way is missing an ISIN or a FIGI the passport has, that
//     gap is backfilled (see backfillIdentifiers): the catalog is shared,
//     and leaving it empty would cost the next lookup a hit it should have
//     had.
//  6. creation, if nothing above found a row — see createInstrument.
//
// Every path that reaches a resolution — a map hit as much as a freshly
// created row — ends by writing the map (see (*Store).saveMap) with ref's
// CURRENT four identifiers, UNLESS ref carries no instrument_uid at all (see
// the guard in the body below): a hit refreshes the row with what THIS call
// sees, so a drift in any one of the four identifiers, not only the one
// this call happened to match on, is captured rather than left to make the
// row stale.
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
// then by ticker (backfilling a found row's empty ISIN/FIGI), then
// creation.
func (r *Resolver) findOrCreate(ctx context.Context, src passportSource, typ instrument.Type, brief InstrumentBrief) (instrument.Instrument, error) {
	if brief.ISIN != "" {
		inst, err := r.catalog.ByISIN(ctx, brief.ISIN)
		switch {
		case err == nil:
			return inst, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return instrument.Instrument{}, fmt.Errorf("tinvest: catalog by isin: %w", err)
		}
	}

	if brief.Ticker != "" {
		inst, err := r.catalog.ByTickerTradable(ctx, brief.Ticker)
		switch {
		case err == nil:
			return r.backfillIdentifiers(ctx, inst, brief)
		case !errors.Is(err, pgx.ErrNoRows):
			return instrument.Instrument{}, fmt.Errorf("tinvest: catalog by ticker: %w", err)
		}
	}

	return r.createInstrument(ctx, src, typ, brief)
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
		return instrument.Instrument{}, fmt.Errorf("tinvest: backfill instrument identifiers: %w", err)
	}
	return updated, nil
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
			return instrument.Instrument{}, fmt.Errorf("tinvest: bond nominal %s: %w", brief.UID, err)
		}
		faceMinor, err := money.Minor(nominal.Decimal().Mul(minorScale))
		if err != nil {
			return instrument.Instrument{}, fmt.Errorf("tinvest: bond nominal %s: %w", brief.UID, err)
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
		found, ferr := r.catalog.ByTickerTradable(ctx, brief.Ticker)
		if ferr != nil {
			return instrument.Instrument{}, fmt.Errorf(
				"tinvest: instrument create lost the ticker race and the re-lookup failed: %w", ferr)
		}
		return r.backfillIdentifiers(ctx, found, brief)
	}
	if err != nil {
		return instrument.Instrument{}, fmt.Errorf("tinvest: create instrument: %w", err)
	}
	return created, nil
}

// mapMatch is one hit of the connection's instrument map, joined against the
// catalog's own type/isin/ticker columns (see (*Store).mapByInstrumentUID).
// The join exists so a hit answers Resolve fully — id, type, and what
// Resolve's own final write needs — without a second round trip through
// instrumentCatalog: the entire point of checking this table before the
// broker's passport is to skip that call, and paying for a catalog read here
// would give back half the saving.
type mapMatch struct {
	InstrumentID uuid.UUID
	Type         instrument.Type
	ISIN, Ticker string
}

// mapByInstrumentUID and mapByFIGI are the resolver's two lookups into
// tinvest_instrument_map, tried in that order by lookupMap. Both return
// pgx.ErrNoRows on a miss (this package's usual wrapping — see the note
// above scanConnection in store.go — leaves errors.Is working).
//
// A row can never dangle: instrument_id references instruments(id) ON
// DELETE CASCADE (migration 0014), so a match here is always joinable.
func (s *Store) mapByInstrumentUID(ctx context.Context, connectionID uuid.UUID, instrumentUID string) (mapMatch, error) {
	if instrumentUID == "" {
		// An empty instrument_uid is not something the table's own
		// uniqueness (connection_id, instrument_uid) could ever narrow to
		// one row on purpose — see Resolve's own guard on writing one.
		// Refusing before the query keeps that from silently matching
		// whatever row a previous empty-uid write happened to leave.
		return mapMatch{}, fmt.Errorf("tinvest: instrument map by instrument_uid: %w", pgx.ErrNoRows)
	}
	var m mapMatch
	err := s.pool.QueryRow(ctx, `
		SELECT im.instrument_id, i.type, i.isin, i.ticker
		FROM tinvest_instrument_map im
		JOIN instruments i ON i.id = im.instrument_id
		WHERE im.connection_id = $1 AND im.instrument_uid = $2`,
		connectionID, instrumentUID).Scan(&m.InstrumentID, &m.Type, &m.ISIN, &m.Ticker)
	if err != nil {
		return mapMatch{}, fmt.Errorf("tinvest: instrument map by instrument_uid: %w", err)
	}
	return m, nil
}

// mapByFIGI is lookupMap's fallback for when the operation's instrument_uid
// is not one this connection recognizes: an operation that predates
// instrument_uid, or one where it has drifted since the last sync, may still
// carry a figi this connection has already resolved.
//
// The index behind this query (tinvest_map_figi_idx) is not unique, so a
// connection can hold more than one row for one figi — most plausibly an
// older row an instrument_uid has since drifted away from. The most
// recently updated one is taken, on the reasoning that a later write is the
// more likely one to still be right about what this figi means today.
func (s *Store) mapByFIGI(ctx context.Context, connectionID uuid.UUID, figi string) (mapMatch, error) {
	if figi == "" {
		// Every row this connection has never set a figi for shares the
		// empty string; picking "most recently updated" among them would
		// return an unrelated instrument, not "no match" — see the same
		// guard on mapByInstrumentUID above.
		return mapMatch{}, fmt.Errorf("tinvest: instrument map by figi: %w", pgx.ErrNoRows)
	}
	var m mapMatch
	err := s.pool.QueryRow(ctx, `
		SELECT im.instrument_id, i.type, i.isin, i.ticker
		FROM tinvest_instrument_map im
		JOIN instruments i ON i.id = im.instrument_id
		WHERE im.connection_id = $1 AND im.figi = $2
		ORDER BY im.updated_at DESC
		LIMIT 1`,
		connectionID, figi).Scan(&m.InstrumentID, &m.Type, &m.ISIN, &m.Ticker)
	if err != nil {
		return mapMatch{}, fmt.Errorf("tinvest: instrument map by figi: %w", err)
	}
	return m, nil
}

// saveMap records instrumentID as the answer for ref.InstrumentUID, along
// with every other identifier ref carries and the catalog's own isin/ticker
// for it — replacing whatever this connection last wrote under that same
// instrument_uid (ON CONFLICT (connection_id, instrument_uid), migration
// 0014's own uniqueness). It is called on every resolution Resolve
// completes, a map hit as much as a freshly created row, so that a drift in
// figi/position_uid/asset_uid ALONE — with instrument_uid unchanged, and so
// still hitting mapByInstrumentUID on its own — is still captured rather
// than left on a row nothing ever revisits again.
//
// Callers must not call this with ref.InstrumentUID == "" — see Resolve's
// own guard, which is the only caller and never does.
func (s *Store) saveMap(ctx context.Context, connectionID, instrumentID uuid.UUID, ref InstrumentRef, isin, ticker string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO tinvest_instrument_map
			(connection_id, instrument_id, figi, instrument_uid, position_uid, asset_uid, isin, ticker)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (connection_id, instrument_uid) DO UPDATE SET
			instrument_id = EXCLUDED.instrument_id,
			figi          = EXCLUDED.figi,
			position_uid  = EXCLUDED.position_uid,
			asset_uid     = EXCLUDED.asset_uid,
			isin          = EXCLUDED.isin,
			ticker        = EXCLUDED.ticker,
			updated_at    = now()`,
		connectionID, instrumentID, ref.FIGI, ref.InstrumentUID, ref.PositionUID, ref.AssetUID, isin, ticker)
	if err != nil {
		return fmt.Errorf("tinvest: save instrument map: %w", err)
	}
	return nil
}
