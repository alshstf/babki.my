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
	CreatedAt    time.Time
}

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
