package tinvest

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
	"babki.my/babki/internal/platform/currency"
	"babki.my/babki/internal/platform/money"
)

// InstrumentRef is the broker's identifiers for one instrument as one operation
// carried them. They drift independently (figi and instrument_uid on old
// operations have changed); Resolve survives any one changing.
type InstrumentRef struct {
	InstrumentUID, FIGI, PositionUID, AssetUID string
	// Ticker is what the operation called the paper, used only when the broker
	// has forgotten the instrument (see resolveOne).
	Ticker string
}

// Resolved is what Resolve found: the catalog id, type and currency. Currency
// is the catalog's (the passport for a row this import created), which an entry
// about the paper rather than a payment must use (see
// projectSecuritiesTransfer).
type Resolved struct {
	InstrumentID uuid.UUID
	Type         instrument.Type
	Currency     string
}

// ErrUnsupportedInstrumentType: the broker's instrument_type is not one this
// program accounts for (share, bond, etf; see brokerInstrumentTypes). The
// operation becomes a visible unparsed row rather than an unvaluable catalog
// row.
var ErrUnsupportedInstrumentType = errors.New("tinvest: unsupported instrument type")

// ErrDifferentSecurity: the catalog row found by the broker's ticker is a
// different security, by ISIN or type. Unlike ErrUnsupportedInstrumentType it
// means the catalog and the passport disagree and one must be corrected. Reached
// only on the ticker-race path, where a second row is not available (see
// contradicts).
var ErrDifferentSecurity = errors.New("tinvest: the catalog row with this ticker is a different security")

// ErrIncompletePassport: the broker's passport lacks what a catalog row needs
// (see checkPassport and the bond nominal in createInstrument).
var ErrIncompletePassport = errors.New("tinvest: broker passport is missing what a catalog row requires")

// instrumentCatalog is the part of instrument.Store the resolver needs. Tests
// wrap the real store, since tinvest_instrument_map has a foreign key to
// instruments.
type instrumentCatalog interface {
	ByISIN(ctx context.Context, isin string) (instrument.Instrument, error)
	ByTickerTradable(ctx context.Context, ticker string) (instrument.Instrument, error)
	Create(ctx context.Context, inst instrument.Instrument) (instrument.Instrument, error)
	Update(ctx context.Context, id uuid.UUID, upd instrument.Update) (instrument.Instrument, error)
	ByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]instrument.Instrument, error)
}

// passportSource is the broker calls that identify an instrument nothing local
// knows. A Resolve parameter, not a field, because it is bound to one
// connection's token while one Resolver serves a whole run. BondNominalByUID is
// here because GetInstrumentBy carries no bond nominal (checked live) and a bond
// cannot be created without one.
type passportSource interface {
	InstrumentByUID(ctx context.Context, uid string) (InstrumentBrief, error)
	BondNominalByUID(ctx context.Context, uid string) (MoneyValue, error)
	currencySource
}

// currencySource is the one call ResolveCurrency needs.
type currencySource interface {
	CurrencyNominalByUID(ctx context.Context, uid string) (MoneyValue, error)
}

// brokerInstrumentTypes maps the broker's instrument_type to the catalog's
// Type; absence is the refusal (ErrUnsupportedInstrumentType).
//
// Currency is deliberately absent. A currency trade becomes conversion entries,
// which name no instrument (the engine skips conversions), and needs only
// ResolveCurrency. Accepted here, a currency could never be found by ISIN or
// tradable ticker and would be created, and the unique ticker index does not
// cover currencies (migration 0011): two connections left two rows for one
// currency when this was tried.
var brokerInstrumentTypes = map[string]instrument.Type{
	"share": instrument.TypeShare,
	"bond":  instrument.TypeBond,
	"etf":   instrument.TypeETF,
}

// minorScale is the two-decimal minor-unit factor every money figure uses,
// bond nominals in any currency included.
var minorScale = decimal.New(1, 2)

// Resolver turns a broker instrument reference into this instance's catalog id,
// creating a catalog row the first time the instrument is seen anywhere. One per
// sync run, reused across its operations. Not safe for concurrent use.
type Resolver struct {
	store     *Store
	catalog   instrumentCatalog
	log       *slog.Logger
	passports map[string]InstrumentBrief
	// currencies memoizes ResolveCurrency for the run.
	currencies map[string]TradedCurrency
	// forgotten remembers the broker's "no such instrument" per call and uid for
	// the run: a forgotten paper is one a history is full of operations on.
	// Other failures are asked again.
	forgotten map[forgottenKey]error
	// rates proves what a forgotten currency pair trades (see
	// ResolveCurrency); nil disables that fallback.
	rates rateOracle
}

