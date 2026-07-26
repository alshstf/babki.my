package family

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/alexedwards/argon2id"
	"github.com/jackc/pgx/v5"
)

var (
	ErrAlreadySetUp       = errors.New("instance is already set up")
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrForbidden          = errors.New("forbidden")
	ErrValidation         = errors.New("validation failed")
)

var usernameRe = regexp.MustCompile(`^[a-z0-9_]{3,32}$`)

// Service implements authentication and member management on top of Store.
type Service struct{ store *Store }

func NewService(store *Store) *Service { return &Service{store: store} }

type SetupParams struct {
	SpaceName   string
	Username    string
	DisplayName string
	Password    string
}

func validateCredentials(username, password string) error {
	if !usernameRe.MatchString(username) {
		return fmt.Errorf("%w: username must match [a-z0-9_]{3,32}", ErrValidation)
	}
	if len(password) < 8 {
		return fmt.Errorf("%w: password must be at least 8 characters", ErrValidation)
	}
	return nil
}

func (s *Service) HashPassword(password string) (string, error) {
	return argon2id.CreateHash(password, argon2id.DefaultParams)
}

// SetupNeeded reports whether the instance has no users yet.
func (s *Service) SetupNeeded(ctx context.Context) (bool, error) {
	n, err := s.store.CountUsers(ctx)
	return n == 0, err
}

// Setup creates the first user, the family space and the owner membership.
func (s *Service) Setup(ctx context.Context, p SetupParams) (User, Principal, error) {
	needed, err := s.SetupNeeded(ctx)
	if err != nil {
		return User{}, Principal{}, err
	}
	if !needed {
		return User{}, Principal{}, ErrAlreadySetUp
	}
	if p.SpaceName == "" || p.DisplayName == "" {
		return User{}, Principal{}, fmt.Errorf("%w: space name and display name are required", ErrValidation)
	}
	if err := validateCredentials(p.Username, p.Password); err != nil {
		return User{}, Principal{}, err
	}
	hash, err := s.HashPassword(p.Password)
	if err != nil {
		return User{}, Principal{}, err
	}
	u, err := s.store.CreateUser(ctx, p.Username, p.DisplayName, hash)
	if err != nil {
		return User{}, Principal{}, err
	}
	sp, err := s.store.CreateSpaceWithOwner(ctx, p.SpaceName, u.ID)
	if err != nil {
		return User{}, Principal{}, err
	}
	return u, Principal{UserID: u.ID, SpaceID: sp.ID, Role: RoleOwner}, nil
}

// Login verifies credentials. Unknown user and wrong password return the
// same ErrInvalidCredentials to avoid user enumeration.
func (s *Service) Login(ctx context.Context, username, password string) (User, Principal, error) {
	u, err := s.store.UserByUsername(ctx, username)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, Principal{}, ErrInvalidCredentials
	}
	if err != nil {
		return User{}, Principal{}, err
	}
	ok, err := argon2id.ComparePasswordAndHash(password, u.PasswordHash)
	if err != nil {
		return User{}, Principal{}, err
	}
	if !ok {
		return User{}, Principal{}, ErrInvalidCredentials
	}
	p, err := s.store.MembershipFor(ctx, u.ID)
	if err != nil {
		return User{}, Principal{}, err
	}
	return u, p, nil
}

// CreateMember lets the owner add a family member with role editor|viewer.
func (s *Service) CreateMember(ctx context.Context, p Principal, username, displayName, password string, role Role) (Member, error) {
	if p.Role != RoleOwner {
		return Member{}, ErrForbidden
	}
	if role != RoleEditor && role != RoleViewer {
		return Member{}, fmt.Errorf("%w: role must be editor or viewer", ErrValidation)
	}
	if displayName == "" {
		return Member{}, fmt.Errorf("%w: display name is required", ErrValidation)
	}
	if err := validateCredentials(username, password); err != nil {
		return Member{}, err
	}
	hash, err := s.HashPassword(password)
	if err != nil {
		return Member{}, err
	}
	u, err := s.store.CreateUser(ctx, username, displayName, hash)
	if err != nil {
		return Member{}, err
	}
	if err := s.store.AddMember(ctx, p.SpaceID, u.ID, role); err != nil {
		return Member{}, err
	}
	return Member{User: u, Role: role}, nil
}
