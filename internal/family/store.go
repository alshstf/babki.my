package family

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

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
		RETURNING id, name, created_at`, name).Scan(&sp.ID, &sp.Name, &sp.CreatedAt)
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

func (s *Store) SpaceByID(ctx context.Context, id uuid.UUID) (Space, error) {
	var sp Space
	err := s.pool.QueryRow(ctx, `SELECT id, name, created_at FROM spaces WHERE id = $1`, id).
		Scan(&sp.ID, &sp.Name, &sp.CreatedAt)
	return sp, err
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