// rateOracle is the official rate of one currency against another on a
// day.
type rateOracle interface {
	Rate(ctx context.Context, from, to string, on time.Time) (decimal.Decimal, time.Time, error)
}

// NewResolver builds a Resolver over the connection map (store) and the
// shared catalog.
func NewResolver(store *Store, catalog instrumentCatalog, log *slog.Logger) *Resolver {
	if log == nil {
		log = slog.Default()
	}
	return &Resolver{
		store: store, catalog: catalog, log: log,
		passports: map[string]InstrumentBrief{}, currencies: map[string]TradedCurrency{},
		forgotten: map[forgottenKey]error{},
	}
}

// WithRates enables working out a forgotten currency pair (see
// ResolveCurrency).
func (r *Resolver) WithRates(rates rateOracle) *Resolver {
	r.rates = rates
	return r
}

// Resolve turns one broker instrument reference into a catalog id and type,
// going no further than it must:
//
//  1. tinvest_instrument_map by instrument_uid, then figi: one query, no broker.
//  2. the broker's passport, cached for the run.
//  3. its instrument_type against brokerInstrumentTypes.
//  4. the catalog by ISIN, shared instance-wide.
//  5. the catalog by tradable ticker, a weak match taken only if nothing
//     contradicts it (contradicts), then backfilled (backfillIdentifiers).
//  6. creation (createInstrument).
//
// Every resolution then writes the map with ref's current identifiers, unless ref
// has no instrument_uid, so a drift in any of them is captured; saveMap skips the
// write when nothing changed.
func (r *Resolver) Resolve(ctx context.Context, connectionID uuid.UUID, src passportSource, ref InstrumentRef) (Resolved, error) {
	resolved, isin, ticker, listingCurrency, err := r.resolveOne(ctx, connectionID, src, ref)
	if err != nil {
		return Resolved{}, err
	}

	// No instrument_uid: the map is unique per (connection, uid), and an
	// empty key would let unrelated figi-only refs collide. Resolved fresh each
	// time instead.
	if ref.InstrumentUID != "" {
		if err := r.store.saveMap(ctx, connectionID, resolved.InstrumentID, ref, isin, ticker, listingCurrency); err != nil {
			return Resolved{}, err
		}
	}
	return resolved, nil
}

// resolveOne is steps 1-6 without writing the map. It returns the ISIN and
// ticker for Resolve's write, which a map hit already has.
func (r *Resolver) resolveOne(ctx context.Context, connectionID uuid.UUID, src passportSource, ref InstrumentRef) (resolved Resolved, isin, ticker, listingCurrency string, err error) {
	m, ok, err := r.lookupMap(ctx, connectionID, ref)
	if err != nil {
		return Resolved{}, "", "", "", err
	}
	if ok {
		// No passport, so no listing currency: "" leaves the stored one alone
		// (see saveMap).
		return Resolved{InstrumentID: m.InstrumentID, Type: m.Type, Currency: m.Currency}, m.ISIN, m.Ticker, "", nil
	}

	brief, err := r.passport(ctx, src, ref.InstrumentUID)
	if err != nil {
		// The broker forgets papers it once traded (a fund wound up, a company
		// redomiciled): the passport answers 404 for good. For those it puts the
		// ISIN in the operation's ticker field ("IE00BD3QJN10", "RU000A101X68",
		// the FinEx funds), and an exact catalog match by ISIN is proof. Not by
		// figi: it is reissued per listing (TCS20A101X68 vs TCS33A101X68).
		// Nothing is created: a paper the catalog does not know stays unresolved.
		if errors.Is(err, ErrInstrumentNotFound) && ref.Ticker != "" {
			if inst, found, ferr := r.catalogByISIN(ctx, ref.Ticker); ferr != nil {
				return Resolved{}, "", "", "", ferr
			} else if found {
				return Resolved{InstrumentID: inst.ID, Type: inst.Type, Currency: inst.Currency},
					inst.ISIN, inst.Ticker, "", nil
			}
		}
		return Resolved{}, "", "", "", err
	}

	typ, ok := brokerInstrumentTypes[brief.InstrumentType]
	if !ok {
		// Debug: a designed refusal the owner sees as an unparsed row; an Error
		// per futures operation would flood the log.
		r.log.Debug("tinvest: the broker's instrument type is not one this program accounts for",
			"instrument_type", brief.InstrumentType, "ticker", brief.Ticker, "instrument_uid", brief.UID)
		return Resolved{}, "", "", "", fmt.Errorf("%w: %s", ErrUnsupportedInstrumentType, brief.InstrumentType)
	}

	inst, err := r.findOrCreate(ctx, src, typ, brief)
	if err != nil {
		return Resolved{}, "", "", "", err
	}
	// brief.Currency: the listing's currency, which a row found by ISIN may
	// not share (one paper, two venues).
	return Resolved{InstrumentID: inst.ID, Type: inst.Type, Currency: inst.Currency}, inst.ISIN, inst.Ticker, brief.Currency, nil
}

