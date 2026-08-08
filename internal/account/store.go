package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/db"
)

const (
	pgForeignKeyViolation = "23503"
	// accountsOwnerFK is the constraint behind accounts.owner_user_id, named
	// by Postgres itself when migration 0003 declared the reference.
	accountsOwnerFK = "accounts_owner_user_id_fkey"
)

// ErrOwnerNotFound means owner_user_id names nobody. It is what turns a request
// carrying a stale or mistyped user id into a 400 saying so, instead of the
// "internal error" an unmapped foreign-key violation produces — a 500 blames
// the server for a value the client chose.
//
// "of this instance" and not "of this space" is the whole truth today and will
// have to be revisited: an instance holds exactly one space (setup succeeds
// once; see family.ErrAlreadySetUp), so every user there is a member of the
// only space there is. The day that stops being true, this check stops being
// enough on its own — a user id belonging to another space would satisfy the
// foreign key and this message — and the handler will need a membership check
// beside it.
var ErrOwnerNotFound = fmt.Errorf("%w: owner_user_id does not name a user of this instance", family.ErrValidation)

// wrapOwnerFK maps a foreign-key violation on accounts.owner_user_id to
// ErrOwnerNotFound. Every other error passes through untouched — the same shape
// as instrument.wrapTickerConflict and family.wrapUsernameConflict, each of
// which translates exactly the constraints its own writes can trip and nothing
// else. A violation on accounts.space_id is deliberately not translated: the
// space id never comes from the request, so that one really would be this
// program's own fault and belongs in the 500 it produces.
func wrapOwnerFK(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) &&
		pgErr.Code == pgForeignKeyViolation && pgErr.ConstraintName == accountsOwnerFK {
		return ErrOwnerNotFound
	}
	return err
}

type Store struct{ db db.Executor }

func NewStore(x db.Executor) *Store { return &Store{db: x} }

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
	err := s.db.QueryRow(ctx, `
		INSERT INTO accounts (space_id, owner_user_id, name, type, currency, institution)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, space_id, owner_user_id, name, type, currency, institution, status, created_at, updated_at`,
		spaceID, ownerUserID, name, t, currency, institution).
		Scan(&a.ID, &a.SpaceID, &a.OwnerUserID, &a.Name, &a.Type, &a.Currency,
			&a.Institution, &a.Status, &a.CreatedAt, &a.UpdatedAt)
	return a, wrapOwnerFK(err)
}

func (s *Store) ByID(ctx context.Context, spaceID, id uuid.UUID) (WithBalance, error) {
	return scanWithBalance(s.db.QueryRow(ctx,
		withBalanceQuery+` WHERE a.space_id = $1 AND a.id = $2`, spaceID, id))
}

func (s *Store) ListWithBalance(ctx context.Context, spaceID uuid.UUID) ([]WithBalance, error) {
	rows, err := s.db.Query(ctx, withBalanceQuery+`
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
	ct, err := s.db.Exec(ctx, `
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
		return WithBalance{}, wrapOwnerFK(err)
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
	ct, err := s.db.Exec(ctx, `
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
	rows, err := s.db.Query(ctx, `SELECT DISTINCT currency FROM accounts ORDER BY currency`)
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
//
// Which types count as debt is a PARAMETER and not a literal in the statement:
// the list comes from LiabilityTypes, which derives it from Type.IsLiability,
// so the query cannot go on splitting by an older idea of what a debt is than
// the rest of the package holds.
func (s *Store) SummaryByCurrency(ctx context.Context, spaceID uuid.UUID) ([]CurrencyTotal, error) {
	rows, err := s.db.Query(ctx, `
		SELECT a.currency,
			COALESCE(SUM(CASE WHEN a.type = ANY($2) THEN 0 ELSE COALESCE(b.amount_minor, 0) END), 0),
			COALESCE(SUM(CASE WHEN a.type = ANY($2) THEN COALESCE(b.amount_minor, 0) ELSE 0 END), 0)
		FROM accounts a
		LEFT JOIN LATERAL (
			SELECT amount_minor FROM account_balances
			WHERE account_id = a.id ORDER BY as_of DESC LIMIT 1
		) b ON true
		WHERE a.space_id = $1 AND a.status = 'active'
		GROUP BY a.currency ORDER BY a.currency`, spaceID, LiabilityTypes())
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
