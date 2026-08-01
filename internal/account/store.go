package account

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const accCols = `a.id, a.space_id, a.owner_user_id, a.name, a.type, a.currency,
	a.institution, a.status, a.created_at, a.updated_at`

// withBalanceQuery joins the latest balance mark per account.
const withBalanceQuery = `
	SELECT ` + accCols + `, b.as_of, b.amount_minor
	FROM accounts a
	LEFT JOIN LATERAL (
		SELECT as_of, amount_minor FROM account_balances
		WHERE account_id = a.id ORDER BY as_of DESC LIMIT 1
	) b ON true`

func scanWithBalance(row pgx.Row) (WithBalance, error) {
	var a WithBalance
	var asOf *time.Time
	var amount *int64
	err := row.Scan(&a.ID, &a.SpaceID, &a.OwnerUserID, &a.Name, &a.Type, &a.Currency,
		&a.Institution, &a.Status, &a.CreatedAt, &a.UpdatedAt, &asOf, &amount)
	if err != nil {
		return WithBalance{}, err
	}
	if asOf != nil && amount != nil {
		a.Balance = &BalancePoint{AsOf: *asOf, AmountMinor: *amount}
	}
	return a, nil
}

func (s *Store) Create(
	ctx context.Context,
	spaceID uuid.UUID,
	ownerUserID *uuid.UUID,
	name string,
	t Type,
	currency, institution string,
) (Account, error) {
	var a Account
	err := s.pool.QueryRow(ctx, `
		INSERT INTO accounts (space_id, owner_user_id, name, type, currency, institution)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, space_id, owner_user_id, name, type, currency, institution, status, created_at, updated_at`,
		spaceID, ownerUserID, name, t, currency, institution).
		Scan(&a.ID, &a.SpaceID, &a.OwnerUserID, &a.Name, &a.Type, &a.Currency,
			&a.Institution, &a.Status, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

func (s *Store) ByID(ctx context.Context, spaceID, id uuid.UUID) (WithBalance, error) {
	return scanWithBalance(s.pool.QueryRow(ctx,
		withBalanceQuery+` WHERE a.space_id = $1 AND a.id = $2`, spaceID, id))
}

func (s *Store) ListWithBalance(ctx context.Context, spaceID uuid.UUID) ([]WithBalance, error) {
	rows, err := s.pool.Query(ctx, withBalanceQuery+`
		WHERE a.space_id = $1
		ORDER BY a.status, a.name`, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WithBalance
	for rows.Next() {
		a, err := scanWithBalance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) Update(ctx context.Context, spaceID, id uuid.UUID, upd Update) (WithBalance, error) {
	ct, err := s.pool.Exec(ctx, `
		UPDATE accounts SET
			name          = COALESCE($3, name),
			institution   = COALESCE($4, institution),
			owner_user_id = CASE WHEN $5 THEN $6 ELSE owner_user_id END,
			status        = COALESCE($7, status),
			updated_at    = now()
		WHERE space_id = $1 AND id = $2`,
		spaceID, id, upd.Name, upd.Institution,
		upd.OwnerUserID != nil, ownerValue(upd.OwnerUserID), upd.Status)
	if err != nil {
		return WithBalance{}, err
	}
	if ct.RowsAffected() == 0 {
		return WithBalance{}, pgx.ErrNoRows
	}
	return s.ByID(ctx, spaceID, id)
}

func ownerValue(p **uuid.UUID) *uuid.UUID {
	if p == nil {
		return nil
	}
	return *p
}

func (s *Store) Archive(ctx context.Context, spaceID, id uuid.UUID) error {
	st := StatusArchived
	_, err := s.Update(ctx, spaceID, id, Update{Status: &st})
	return err
}

// SetBalance upserts a manual balance mark, verifying space ownership.
func (s *Store) SetBalance(ctx context.Context, spaceID, accountID uuid.UUID, asOf time.Time, amountMinor int64) error {
	ct, err := s.pool.Exec(ctx, `
		INSERT INTO account_balances (account_id, as_of, amount_minor)
		SELECT a.id, $3, $4 FROM accounts a WHERE a.id = $2 AND a.space_id = $1
		ON CONFLICT (account_id, as_of) DO UPDATE SET amount_minor = EXCLUDED.amount_minor`,
		spaceID, accountID, asOf, amountMinor)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// DistinctCurrencies returns the sorted set of currencies used by any
// account in the instance (not scoped to a space: exchange rates are shared
// market data, so there is no point fetching them per space). Deciding which
// currencies to actually backfill rates for is not this method's job — that
// belongs to the fx backfill job, which also consults operation.Store's
// currencies. Returns an empty slice, not an error, when there are no
// accounts: unlike EarliestOccurredOn, "no currencies in use" is itself a
// meaningful answer, not a missing value.
func (s *Store) DistinctCurrencies(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT currency FROM accounts ORDER BY currency`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SummaryByCurrency aggregates latest balances of active accounts per currency.
func (s *Store) SummaryByCurrency(ctx context.Context, spaceID uuid.UUID) ([]CurrencyTotal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.currency,
			COALESCE(SUM(CASE WHEN a.type IN ('credit_card','loan') THEN 0 ELSE COALESCE(b.amount_minor, 0) END), 0),
			COALESCE(SUM(CASE WHEN a.type IN ('credit_card','loan') THEN COALESCE(b.amount_minor, 0) ELSE 0 END), 0)
		FROM accounts a
		LEFT JOIN LATERAL (
			SELECT amount_minor FROM account_balances
			WHERE account_id = a.id ORDER BY as_of DESC LIMIT 1
		) b ON true
		WHERE a.space_id = $1 AND a.status = 'active'
		GROUP BY a.currency ORDER BY a.currency`, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CurrencyTotal
	for rows.Next() {
		var t CurrencyTotal
		if err := rows.Scan(&t.Currency, &t.AssetsMinor, &t.LiabilitiesMinor); err != nil {
			return nil, err
		}
		t.NetMinor = t.AssetsMinor + t.LiabilitiesMinor
		out = append(out, t)
	}
	return out, rows.Err()
}
