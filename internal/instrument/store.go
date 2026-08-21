package instrument

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/db"
)

// pgUniqueViolation is the SQLSTATE code Postgres returns for a unique
// constraint violation; instrumentsTickerUnique names the one this package can
// trip (migration 0011).
const (
	pgUniqueViolation       = "23505"
	instrumentsTickerUnique = "instruments_ticker_uniq"
	instrumentsISINUnique   = "instruments_isin_uniq"
)

// ErrTickerTaken means another instrument already carries this ticker. The
// catalog holds at most one instrument per ticker among shares, bonds and
// funds, because the quotes job looks instruments up by ticker: a second row
// under the same one would simply never be priced (see marketdata.quotesWorker
// and migration 0011). Instruments no price is fetched for — crypto, metals,
// currencies, custom holdings, and anything with no ticker at all — are outside
// that index and so outside this error; two of them may share a ticker.
//
// Wrapping family.ErrValidation is what turns it into a 400 saying which rule
// was broken, instead of the "internal error" that an unmapped unique violation
// falls through to. 400 and not 409 because of the contract (api/openapi.yaml):
// POST /api/v1/instruments declares 400 and 403, the PATCH beside it declares
// 400, 403 and 404, and 409 appears on neither — so it is not among the answers
// either of the two writes below is allowed to give.
//
// The convention next door says 409, and this is deferred rather than in
// disagreement with it: family.ErrUsernameTaken is deliberately NOT an
// ErrValidation — "the input itself is well-formed; the conflict is with
// existing state, so it maps to 409 rather than 400" — and a taken ticker is
// the same shape of thing. Aligning it means changing the contract and
// regenerating the client, which is scope this change does not carry. Nothing
// visible turns on it today: the create dialog is the only screen that reaches
// either write, and it renders one generic message whatever comes back.
var ErrTickerTaken = fmt.Errorf("%w: ticker already belongs to another instrument", family.ErrValidation)

// ErrISINTaken means another instrument already carries this ISIN.
//
// AN ISIN IDENTIFIES A SECURITY WORLDWIDE, so two rows under one are the same
// paper entered twice — and unlike a shared ticker, that is true of every
// instrument type there is. It became a rule of its own when the ticker stopped
// being one (migration 0020): the ticker's uniqueness had been standing in for
// this, catching two writers who resolved one paper at the same moment, and
// moving identity to the ISIN without moving that protection would have left the
// race to produce duplicates in silence.
//
// The same 400 as ErrTickerTaken, for the same contract reason recorded there.
var ErrISINTaken = fmt.Errorf("%w: isin already belongs to another instrument", family.ErrValidation)

// wrapTickerConflict maps a unique_violation on the ticker index to
// ErrTickerTaken. Every other error passes through untouched — the same shape
// as family.wrapUsernameConflict and operation.mapWriteError, which translate
// exactly the constraints their own writes can trip and nothing else. The shape
// only: the sentinel this one produces is a 400 while family's is a 409, for
// the reason recorded on ErrTickerTaken above.
func wrapTickerConflict(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgUniqueViolation {
		return err
	}
	switch pgErr.ConstraintName {
	case instrumentsTickerUnique:
		return ErrTickerTaken
	case instrumentsISINUnique:
		return ErrISINTaken
	}
	return err
}

type Store struct{ db db.Executor }

func NewStore(x db.Executor) *Store { return &Store{db: x} }

const cols = `id, type, name, ticker, isin, figi, currency,
	face_value_minor, face_currency, frozen, created_at, updated_at`

func scan(row pgx.Row) (Instrument, error) {
	var i Instrument
	err := row.Scan(&i.ID, &i.Type, &i.Name, &i.Ticker, &i.ISIN, &i.FIGI,
		&i.Currency, &i.FaceValueMinor, &i.FaceCurrency, &i.Frozen,
		&i.CreatedAt, &i.UpdatedAt)
	return i, err
}