// lookupMap is step 1: instrument_uid, then figi.
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

// TradedCurrency is what a currency instrument trades: the ISO code acquired
// and how many of its units one instrument unit buys, both from the nominal. A
// currency trade row names only its payment currency, and a unit is not always
// one (Kyrgyz som: 100, Uzbek sum: 10 000; live, 2026-08-05).
type TradedCurrency struct {
	Code           string
	NominalPerUnit decimal.Decimal
}

// ResolveCurrency answers what a currency instrument trades, from CurrencyBy
// (GetInstrumentBy has no nominal), memoized for the run. It does not touch the
// catalog: conversions name no instrument. ErrIncompletePassport when the nominal
// lacks a currency or a positive value.
func (r *Resolver) ResolveCurrency(ctx context.Context, src currencySource, uid string, hint CurrencyHint) (TradedCurrency, error) {
	if known, ok := r.currencies[uid]; ok {
		return known, nil
	}
	nominal, err := remembered(r, forgottenCurrency, uid, func() (MoneyValue, error) {
		return src.CurrencyNominalByUID(ctx, uid)
	})
	if errors.Is(err, ErrInstrumentNotFound) {
		traded, ok, ferr := r.currencyFromHint(ctx, hint)
		if ferr != nil {
			return TradedCurrency{}, ferr
		}
		if ok {
			r.currencies[uid] = traded
			return traded, nil
		}
	}
	if err != nil {
		return TradedCurrency{}, err
	}
	code := strings.ToUpper(nominal.Currency)
	per := nominal.Decimal()
	if code == "" || !per.IsPositive() {
		return TradedCurrency{}, fmt.Errorf("%w: currency instrument %s has a nominal of %s %q",
			ErrIncompletePassport, uid, per, nominal.Currency)
	}
	traded := TradedCurrency{Code: code, NominalPerUnit: per}
	r.currencies[uid] = traded
	return traded, nil
}

// CurrencyHint is what the operation says about its pair: the instrument's
// name, the rouble price per unit and the day.
type CurrencyHint struct {
	Ticker       string
	Settlement   string
	PricePerUnit decimal.Decimal
	On           time.Time
}

// tradedFromTickerBand is how far a trade's price may sit from the official
// rate that day and still be the same currency, either way. A factor of two
// separates nominal misreadings (100 or 10 000) from legitimate market spread
// (tens of percent in March 2022).
const tradedFromTickerBand = 2

// currencyFromHint works out what a forgotten pair trades (a delisted dollar or
// euro pair; two dozen in the owner's history). The ticker's first three letters
// (USD000UTSTOM, EUR_RUB__TOM) are the hypothesis; the trade price agreeing with
// that day's official rate is the proof. A per-hundred pair misses a hundredfold;
// a name that is not a currency (GLDRUB_TOM) has no rate. The proven nominal is 1.
// ok=false means not settled here.
func (r *Resolver) currencyFromHint(ctx context.Context, hint CurrencyHint) (TradedCurrency, bool, error) {
	if r.rates == nil || len(hint.Ticker) < 3 || !hint.PricePerUnit.IsPositive() || hint.On.IsZero() {
		return TradedCurrency{}, false, nil
	}
	code := strings.ToUpper(hint.Ticker[:3])
	if !currency.Valid(code) || code == hint.Settlement {
		return TradedCurrency{}, false, nil
	}
	official, _, err := r.rates.Rate(ctx, code, hint.Settlement, hint.On)
	if err != nil {
		// A rate not held yet is not a refusal: the backfill may bring it.
		r.log.Debug("tinvest: no official rate to check a forgotten currency pair against",
			"ticker", hint.Ticker, "code", code, "on", hint.On.Format(time.DateOnly), "err", err)
		return TradedCurrency{}, false, nil
	}
	if !official.IsPositive() {
		return TradedCurrency{}, false, nil
	}
	ratio := hint.PricePerUnit.Div(official)
	band := decimal.NewFromInt(tradedFromTickerBand)
	if ratio.GreaterThan(band) || ratio.LessThan(decimal.NewFromInt(1).Div(band)) {
		r.log.Warn("tinvest: a forgotten currency pair trades too far from the official rate to be read from its name",
			"ticker", hint.Ticker, "code", code, "on", hint.On.Format(time.DateOnly),
			"price_per_unit", hint.PricePerUnit.String(), "official", official.String())
		return TradedCurrency{}, false, nil
	}
	r.log.Info("tinvest: worked out a forgotten currency pair from its name, checked against the official rate",
		"ticker", hint.Ticker, "code", code, "on", hint.On.Format(time.DateOnly),
		"price_per_unit", hint.PricePerUnit.String(), "official", official.String())
	return TradedCurrency{Code: code, NominalPerUnit: decimal.NewFromInt(1)}, true, nil
}

