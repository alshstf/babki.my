package family

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/alexedwards/argon2id"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/platform/currency"
)

var (
	ErrAlreadySetUp       = errors.New("instance is already set up")
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrForbidden          = errors.New("forbidden")
	ErrValidation         = errors.New("validation failed")
	// ErrUsernameTaken: the username exists. A conflict with state, so 409, not
	// 400.
	ErrUsernameTaken = errors.New("username already taken")
)

// UsernamePattern is the shape of a username; the contract and the web dialogs
// state it too, and tests hold them together. Login checks no shape, so the
// contract does not declare one there.
const UsernamePattern = `^[a-z0-9_]{3,32}$`

// MinPasswordRunes is the shortest password for a new user, in characters as
// the message and JSON Schema's minLength count them (#117: it was bytes).
// Login compares hashes only, so older passwords keep working.
const MinPasswordRunes = 8

// MaxPasswordRunes caps what gets hashed.
const MaxPasswordRunes = 1024

// MaxNameRunes caps display and space names.
const MaxNameRunes = 100

// checkName refuses an empty name or one over MaxNameRunes.
func checkName(what, name string) error {
	if name == "" {
		return fmt.Errorf("%w: %s is required", ErrValidation, what)
	}
	if utf8.RuneCountInString(name) > MaxNameRunes {
		return fmt.Errorf("%w: %s must be at most %d characters", ErrValidation, what, MaxNameRunes)
	}
	return nil
}

var usernameRe = regexp.MustCompile(UsernamePattern)

// hashParams are argon2id.DefaultParams: 64 MiB, one pass, a lane per CPU. A
// new user's hash and dummyHash must cost the same.
var hashParams = argon2id.DefaultParams

// hashSlots bounds concurrent hashing per process: each hash takes 64 MiB,
// and fifty simultaneous sign-ins would take three gigabytes.
var hashSlots = make(chan struct{}, 2)

// withHashSlot runs fn holding a slot, or gives up when the request does.
func withHashSlot(ctx context.Context, fn func()) error {
	select {
	case hashSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-hashSlots }()
	fn()
	return nil
}

// dummyHash is a hash of a password nobody has, made with the real parameters,
// so an unknown username takes as long as a wrong password.
var dummyHash = sync.OnceValue(func() string {
	hash, err := argon2id.CreateHash("a password nobody has", hashParams)
	if err != nil {
		// Hashing fails only when the random source does; then nothing can be set,
		// and comparing against "" fails at once.
		return ""
	}
	return hash
})

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
	return validatePassword(password)
}

// validatePassword is the rule every new password is held to.
func validatePassword(password string) error {
	// Characters, not bytes: see MinPasswordRunes.
	runes := utf8.RuneCountInString(password)
	if runes < MinPasswordRunes {
		return fmt.Errorf("%w: password must be at least %d characters", ErrValidation, MinPasswordRunes)
	}
	if runes > MaxPasswordRunes {
		return fmt.Errorf("%w: password must be at most %d characters", ErrValidation, MaxPasswordRunes)
	}
	return nil
}

// HashPassword hashes a password, within the hash slots.
func (s *Service) HashPassword(ctx context.Context, password string) (string, error) {
	var hash string
	var err error
	if slotErr := withHashSlot(ctx, func() { hash, err = argon2id.CreateHash(password, hashParams) }); slotErr != nil {
		return "", slotErr
	}
	return hash, err
}

// passwordMatches compares a password with a hash, within the hash slots.
func passwordMatches(ctx context.Context, password, hash string) (bool, error) {
	var ok bool
	var err error
	if slotErr := withHashSlot(ctx, func() { ok, err = argon2id.ComparePasswordAndHash(password, hash) }); slotErr != nil {
		return false, slotErr
	}
	return ok, err
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
	if err := checkName("space name", p.SpaceName); err != nil {
		return User{}, Principal{}, err
	}
	if err := checkName("display name", p.DisplayName); err != nil {
		return User{}, Principal{}, err
	}
	if err := validateCredentials(p.Username, p.Password); err != nil {
		return User{}, Principal{}, err
	}
	hash, err := s.HashPassword(ctx, p.Password)
	if err != nil {
		return User{}, Principal{}, err
	}
	u, sp, err := s.store.CreateFirstUserWithSpace(ctx, p.SpaceName, p.Username, p.DisplayName, hash)
	if err != nil {
		return User{}, Principal{}, err
	}
	return u, Principal{UserID: u.ID, SpaceID: sp.ID, Role: RoleOwner}, nil
}

