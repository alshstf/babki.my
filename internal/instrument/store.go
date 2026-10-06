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

// pgUniqueViolation is Postgres's SQLSTATE for a unique violation;
// instrumentsTickerUnique is the index this package can trip (migration 0011).
const (
	pgUniqueViolation       = "23505"
	instrumentsTickerUnique = "instruments_ticker_uniq"
	instrumentsISINUnique   = "instruments_isin_uniq"
)

// ErrTickerTaken means another share, bond or fund already carries this
// ticker: the quotes job looks those up by ticker. Untradable kinds may share
// one. It is a 400 rather than 409 because the contract declares no 409 on
// these writes; family.ErrUsernameTaken is the 409 precedent, and aligning
// them needs a contract change.
var ErrTickerTaken = fmt.Errorf("%w: ticker already belongs to another instrument", family.ErrValidation)

// ErrISINTaken means another instrument already carries this ISIN, which
// identifies a security worldwide (migration 0020). A 400, as above.
var ErrISINTaken = fmt.Errorf("%w: isin already belongs to another instrument", family.ErrValidation)

// wrapTickerConflict maps the ticker and ISIN unique violations to their
// errors and passes everything else through.
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
	face_value_minor, face_currency, frozen, coingecko_id, created_at, updated_at`

func scan(row pgx.Row) (Instrument, error) {
	var i Instrument
	err := row.Scan(&i.ID, &i.Type, &i.Name, &i.Ticker, &i.ISIN, &i.FIGI,
		&i.Currency, &i.FaceValueMinor, &i.FaceCurrency, &i.Frozen, &i.CoinGeckoID,
		&i.CreatedAt, &i.UpdatedAt)
	return i, err
}

func (s *Store) Create(ctx context.Context, inst Instrument) (Instrument, error) {
	created, err := scan(s.db.QueryRow(ctx, `
		INSERT INTO instruments (type, name, ticker, isin, figi, currency,
			face_value_minor, face_currency, frozen, coingecko_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+cols,
		inst.Type, inst.Name, inst.Ticker, inst.ISIN, inst.FIGI,
		inst.Currency, inst.FaceValueMinor, inst.FaceCurrency, inst.Frozen, inst.CoinGeckoID))
	if err != nil {
		return Instrument{}, wrapTickerConflict(err)
	}
	return created, nil
}

func (s *Store) ByID(ctx context.Context, id uuid.UUID) (Instrument, error) {
	return scan(s.db.QueryRow(ctx,
		`SELECT `+cols+` FROM instruments WHERE id = $1`, id))
}

// ByISIN finds the instrument with exactly this ISIN, for the T-Invest
// resolver. ISINs are unique (migration 0020); should a duplicate appear
// anyway, the oldest row wins. An empty ISIN is pgx.ErrNoRows without a query,
// since it would match every row without one.
func (s *Store) ByISIN(ctx context.Context, isin string) (Instrument, error) {
	if isin == "" {
		return Instrument{}, pgx.ErrNoRows
	}
	return scan(s.db.QueryRow(ctx,
		`SELECT `+cols+` FROM instruments WHERE isin = $1 ORDER BY created_at, id LIMIT 1`, isin))
}

// ByTickerTradable finds the tradable instrument (see ListTradable) with
// exactly this ticker. At most one can match because the unique index covers
// exactly that set — held by
// TestByTickerTradableAnswersOnlyWhereOneRowIsGuaranteed. An empty ticker is
// pgx.ErrNoRows without a query.
func (s *Store) ByTickerTradable(ctx context.Context, ticker string) (Instrument, error) {
	if ticker == "" {
		return Instrument{}, pgx.ErrNoRows
	}
	return scan(s.db.QueryRow(ctx,
		`SELECT `+cols+` FROM instruments
		WHERE type IN ('share', 'bond', 'etf') AND ticker = $1`, ticker))
}

// ByIDs returns the instruments behind ids in one round trip. Missing ids are
// absent from the map, never zero-valued.
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

// Search returns one page of instruments matching a name, ticker or ISIN
// fragment (case-insensitive; empty matches all) and whether more exist —
// learned by fetching one extra row, not from the page's length (#86, #104).
// Ordered by name then id: names are not unique, and without the tie-break
// pages could repeat or skip rows. A non-positive limit or negative offset is a
// programming error; the handler refuses them first.
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

// ListTradable returns shares, bonds and funds with a non-empty ticker: what
// the market-data jobs can quote. The filter is repeated as the predicate of
// the unique ticker index (migration 0011), and ByTickerTradable relies on
// the same set; tests hold all three together.
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
			coingecko_id     = COALESCE($11, coingecko_id),
			updated_at       = now()
		WHERE id = $1`,
		id, upd.Name, upd.Ticker, upd.ISIN, upd.FIGI, upd.Frozen,
		upd.FaceValueMinor != nil, doublePtr(upd.FaceValueMinor),
		upd.FaceCurrency != nil, doublePtr(upd.FaceCurrency), upd.CoinGeckoID)
	if err != nil {
		return Instrument{}, wrapTickerConflict(err)
	}
	if ct.RowsAffected() == 0 {
		return Instrument{}, pgx.ErrNoRows
	}
	return s.ByID(ctx, id)
}
