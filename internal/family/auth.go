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
	// ErrUsernameTaken is returned when a username collides with an existing
	// user (unique_violation on users.username). Distinct from ErrValidation
	// because the input itself is well-formed; the conflict is with existing
	// state, so it maps to 409 rather than 400.
	ErrUsernameTaken = errors.New("username already taken")
)

// UsernamePattern is the shape of a username, as a regular expression source
// rather than as a compiled one so the contract-site test can compare it against
// the `pattern` api/openapi.yaml states on the two request schemas that carry a
// username, and the frontend-site test against the regex literal the two dialogs
// hold. Exported for the same reason currency.Pattern is: a rule written down in
// three languages that nothing keeps in step is this codebase's recurring bug.
//
// It is NOT declared on LoginRequest, and that is deliberate: Login checks no
// shape at all (see it), so declaring one there would describe a refusal that
// does not exist.
const UsernamePattern = `^[a-z0-9_]{3,32}$`

// MinPasswordRunes is the shortest password the two doors that create a user
// accept, counted in RUNES.
//
// IT USED TO BE COUNTED IN BYTES while the refusal beside it said «characters»,
// and one of the two was necessarily wrong (#117). The count moved rather than
// the sentence, because the sentence is what the person reads and because
// `minLength` in JSON Schema counts characters too — so api/openapi.yaml can now
// state this floor at all, which with a byte count it could not: «паролям» is
// seven characters and fourteen bytes, and a document saying `minLength: 8`
// would have refused what the server took.
//
// The interface said the same thing the refusal did, and was wrong in the same
// way: setup.passwordHint reads «минимум 8 символов». It is true now.
//
// Runes rather than bytes therefore makes this door STRICTER, and only for
// non-ASCII passwords. Nobody is locked out by it: Login never calls
// validateCredentials — it compares against the stored hash and nothing else —
// so a password accepted under the old count keeps working for good. What
// changes is that setting a NEW one now needs eight characters however they are
// spelled, which is what the refusal has claimed all along.
const MinPasswordRunes = 8

// MaxPasswordRunes is the longest password either door accepts, counted the way
// MinPasswordRunes is. Nobody's password is a thousand characters; the ceiling is
// there so that what gets hashed has a size this server chose.
const MaxPasswordRunes = 1024

// MaxNameRunes is the longest display name and space name, counted the way
// MinPasswordRunes is. A name is a word or two on a header; the ceiling is there
// so that what every screen draws has a size this server chose.
const MaxNameRunes = 100

// checkName refuses an empty name or one past MaxNameRunes; what names the
// field in the refusal.
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

// hashParams are what a password is hashed with.
//
// argon2id.DefaultParams as this machine evaluates it: 64 MiB, one pass, and as
// many lanes as there are CPUs. One name for it, because the hash a new user
// gets and the hash an unknown username is compared against (see dummyHash)
// have to cost the same.
var hashParams = argon2id.DefaultParams

// hashSlots bounds how many passwords are being hashed at once in this process.
//
// Every hash takes 64 MiB for as long as it runs. Unbounded, fifty sign-in
// attempts arriving together take three gigabytes, and a home server falls over
// before a single password has been refused. Two at a time is 128 MiB at the
// worst and still more sign-ins a second than a household makes in a day.
//
// Package-level, because the memory is the process's however many Services it
// holds.
var hashSlots = make(chan struct{}, 2)

// withHashSlot runs fn while holding one of the hash slots, or gives up when
// the request does.
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

// dummyHash is a hash of a password nobody has, made once per process with the
// very parameters a real one is made with. Login compares against it for a
// username it does not know, so that branch takes as long as a wrong password
// does and the response time does not say which usernames exist.
//
// It used to be a constant hashed with ten lanes, while real hashes are made
// with as many lanes as the machine has CPUs — on a two-core server the two
// branches took measurably different time.
var dummyHash = sync.OnceValue(func() string {
	hash, err := argon2id.CreateHash("a password nobody has", hashParams)
	if err != nil {
		// Creating a hash fails only when the system's random source does, and
		// then no real password can be set either. The comparison against an
		// empty hash fails at once; the timing is the least of it.
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
	// utf8.RuneCountInString, not len: see MinPasswordRunes for why the sentence
	// below is the rule and the byte count was the bug.
	runes := utf8.RuneCountInString(password)
	if runes < MinPasswordRunes {
		return fmt.Errorf("%w: password must be at least %d characters", ErrValidation, MinPasswordRunes)
	}
	if runes > MaxPasswordRunes {
		return fmt.Errorf("%w: password must be at most %d characters", ErrValidation, MaxPasswordRunes)
	}
	return nil
}

// HashPassword hashes a password for storage, one of at most len(hashSlots) at
// a time.
func (s *Service) HashPassword(ctx context.Context, password string) (string, error) {
	var hash string
	var err error
	if slotErr := withHashSlot(ctx, func() { hash, err = argon2id.CreateHash(password, hashParams) }); slotErr != nil {
		return "", slotErr
	}
	return hash, err
}

// passwordMatches compares a password with a stored hash under the same bound
// HashPassword works under.
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

// Login verifies credentials. Unknown user and wrong password return the
// same ErrInvalidCredentials to avoid user enumeration.
func (s *Service) Login(ctx context.Context, username, password string) (User, Principal, error) {
	if utf8.RuneCountInString(password) > MaxPasswordRunes {
		// Longer than any password that could have been set, so it is nobody's.
		return User{}, Principal{}, ErrInvalidCredentials
	}
	u, err := s.store.UserByUsername(ctx, username)
	if errors.Is(err, pgx.ErrNoRows) {
		// A real comparison against a hash of nothing, so this branch takes as
		// long as the wrong-password branch below (see dummyHash). What it
		// answers is not looked at — only a request that gave up is reported.
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
		// User exists without a membership (e.g. orphaned by a partial
		// failure elsewhere). Don't leak that detail to the caller.
		return User{}, Principal{}, ErrInvalidCredentials
	}
	if err != nil {
		return User{}, Principal{}, err
	}
	return u, p, nil
}

// ChangePassword replaces the user's password once the current one is given,
// and ends every session signed in before the change; the moment it took
// effect is returned for the caller's own session to be signed in at (see
// Auth.SignInAt). A wrong current password is ErrInvalidCredentials.
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
	// Whole microseconds, as the column keeps them, so that the moment this
	// session is signed in at is exactly the one stored, never a hair before.
	at := time.Now().Truncate(time.Microsecond)
	return at, s.store.SetPassword(ctx, userID, hash, at)
}

// SignOutElsewhere ends every session of the user signed in before now; the
// moment is returned for the caller's own session to be signed in at.
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
