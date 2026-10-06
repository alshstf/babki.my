// Package family owns users, the family space, memberships/roles,
// authentication and sessions. Tables: users, spaces, memberships, sessions.
package family

import (
	"time"

	"github.com/google/uuid"
)

type Role string

const (
	RoleOwner  Role = "owner"
	RoleEditor Role = "editor"
	RoleViewer Role = "viewer"
)

var roleRank = map[Role]int{RoleViewer: 1, RoleEditor: 2, RoleOwner: 3}

// AtLeast reports whether r grants at least the privileges of min.
func (r Role) AtLeast(min Role) bool { return roleRank[r] >= roleRank[min] }

type User struct {
	ID           uuid.UUID
	Username     string
	DisplayName  string
	PasswordHash string
	CreatedAt    time.Time
}

type Space struct {
	ID           uuid.UUID
	Name         string
	BaseCurrency string
	// TaxResidency is the owner's country of tax residency (ISO 3166-1 alpha-2),
	// deciding which cost basis rules apply. It belongs to the space because
	// residency is the person's: one country governs every account.
	TaxResidency string
	// FullValuation is where the full valuation starts (decision Р-11).
	FullValuation FullValuation
	CreatedAt     time.Time
}

// FullValuation is which reference prices the full valuation may use: none
// (it equals the liquid one), funds' NAV, or NAV and foreign shares' home
// exchange prices.
type FullValuation string

const (
	FullValuationLiquid        FullValuation = "liquid"
	FullValuationNAV           FullValuation = "nav"
	FullValuationNAVAndForeign FullValuation = "nav_and_foreign"
)

// Valid reports a known value.
func (v FullValuation) Valid() bool {
	return v == FullValuationLiquid || v == FullValuationNAV || v == FullValuationNAVAndForeign
}

// CostBasisRules is what TaxResidency implies (see TaxRulesFor).
func (s Space) CostBasisRules() TaxRules { return TaxRulesFor(s.TaxResidency) }

type Member struct {
	User
	Role Role
}

// Principal is the authenticated caller's identity within a space.
type Principal struct {
	UserID  uuid.UUID
	SpaceID uuid.UUID
	Role    Role
}
