package instrument

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

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
	return scan(s.pool.QueryRow(ctx, `
		INSERT INTO instruments (type, name, ticker, isin, figi, currency,
			face_value_minor, face_currency, frozen)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+cols,
		inst.Type, inst.Name, inst.Ticker, inst.ISIN, inst.FIGI,
		inst.Currency, inst.FaceValueMinor, inst.FaceCurrency, inst.Frozen))
}

func (s *Store) ByID(ctx context.Context, id uuid.UUID) (Instrument, error) {
	return scan(s.pool.QueryRow(ctx,
		`SELECT `+cols+` FROM instruments WHERE id = $1`, id))
}

// Search finds instruments by name/ticker/isin fragment (case-insensitive).
// Empty query lists the whole catalog ordered by name.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]Instrument, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+cols+` FROM instruments
		WHERE $1 = '' OR name ILIKE '%'||$1||'%' OR ticker ILIKE '%'||$1||'%' OR isin ILIKE '%'||$1||'%'
		ORDER BY name LIMIT $2`, query, limit)
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

// ListTradable returns instruments of type share, bond, or etf that carry a
// non-empty ticker — the subset background market-data jobs can look up on
// an exchange. Currency/crypto/metal/custom instruments and tickerless rows
// are excluded: there is no exchange ticker to fetch a quote for.
func (s *Store) ListTradable(ctx context.Context) ([]Instrument, error) {
	rows, err := s.pool.Query(ctx, `
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
	ct, err := s.pool.Exec(ctx, `
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
		return Instrument{}, err
	}
	if ct.RowsAffected() == 0 {
		return Instrument{}, pgx.ErrNoRows
	}
	return s.ByID(ctx, id)
}