// passport returns uid's instrument passport, asking src at most once per uid
// for the Resolver's life: a history can name one instrument hundreds of
// times, and the broker's limit is per minute.
func (r *Resolver) passport(ctx context.Context, src passportSource, uid string) (InstrumentBrief, error) {
	if brief, ok := r.passports[uid]; ok {
		return brief, nil
	}
	brief, err := remembered(r, forgottenPassport, uid, func() (InstrumentBrief, error) {
		brief, err := src.InstrumentByUID(ctx, uid)
		if err != nil {
			return InstrumentBrief{}, fmt.Errorf("tinvest: instrument passport %s: %w", uid, err)
		}
		return brief, nil
	})
	if err != nil {
		return InstrumentBrief{}, err
	}
	r.passports[uid] = brief
	return brief, nil
}

// forgottenKey is one broker call about one uid.
type forgottenKey struct {
	call string
	uid  string
}

const (
	forgottenPassport = "passport"
	forgottenCurrency = "currency"
)

// remembered makes a broker call about uid unless the broker already said
// this run that it has no such instrument (Resolver.forgotten).
func remembered[T any](r *Resolver, call, uid string, ask func() (T, error)) (T, error) {
	key := forgottenKey{call, uid}
	if err, ok := r.forgotten[key]; ok {
		var zero T
		return zero, err
	}
	v, err := ask()
	if errors.Is(err, ErrInstrumentNotFound) {
		r.forgotten[key] = err
	}
	return v, err
}

// catalogByISIN looks a row up by ISIN, telling "none" from a failure.
func (r *Resolver) catalogByISIN(ctx context.Context, isin string) (instrument.Instrument, bool, error) {
	inst, err := r.catalog.ByISIN(ctx, isin)
	switch {
	case err == nil:
		return inst, true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return instrument.Instrument{}, false, nil
	default:
		return instrument.Instrument{}, false, err
	}
}

// findOrCreate is steps 4-6: by ISIN, by ticker (checked, then
// backfilled), then creation.
func (r *Resolver) findOrCreate(ctx context.Context, src passportSource, typ instrument.Type, brief InstrumentBrief) (instrument.Instrument, error) {
	if brief.ISIN != "" {
		inst, err := r.catalog.ByISIN(ctx, brief.ISIN)
		switch {
		case err == nil:
			// The oldest row with this ISIN (ByISIN); there may be more, and naming
			// the chosen one is what helps later.
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
			// A contradiction leads to a row of its own: since migration 0020 two
			// papers may share a ticker (AT&T and Т-Технологии are both "T").
			if r.contradicts(inst, typ, brief) {
				r.log.Info("tinvest: the catalog row under this ticker is a different security, creating a row of our own",
					"ticker", brief.Ticker, "catalog_isin", inst.ISIN, "broker_isin", brief.ISIN,
					"catalog_type", inst.Type, "instrument_uid", brief.UID)
				break
			}
			return r.backfillIdentifiers(ctx, inst, brief)
		case !errors.Is(err, pgx.ErrNoRows):
			return instrument.Instrument{}, fmt.Errorf("tinvest: catalog by ticker: %w", err)
		}
	}

	return r.createInstrument(ctx, src, typ, brief)
}