func (s *Store) Create(ctx context.Context, inst Instrument) (Instrument, error) {
	created, err := scan(s.db.QueryRow(ctx, `
		INSERT INTO instruments (type, name, ticker, isin, figi, currency,
			face_value_minor, face_currency, frozen)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+cols,
		inst.Type, inst.Name, inst.Ticker, inst.ISIN, inst.FIGI,
		inst.Currency, inst.FaceValueMinor, inst.FaceCurrency, inst.Frozen))
	if err != nil {
		return Instrument{}, wrapTickerConflict(err)
	}
	return created, nil
}

func (s *Store) ByID(ctx context.Context, id uuid.UUID) (Instrument, error) {
	return scan(s.db.QueryRow(ctx,
		`SELECT `+cols+` FROM instruments WHERE id = $1`, id))
}

// ByISIN finds the instrument whose isin matches exactly — unlike Search,
// which finds a fragment via ILIKE. It exists for a caller (the T-Invest
// resolver) that has one authoritative ISIN from a broker and needs the one
// catalog row it names, not every row that happens to contain it.
//
// The catalog now carries uniqueness on isin (migration 0020), so at most one
// row can match and the ordering below decides nothing. It is kept because it
// once did: duplicates were enterable by hand until that migration, which
// refuses to upgrade a database still holding any. Should one ever arrive by
// another door, the OLDEST row (lowest created_at, ties broken by id) is what
// this hands back — the original entry rather than a later copy, and a
// deterministic answer either way instead of whichever row Postgres returned
// first.
//
// isin == "" returns pgx.ErrNoRows without querying at all, rather than
// running the same comparison against an empty string. isin has no NOT
// NULL/non-empty constraint, so `WHERE isin = ”` would match every
// instrument nobody has ever set one on and hand back whichever is oldest —
// a plausible-looking instrument that answers nothing about the empty ISIN
// the caller asked for. Refusing before the query keeps that state
// unreachable instead of silently plausible.
func (s *Store) ByISIN(ctx context.Context, isin string) (Instrument, error) {
	if isin == "" {
		return Instrument{}, pgx.ErrNoRows
	}
	return scan(s.db.QueryRow(ctx,
		`SELECT `+cols+` FROM instruments WHERE isin = $1 ORDER BY created_at, id LIMIT 1`, isin))
}

// ByTickerTradable finds the tradable instrument (share, bond, or etf; see
// ListTradable) whose ticker matches exactly. It exists for the same reason
// ByISIN does: a caller with one authoritative ticker needs the one row it
// names.
//
// AT MOST ONE ROW CAN EVER MATCH — the partial unique index behind
// ErrTickerTaken (migration 0011) covers exactly the rows ListTradable
// returns (see TestUniqueTickerCoversExactlyTheRowsListTradableReturns), and
// this method's WHERE is that same set, so no ORDER BY/LIMIT tie-break is
// needed the way ByISIN's is. That sentence is an argument about a filter,
// which is the shape that stops being true without anything failing: widen the
// list below by one type and this goes on returning a row, now whichever of
// several the planner reached first, and tinvest's resolver files a broker's
// trades against it. TestByTickerTradableAnswersOnlyWhereOneRowIsGuaranteed is
// what turns that red.
//
// ticker == "" returns pgx.ErrNoRows without querying, for the same reason
// ByISIN("") does: the empty ticker is deliberately outside that index (see
// ListTradable), so any number of rows could carry it, and "at most one"
// would stop being true for the one input this method would otherwise
// silently pick a row for.
func (s *Store) ByTickerTradable(ctx context.Context, ticker string) (Instrument, error) {
	if ticker == "" {
		return Instrument{}, pgx.ErrNoRows
	}
	return scan(s.db.QueryRow(ctx,
		`SELECT `+cols+` FROM instruments
		WHERE type IN ('share', 'bond', 'etf') AND ticker = $1`, ticker))
}

// ByIDs returns the instruments behind a whole set of ids in a single round
// trip. Ids with no row are simply absent from the map — never zero-valued,
// which would hand a caller an instrument with an empty name and an invalid
// type that reads exactly like a real catalog row. Modelled on
// marketdata.Store.LatestQuotes, which answers a set of instrument ids the
// same way and for the same reason.
//
// It exists for the screens that hold many positions at once (see
// portfolio.Handler.handleList): one query for the catalog rows behind the
// whole page instead of one per row. ByID stays for the single-instrument
// reads, where a missing row is an ordinary pgx.ErrNoRows the caller renders
// as a 404 — a batch cannot say that about one id out of thirty, so absence
// is what it says instead, and the caller decides.
func (s *Store) ByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Instrument, error) {
	out := make(map[uuid.UUID]Instrument, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx,
		`SELECT `+cols+` FROM instruments WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		i, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out[i.ID] = i
	}
	return out, rows.Err()
}

// Search returns one page of the catalog matching a name/ticker/isin fragment
// (case-insensitive), and says whether the catalog holds anything beyond it.
// An empty query lists the whole catalog.
//
// THE SECOND RESULT IS FETCHED, NOT INFERRED, exactly as operation.Store's
// ListByAccount fetches its own: one row beyond the page is asked for, and
// whether it arrives IS the answer; the trim below drops it again before
// anything downstream can mistake it for part of the page. Comparing the page's
// length against the limit would be a different claim — one that stops being
// true the moment a caller reduces the limit it was given — and reading a short
// page as the end of a list is what #86 was, and half of what #104 was.
//
// ORDER BY name, id AND NOT BY name ALONE, which is what makes offsets
// partition the catalog. NOTHING IN THIS PROGRAM HOLDS INSTRUMENT NAMES UNIQUE:
// migration 0011's index covers the ticker and only for tradable rows, neither
// write door here looks at the name beyond refusing an empty one, and the
// T-Invest resolver creates rows under whatever the broker calls a paper. So
// equal names are a state the catalog can reach — and among them Postgres may
// return rows in any order it likes, a different one per query, since nothing
// in the query asks for one. Two pages read from such a catalog would then
// repeat one instrument and skip another, with both pages looking perfectly
// ordinary. id is arbitrary but total, which is all a tie-break has to be.
//
// limit must be positive and offset must not be negative, enforced here rather
// than asked for: a limit of zero asks the query for the probe row alone and
// then trims the page to nothing, publishing an empty page with hasMore true —
// a list showing nothing behind a control that loads nothing however often it
// is pressed — and a negative one panics on the same trim. A negative offset is
// refused by Postgres itself; refusing it here names the parameter instead. The
// refusals are plain errors, not validation ones: the handler in front of this
// answers 400 on both before it ever gets here (see parsePage), so a bad bound
// arriving means the program is wrong, not the person using it.
func (s *Store) Search(ctx context.Context, query string, limit, offset int) ([]Instrument, bool, error) {
	if limit < 1 {
		return nil, false, fmt.Errorf("search instruments: limit must be positive, got %d", limit)
	}
	if offset < 0 {
		return nil, false, fmt.Errorf("search instruments: offset must not be negative, got %d", offset)
	}
	rows, err := s.db.Query(ctx, `
		SELECT `+cols+` FROM instruments
		WHERE $1 = '' OR name ILIKE '%'||$1||'%' OR ticker ILIKE '%'||$1||'%' OR isin ILIKE '%'||$1||'%'
		ORDER BY name, id LIMIT $2 OFFSET $3`, query, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []Instrument
	for rows.Next() {
		i, err := scan(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

// ListTradable returns instruments of type share, bond, or etf that carry a
// non-empty ticker — the subset background market-data jobs can look up on
// an exchange. Currency/crypto/metal/custom instruments and tickerless rows
// are excluded: there is no exchange ticker to fetch a quote for.
//
// The filter below is written again as the predicate of the partial unique
// index on instruments.ticker (migration 0011), because every row this reader
// can return has to be unique by ticker — the quotes job keys a map on it.
// Widening this filter without widening that predicate reopens the silent
// overwrite the index closed; see
// TestUniqueTickerCoversExactlyTheRowsListTradableReturns, which fails if the
// two stop describing the same rows.
//
// ByTickerTradable is a third spelling of it, and it rests on the same
// uniqueness for a different reason — it returns ONE row and needs there to be
// only one. TestByTickerTradableAnswersOnlyWhereOneRowIsGuaranteed holds it to
// this reader in the same way.
func (s *Store) ListTradable(ctx context.Context) ([]Instrument, error) {
	rows, err := s.db.Query(ctx, `
		SELECT `+cols+` FROM instruments
		WHERE type IN ('share', 'bond', 'etf') AND ticker <> ''
		ORDER BY ticker`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Instrument
	for rows.Next() {
		i, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func doublePtr[T any](p **T) *T {
	if p == nil {
		return nil
	}
	return *p
}

func (s *Store) Update(ctx context.Context, id uuid.UUID, upd Update) (Instrument, error) {
	ct, err := s.db.Exec(ctx, `
		UPDATE instruments SET
			name             = COALESCE($2, name),
			ticker           = COALESCE($3, ticker),
			isin             = COALESCE($4, isin),
			figi             = COALESCE($5, figi),
			frozen           = COALESCE($6, frozen),
			face_value_minor = CASE WHEN $7 THEN $8 ELSE face_value_minor END,
			face_currency    = CASE WHEN $9 THEN $10 ELSE face_currency END,
			updated_at       = now()
		WHERE id = $1`,
		id, upd.Name, upd.Ticker, upd.ISIN, upd.FIGI, upd.Frozen,
		upd.FaceValueMinor != nil, doublePtr(upd.FaceValueMinor),
		upd.FaceCurrency != nil, doublePtr(upd.FaceCurrency))
	if err != nil {
		return Instrument{}, wrapTickerConflict(err)
	}
	if ct.RowsAffected() == 0 {
		return Instrument{}, pgx.ErrNoRows
	}
	return s.ByID(ctx, id)
}
