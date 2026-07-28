package family

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgUniqueViolation is the SQLSTATE code Postgres returns for a unique
// constraint violation.
const pgUniqueViolation = "23505"

// wrapUsernameConflict maps a unique_violation on users.username to
// ErrUsernameTaken, so callers get a 409 Conflict instead of an opaque
// 500 when a chosen username is already in use.
func wrapUsernameConflict(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == "users_username_key" {
		return ErrUsernameTaken
	}
	return err
}

// Store is the data access layer of the family module.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

const userCols = `id, username, display_name, password_hash, created_at`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.CreatedAt)
	return u, err
}

func (s *Store) CreateUser(ctx context.Context, username, displayName, passwordHash string) (User, error) {
	return scanUser(s.pool.QueryRow(ctx, `
		INSERT INTO users (username, display_name, password_hash)
		VALUES ($1, $2, $3) RETURNING `+userCols, username, displayName, passwordHash))
}

func (s *Store) UserByUsername(ctx context.Context, username string) (User, error) {
	return scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE username = $1`, username))
}

func (s *Store) UserByID(ctx context.Context, id uuid.UUID) (User, error) {
	return scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE id = $1`, id))
}

// CreateSpaceWithOwner creates the space and the owner membership atomically.
func (s *Store) CreateSpaceWithOwner(ctx context.Context, name string, ownerID uuid.UUID) (Space, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Space{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var sp Space
	err = tx.QueryRow(ctx, `INSERT INTO spaces (name) VALUES ($1)
		RETURNING id, name, base_currency, created_at`, name).Scan(&sp.ID, &sp.Name, &sp.BaseCurrency, &sp.CreatedAt)
	if err != nil {
		return Space{}, fmt.Errorf("insert space: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO memberships (space_id, user_id, role)
		VALUES ($1, $2, 'owner')`, sp.ID, ownerID)
	if err != nil {
		return Space{}, fmt.Errorf("insert owner membership: %w", err)
	}
	return sp, tx.Commit(ctx)
}

// CreateFirstUserWithSpace creates the first user, the family space and the
// owner membership in a single transaction, so a mid-way failure can never
// orphan a user row (which would otherwise permanently wedge SetupNeeded).
func (s *Store) CreateFirstUserWithSpace(ctx context.Context, spaceName, username, displayName, passwordHash string) (User, Space, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, Space{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var u User
	err = tx.QueryRow(ctx, `INSERT INTO users (username, display_name, password_hash)
		VALUES ($1, $2, $3) RETURNING `+userCols, username, displayName, passwordHash).Scan(
		&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		if wrapped := wrapUsernameConflict(err); wrapped != err {
			return User{}, Space{}, wrapped
		}
		return User{}, Space{}, fmt.Errorf("insert user: %w", err)
	}

	var sp Space
	err = tx.QueryRow(ctx, `INSERT INTO spaces (name) VALUES ($1)
		RETURNING id, name, base_currency, created_at`, spaceName).Scan(&sp.ID, &sp.Name, &sp.BaseCurrency, &sp.CreatedAt)
	if err != nil {
		return User{}, Space{}, fmt.Errorf("insert space: %w", err)
	}

	_, err = tx.Exec(ctx, `INSERT INTO memberships (space_id, user_id, role)
		VALUES ($1, $2, 'owner')`, sp.ID, u.ID)
	if err != nil {
		return User{}, Space{}, fmt.Errorf("insert owner membership: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return User{}, Space{}, err
	}
	return u, sp, nil
}

// CreateUserInSpace creates a user and its membership in an existing space in
// a single transaction, so a mid-way failure can never orphan a user row.
func (s *Store) CreateUserInSpace(ctx context.Context, spaceID uuid.UUID, username, displayName, passwordHash string, role Role) (User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var u User
	err = tx.QueryRow(ctx, `INSERT INTO users (username, display_name, password_hash)
		VALUES ($1, $2, $3) RETURNING `+userCols, username, displayName, passwordHash).Scan(
		&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		if wrapped := wrapUsernameConflict(err); wrapped != err {
			return User{}, wrapped
		}
		return User{}, fmt.Errorf("insert user: %w", err)
	}

	_, err = tx.Exec(ctx, `INSERT INTO memberships (space_id, user_id, role)
		VALUES ($1, $2, $3)`, spaceID, u.ID, role)
	if err != nil {
		return User{}, fmt.Errorf("insert membership: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return User{}, err
	}
	return u, nil
}

func (s *Store) SpaceByID(ctx context.Context, id uuid.UUID) (Space, error) {
	var sp Space
	err := s.pool.QueryRow(ctx, `SELECT id, name, base_currency, created_at FROM spaces WHERE id = $1`, id).
		Scan(&sp.ID, &sp.Name, &sp.BaseCurrency, &sp.CreatedAt)
	return sp, err
}

// UpdateBaseCurrency sets the space's base currency. Returns pgx.ErrNoRows
// if the space doesn't exist.
func (s *Store) UpdateBaseCurrency(ctx context.Context, spaceID uuid.UUID, currency string) error {
	ct, err := s.pool.Exec(ctx, `UPDATE spaces SET base_currency = $2 WHERE id = $1`, spaceID, currency)
	if err == nil && ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return err
}

func (s *Store) AddMember(ctx context.Context, spaceID, userID uuid.UUID, role Role) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO memberships (space_id, user_id, role)
		VALUES ($1, $2, $3)`, spaceID, userID, role)
	return err
}

// MembershipFor returns the caller's principal (first membership).
func (s *Store) MembershipFor(ctx context.Context, userID uuid.UUID) (Principal, error) {
	p := Principal{UserID: userID}
	err := s.pool.QueryRow(ctx, `SELECT space_id, role FROM memberships
		WHERE user_id = $1 ORDER BY created_at LIMIT 1`, userID).Scan(&p.SpaceID, &p.Role)
	return p, err
}

func (s *Store) ListMembers(ctx context.Context, spaceID uuid.UUID) ([]Member, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.username, u.display_name, u.password_hash, u.created_at, m.role
		FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.space_id = $1 ORDER BY m.created_at`, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.Username, &m.DisplayName, &m.PasswordHash, &m.CreatedAt, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) UpdateMemberRole(ctx context.Context, spaceID, userID uuid.UUID, role Role) error {
	ct, err := s.pool.Exec(ctx, `UPDATE memberships SET role = $3
		WHERE space_id = $1 AND user_id = $2`, spaceID, userID, role)
	if err == nil && ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return err
}

func (s *Store) RemoveMember(ctx context.Context, spaceID, userID uuid.UUID) error {
	ct, err := s.pool.Exec(ctx, `DELETE FROM memberships
		WHERE space_id = $1 AND user_id = $2`, spaceID, userID)
	if err == nil && ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return err
}
