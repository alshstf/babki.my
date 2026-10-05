package family

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"babki.my/babki/internal/platform/db"
)

// pgUniqueViolation is Postgres's SQLSTATE for a unique violation.
const pgUniqueViolation = "23505"

// wrapUsernameConflict maps a username unique violation to ErrUsernameTaken.
func wrapUsernameConflict(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == "users_username_key" {
		return ErrUsernameTaken
	}
	return err
}

// Store is the data access layer of the family module.
type Store struct{ db db.Executor }

func NewStore(x db.Executor) *Store { return &Store{db: x} }

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

const userCols = `id, username, display_name, password_hash, created_at`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.CreatedAt)
	return u, err
}

// spaceCols and scanSpace are the one way a space is read.
const spaceCols = `id, name, base_currency, tax_residency, created_at`

func scanSpace(row pgx.Row) (Space, error) {
	var sp Space
	err := row.Scan(&sp.ID, &sp.Name, &sp.BaseCurrency, &sp.TaxResidency, &sp.CreatedAt)
	return sp, err
}

func (s *Store) CreateUser(ctx context.Context, username, displayName, passwordHash string) (User, error) {
	return scanUser(s.db.QueryRow(ctx, `
		INSERT INTO users (username, display_name, password_hash)
		VALUES ($1, $2, $3) RETURNING `+userCols, username, displayName, passwordHash))
}

func (s *Store) UserByUsername(ctx context.Context, username string) (User, error) {
	return scanUser(s.db.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE username = $1`, username))
}

func (s *Store) UserByID(ctx context.Context, id uuid.UUID) (User, error) {
	return scanUser(s.db.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE id = $1`, id))
}

// CreateSpaceWithOwner creates the space and the owner membership atomically.
func (s *Store) CreateSpaceWithOwner(ctx context.Context, name string, ownerID uuid.UUID) (Space, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Space{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	sp, err := scanSpace(tx.QueryRow(ctx, `INSERT INTO spaces (name) VALUES ($1)
		RETURNING `+spaceCols, name))
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

// firstUserLockKey is CreateFirstUserWithSpace's advisory lock ("babki1st").
const firstUserLockKey int64 = 0x6261626b69317374

// CreateFirstUserWithSpace creates the first user, the space and the owner
// membership in one transaction, so a failure cannot leave an orphan user that
// would wedge SetupNeeded.
func (s *Store) CreateFirstUserWithSpace(ctx context.Context, spaceName, username, displayName, passwordHash string) (User, Space, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return User{}, Space{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Decide "no user yet" under the lock: two simultaneous setups otherwise both
	// created an owner. The second now waits and is told it is already set up.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, firstUserLockKey); err != nil {
		return User{}, Space{}, fmt.Errorf("lock the first-user setup: %w", err)
	}
	var existing int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&existing); err != nil {
		return User{}, Space{}, fmt.Errorf("count users: %w", err)
	}
	if existing > 0 {
		return User{}, Space{}, ErrAlreadySetUp
	}

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

	sp, err := scanSpace(tx.QueryRow(ctx, `INSERT INTO spaces (name) VALUES ($1)
		RETURNING `+spaceCols, spaceName))
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

// CreateUserInSpace creates a user and its membership in one transaction.
func (s *Store) CreateUserInSpace(ctx context.Context, spaceID uuid.UUID, username, displayName, passwordHash string, role Role) (User, error) {
	tx, err := s.db.Begin(ctx)
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
	return scanSpace(s.db.QueryRow(ctx, `SELECT `+spaceCols+` FROM spaces WHERE id = $1`, id))
}

// DistinctBaseCurrencies returns the sorted base currencies of every space, for
// the rate backfill: a space may display a currency nothing is held in.
func (s *Store) DistinctBaseCurrencies(ctx context.Context) ([]string, error) {
	rows, err := s.db.Query(ctx, `SELECT DISTINCT base_currency FROM spaces ORDER BY base_currency`)
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

// UpdateSpaceSettings updates the given settings in one statement; nil leaves a
// column alone. Validation is Service.UpdateSpace's. pgx.ErrNoRows if the space
// does not exist.
func (s *Store) UpdateSpaceSettings(ctx context.Context, spaceID uuid.UUID, baseCurrency, taxResidency *string) error {
	ct, err := s.db.Exec(ctx, `UPDATE spaces
		SET base_currency = COALESCE($2, base_currency),
		    tax_residency = COALESCE($3, tax_residency)
		WHERE id = $1`, spaceID, baseCurrency, taxResidency)
	if err == nil && ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return err
}

// MembershipFor returns the caller's principal (first membership).
func (s *Store) MembershipFor(ctx context.Context, userID uuid.UUID) (Principal, error) {
	p := Principal{UserID: userID}
	err := s.db.QueryRow(ctx, `SELECT space_id, role FROM memberships
		WHERE user_id = $1 ORDER BY created_at LIMIT 1`, userID).Scan(&p.SpaceID, &p.Role)
	return p, err
}

// sessionFor is MembershipFor plus the moment the user's earlier sessions were
// ended (nil if never), in one round trip.
func (s *Store) sessionFor(ctx context.Context, userID uuid.UUID) (Principal, *time.Time, error) {
	p := Principal{UserID: userID}
	var revoked *time.Time
	err := s.db.QueryRow(ctx, `SELECT m.space_id, m.role, u.sessions_revoked_at
		FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.user_id = $1 ORDER BY m.created_at LIMIT 1`, userID).Scan(&p.SpaceID, &p.Role, &revoked)
	return p, revoked, err
}

// SetPassword stores a new hash and ends sessions signed in before at.
func (s *Store) SetPassword(ctx context.Context, userID uuid.UUID, hash string, at time.Time) error {
	ct, err := s.db.Exec(ctx, `UPDATE users SET password_hash = $2, sessions_revoked_at = $3 WHERE id = $1`,
		userID, hash, at)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// RevokeSessions ends every session of the user signed in before at.
func (s *Store) RevokeSessions(ctx context.Context, userID uuid.UUID, at time.Time) error {
	ct, err := s.db.Exec(ctx, `UPDATE users SET sessions_revoked_at = $2 WHERE id = $1`, userID, at)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (s *Store) ListMembers(ctx context.Context, spaceID uuid.UUID) ([]Member, error) {
	rows, err := s.db.Query(ctx, `
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
	ct, err := s.db.Exec(ctx, `UPDATE memberships SET role = $3
		WHERE space_id = $1 AND user_id = $2`, spaceID, userID, role)
	if err == nil && ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return err
}

func (s *Store) RemoveMember(ctx context.Context, spaceID, userID uuid.UUID) error {
	ct, err := s.db.Exec(ctx, `DELETE FROM memberships
		WHERE space_id = $1 AND user_id = $2`, spaceID, userID)
	if err == nil && ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return err
}
