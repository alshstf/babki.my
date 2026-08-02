package family

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/alexedwards/argon2id"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrAlreadySetUp       = errors.New("instance is already set up")
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrForbidden          = errors.New("forbidden")
	ErrValidation         = errors.New("validation failed")
	// ErrUsernameTaken is returned when a username collides with an existing
	// user (unique_violation on users.username). Distinct from ErrValidation
	// because the input itself is well-formed; the conflict is with existing
	// state, so it maps to 409 rather than 400.
	ErrUsernameTaken = errors.New("username already taken")
)

var (
	usernameRe = regexp.MustCompile(`^[a-z0-9_]{3,32}$`)
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
)

// dummyHash is a precomputed argon2id hash (params: argon2id.DefaultParams,
// passphrase "dummy-password-for-timing-safety") used to run a real
// ComparePasswordAndHash on the unknown-user path in Login. This keeps the
// unknown-user and wrong-password branches taking comparable time, so
// response latency can't be used to enumerate valid usernames.
const dummyHash = "$argon2id$v=19$m=65536,t=1,p=10$uCI9AQQt4/1WJmPclbYXzg$zPMTCrkLSnqn0LFwxFOsmtZzj2IpULcX4CzdpoIRs8k"

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
	u, sp, err := s.store.CreateFirstUserWithSpace(ctx, p.SpaceName, p.Username, p.DisplayName, hash)
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
		// Run a real argon2id comparison against a fixed dummy hash so this
		// branch takes comparable time to the wrong-password branch below,
		// closing the username-enumeration timing side-channel.
		_, _ = argon2id.ComparePasswordAndHash(password, dummyHash)
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
	if errors.Is(err, pgx.ErrNoRows) {
		// User exists without a membership (e.g. orphaned by a partial
		// failure elsewhere). Don't leak that detail to the caller.
		return User{}, Principal{}, ErrInvalidCredentials
	}
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
	u, err := s.store.CreateUserInSpace(ctx, p.SpaceID, username, displayName, hash, role)
	if err != nil {
		return Member{}, err
	}
	return Member{User: u, Role: role}, nil
}

// UpdateMemberRole changes a member's role (owner-only; owner is immutable).
func (s *Service) UpdateMemberRole(ctx context.Context, p Principal, targetID uuid.UUID, role Role) (Member, error) {
	if p.Role != RoleOwner {
		return Member{}, ErrForbidden
	}
	if role != RoleEditor && role != RoleViewer {
		return Member{}, fmt.Errorf("%w: role must be editor or viewer", ErrValidation)
	}
	target, err := s.store.MembershipFor(ctx, targetID)
	if err != nil {
		return Member{}, err
	}
	if target.SpaceID != p.SpaceID {
		return Member{}, pgx.ErrNoRows
	}
	if target.Role == RoleOwner {
		return Member{}, fmt.Errorf("%w: the owner role cannot be changed", ErrValidation)
	}
	if err := s.store.UpdateMemberRole(ctx, p.SpaceID, targetID, role); err != nil {
		return Member{}, err
	}
	u, err := s.store.UserByID(ctx, targetID)
	if err != nil {
		return Member{}, err
	}
	return Member{User: u, Role: role}, nil
}

// SpaceSettings is a partial update of the space: a nil field is left
// unchanged. Pointers rather than empty strings, because "" is a value a caller
// can send by accident and it must be refused, not read as "leave it alone".
type SpaceSettings struct {
	BaseCurrency *string
	TaxResidency *string
}

// UpdateSpace changes the space's base currency and/or the owner's country of
// tax residency (owner-only).
//
// An empty request is REFUSED rather than answered with an unchanged space: a
// caller that meant to change something and sent nothing would otherwise get a
// 200 and a response that looks exactly like success.
//
// The country is checked against the rules table, not merely against the ISO
// shape (see KnownTaxResidency). Accepting "XX" — or "FR", a real country this
// application has no rules for — and then computing FIFO per account for it
// would tell the owner their figures follow rules that were never consulted.
// Refusing is the only answer that stays true: the owner learns immediately
// that this application cannot speak for that country, instead of learning it
// from a tax authority.
func (s *Service) UpdateSpace(ctx context.Context, p Principal, in SpaceSettings) (Space, error) {
	if p.Role != RoleOwner {
		return Space{}, ErrForbidden
	}
	if in.BaseCurrency == nil && in.TaxResidency == nil {
		return Space{}, fmt.Errorf("%w: nothing to update: give base_currency, tax_residency or both", ErrValidation)
	}
	if in.BaseCurrency != nil && !currencyRe.MatchString(*in.BaseCurrency) {
		return Space{}, fmt.Errorf("%w: base_currency must be an uppercase ISO-4217 code (e.g. RUB)", ErrValidation)
	}
	if in.TaxResidency != nil {
		if !taxResidencyRe.MatchString(*in.TaxResidency) {
			return Space{}, fmt.Errorf("%w: tax_residency must be an uppercase ISO 3166-1 alpha-2 code (e.g. RU)", ErrValidation)
		}
		if !KnownTaxResidency(*in.TaxResidency) {
			return Space{}, fmt.Errorf("%w: no cost basis rules are known for tax residency %s; this application can only answer for %s",
				ErrValidation, *in.TaxResidency, strings.Join(taxResidencyCodes(), ", "))
		}
	}
	if err := s.store.UpdateSpaceSettings(ctx, p.SpaceID, in.BaseCurrency, in.TaxResidency); err != nil {
		return Space{}, err
	}
	return s.store.SpaceByID(ctx, p.SpaceID)
}

// RemoveMember deletes a member (owner-only; the owner cannot be removed).
func (s *Service) RemoveMember(ctx context.Context, p Principal, targetID uuid.UUID) error {
	if p.Role != RoleOwner {
		return ErrForbidden
	}
	target, err := s.store.MembershipFor(ctx, targetID)
	if err != nil {
		return err
	}
	if target.SpaceID != p.SpaceID {
		return pgx.ErrNoRows
	}
	if target.Role == RoleOwner {
		return fmt.Errorf("%w: the owner cannot be removed", ErrValidation)
	}
	return s.store.RemoveMember(ctx, p.SpaceID, targetID)
}