// Login verifies credentials. An unknown user and a wrong password both return
// ErrInvalidCredentials.
func (s *Service) Login(ctx context.Context, username, password string) (User, Principal, error) {
	if utf8.RuneCountInString(password) > MaxPasswordRunes {
		// Longer than any password that could have been set, so it is nobody's.
		return User{}, Principal{}, ErrInvalidCredentials
	}
	u, err := s.store.UserByUsername(ctx, username)
	if errors.Is(err, pgx.ErrNoRows) {
		// Compare against a hash of nothing so this branch takes as long as a wrong
		// password; only a request that gave up is reported.
		if _, err := passwordMatches(ctx, password, dummyHash()); err != nil && ctx.Err() != nil {
			return User{}, Principal{}, err
		}
		return User{}, Principal{}, ErrInvalidCredentials
	}
	if err != nil {
		return User{}, Principal{}, err
	}
	ok, err := passwordMatches(ctx, password, u.PasswordHash)
	if err != nil {
		return User{}, Principal{}, err
	}
	if !ok {
		return User{}, Principal{}, ErrInvalidCredentials
	}
	p, err := s.store.MembershipFor(ctx, u.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		// A user with no membership: do not say so.
		return User{}, Principal{}, ErrInvalidCredentials
	}
	if err != nil {
		return User{}, Principal{}, err
	}
	return u, p, nil
}

// ChangePassword replaces the password given the current one and ends the
// user's earlier sessions, returning the moment for the caller's own session
// (see Auth.SignInAt). A wrong current password is ErrInvalidCredentials.
func (s *Service) ChangePassword(ctx context.Context, userID uuid.UUID, current, next string) (time.Time, error) {
	if err := validatePassword(next); err != nil {
		return time.Time{}, err
	}
	if utf8.RuneCountInString(current) > MaxPasswordRunes {
		return time.Time{}, ErrInvalidCredentials
	}
	u, err := s.store.UserByID(ctx, userID)
	if err != nil {
		return time.Time{}, err
	}
	ok, err := passwordMatches(ctx, current, u.PasswordHash)
	if err != nil {
		return time.Time{}, err
	}
	if !ok {
		return time.Time{}, ErrInvalidCredentials
	}
	hash, err := s.HashPassword(ctx, next)
	if err != nil {
		return time.Time{}, err
	}
	// Whole microseconds, as the column stores them.
	at := time.Now().Truncate(time.Microsecond)
	return at, s.store.SetPassword(ctx, userID, hash, at)
}

// SignOutElsewhere ends the user's sessions signed in before now and returns
// the moment for the caller's own session.
func (s *Service) SignOutElsewhere(ctx context.Context, userID uuid.UUID) (time.Time, error) {
	at := time.Now().Truncate(time.Microsecond)
	return at, s.store.RevokeSessions(ctx, userID, at)
}

// CreateMember lets the owner add a family member with role editor|viewer.
func (s *Service) CreateMember(ctx context.Context, p Principal, username, displayName, password string, role Role) (Member, error) {
	if p.Role != RoleOwner {
		return Member{}, ErrForbidden
	}
	if role != RoleEditor && role != RoleViewer {
		return Member{}, fmt.Errorf("%w: role must be editor or viewer", ErrValidation)
	}
	if err := checkName("display name", displayName); err != nil {
		return Member{}, err
	}
	if err := validateCredentials(username, password); err != nil {
		return Member{}, err
	}
	hash, err := s.HashPassword(ctx, password)
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

// SpaceSettings is a partial update of the space; nil leaves a field alone, so
// an accidental "" is refused rather than ignored.
type SpaceSettings struct {
	BaseCurrency *string
	TaxResidency *string
}

// UpdateSpace changes the base currency and/or the owner's tax residency
// (owner only). An empty request is refused rather than answered as success.
// The country must have a row in the rules table, not just the ISO shape: the
// application cannot speak for a country it has no rules for.
func (s *Service) UpdateSpace(ctx context.Context, p Principal, in SpaceSettings) (Space, error) {
	if p.Role != RoleOwner {
		return Space{}, ErrForbidden
	}
	if in.BaseCurrency == nil && in.TaxResidency == nil {
		return Space{}, fmt.Errorf("%w: nothing to update: give base_currency, tax_residency or both", ErrValidation)
	}
	if in.BaseCurrency != nil && !currency.Valid(*in.BaseCurrency) {
		return Space{}, fmt.Errorf("%w: base_currency must be an uppercase ISO-4217 code (e.g. RUB)", ErrValidation)
	}
	if in.TaxResidency != nil {
		if !taxResidencyRe.MatchString(*in.TaxResidency) {
			return Space{}, fmt.Errorf("%w: tax_residency must be an uppercase ISO 3166-1 alpha-2 code (e.g. RU)", ErrValidation)
		}
		if !KnownTaxResidency(*in.TaxResidency) {
			return Space{}, fmt.Errorf("%w: no cost basis rules are known for tax residency %s; this application knows the rules of %s only "+
				"(knowing a country's rules is not the same as its FIFO/account computation matching them — see GET /api/v1/tax-residencies for which of them it does)",
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