// contradicts reports whether a catalog row reached by ticker is a different
// security from the passport: two non-empty ISINs that differ, or two types that
// differ (valuation branches on type). Missing ISINs decide nothing.
//
// A ticker is not an identity: AT&T was entered by hand as "T" without an ISIN,
// and Т-Технологии trade on MOEX as "T" (RU000A107UL4). Accepting the ticker
// match would stamp Т-Технологии's identifiers onto AT&T for every space.
//
// Step 5 answers a contradiction with a new row. The re-lookup after a lost
// ticker race cannot (the ticker went to a row without an ISIN), so it refuses
// with ErrDifferentSecurity, naming both sides.
func (r *Resolver) contradicts(inst instrument.Instrument, typ instrument.Type, brief InstrumentBrief) bool {
	return inst.ISIN != "" && brief.ISIN != "" && inst.ISIN != brief.ISIN || inst.Type != typ
}

// refuseContradiction is contradicts with the refusal attached.
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

// backfillIdentifiers fills a missing ISIN or FIGI on the row from the
// passport, leaving existing ones alone, so the next exact lookup hits. A row
// with both is returned unchanged.
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
	// Info: a write to the shared catalog nobody asked for by hand.
	r.log.Info("tinvest: filled in a catalog row's missing identifiers from the broker",
		"instrument_id", inst.ID, "ticker", inst.Ticker,
		"isin", updated.ISIN, "figi", updated.FIGI, "instrument_uid", brief.UID)
	return updated, nil
}

// checkPassport is what a catalog row needs and a passport may lack.
// instrument.Store.Create validates nothing (name and currency are NOT NULL with
// no CHECK), and the rules live in the catalog's HTTP handler, which an importer
// bypasses: an empty name or currency would create an unsearchable or
// currency-less row. The currency shape is checked, not the ISO register.
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

// createInstrument creates a row for an instrument nothing local knows. A
// bond also needs BondNominalByUID. Frozen is always false: the broker has no
// sanctions-freeze field; the owner marks it by hand.
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
	if errors.Is(err, instrument.ErrISINTaken) {
		// Another writer created the paper first, colliding on the ISIN (the
		// identity since migration 0020), so there is no stranger to refuse.
		r.log.Warn("tinvest: another writer created this security first, taking their catalog row",
			"isin", brief.ISIN, "instrument_uid", brief.UID)
		found, ferr := r.catalog.ByISIN(ctx, brief.ISIN)
		if ferr != nil {
			return instrument.Instrument{}, fmt.Errorf(
				"tinvest: instrument create lost the isin race and the re-lookup failed: %w", ferr)
		}
		return r.backfillIdentifiers(ctx, found, brief)
	}
	if errors.Is(err, instrument.ErrTickerTaken) {
		// Another writer created this ticker first (a concurrent sync or a
		// person); use their row rather than retry.
		r.log.Warn("tinvest: another writer created this ticker first, taking their catalog row",
			"ticker", brief.Ticker, "instrument_uid", brief.UID)
		found, ferr := r.catalog.ByTickerTradable(ctx, brief.Ticker)
		if ferr != nil {
			return instrument.Instrument{}, fmt.Errorf(
				"tinvest: instrument create lost the ticker race and the re-lookup failed: %w", ferr)
		}
		// The race winner was reached by ticker, so it is checked like step 5;
		// a person typing an unrelated paper is the likelier stranger here.
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
	// Info: the one place an import adds a row to the shared catalog.
	r.log.Info("tinvest: created a catalog row for an instrument the broker knows",
		"instrument_id", created.ID, "type", created.Type, "ticker", created.Ticker,
		"isin", created.ISIN, "figi", created.FIGI, "currency", created.Currency,
		"instrument_uid", brief.UID)
	return created, nil
}

// faceValue turns a bond's nominal into stored minor units, refusing what the
// catalog cannot hold before the insert: zero (a "no nominal" answer, which would
// fail migration 0012's CHECK mid-sync with no bond named), and anything over the
// HTTP door's upper bound, which the database does not enforce and which breaks
// the positions screen. Nothing is rounded (see minorFromDecimal): a nominal finer
// than a minor unit is refused, and rounding could turn a tiny one into zero.
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
	minor := nominal.Decimal().Mul(minorScale)
	if !minor.Equal(minor.Truncate(0)) {
		return 0, refuse("and it is finer than a minor unit, which this program does not round away")
	}
	faceMinor, err := money.Minor(minor)
	if err != nil {
		return 0, fmt.Errorf("tinvest: bond nominal %s: %w", brief.UID, err)
	}
	if faceMinor > money.MaxAmountMinor {
		return 0, refuse(fmt.Sprintf("and a face value has to be at most %d minor units", money.MaxAmountMinor))
	}
	return faceMinor, nil
}
