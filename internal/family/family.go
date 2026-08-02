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

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return roleRank[r] != 0 }

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
	// TaxResidency is the OWNER'S country of tax residency, ISO 3166-1
	// alpha-2. It decides which cost basis rules the space's figures must be
	// judged against (see TaxRulesFor).
	//
	// It sits on the space rather than on the account because residency is a
	// property of the person: a Russian resident declares a foreign broker's
	// account by Russian rules just the same, so one country governs every
	// account in the family. A per-account country would let one family claim
	// several rulebooks at once, which no tax authority recognises.
	TaxResidency string
	CreatedAt    time.Time
}

// CostBasisRules is what TaxResidency implies, resolved through the one table
// in taxresidency.go.
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
