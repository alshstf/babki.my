package family_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/testdb"
)

func newStore(t *testing.T) (*family.Store, context.Context) {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return family.NewStore(pool), ctx
}

func TestUserAndSpaceLifecycle(t *testing.T) {
	st, ctx := newStore(t)

	n, err := st.CountUsers(ctx)
	if err != nil || n != 0 {
		t.Fatalf("CountUsers = %d, %v; want 0, nil", n, err)
	}

	u, err := st.CreateUser(ctx, "alex", "Alex", "hash1")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	sp, err := st.CreateSpaceWithOwner(ctx, "Family", u.ID)
	if err != nil {
		t.Fatalf("CreateSpaceWithOwner: %v", err)
	}

	p, err := st.MembershipFor(ctx, u.ID)
	if err != nil {
		t.Fatalf("MembershipFor: %v", err)
	}
	if p.SpaceID != sp.ID || p.Role != family.RoleOwner || p.UserID != u.ID {
		t.Errorf("principal = %+v", p)
	}

	// second member
	u2, err := st.CreateUser(ctx, "kate", "Kate", "hash2")
	if err != nil {
		t.Fatalf("CreateUser 2: %v", err)
	}
	if err := st.AddMember(ctx, sp.ID, u2.ID, family.RoleEditor); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	members, err := st.ListMembers(ctx, sp.ID)
	if err != nil || len(members) != 2 {
		t.Fatalf("ListMembers = %d, %v; want 2", len(members), err)
	}

	if err := st.UpdateMemberRole(ctx, sp.ID, u2.ID, family.RoleViewer); err != nil {
		t.Fatalf("UpdateMemberRole: %v", err)
	}
	p2, _ := st.MembershipFor(ctx, u2.ID)
	if p2.Role != family.RoleViewer {
		t.Errorf("role after update = %s", p2.Role)
	}

	if err := st.RemoveMember(ctx, sp.ID, u2.ID); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if members, _ = st.ListMembers(ctx, sp.ID); len(members) != 1 {
		t.Errorf("members after remove = %d", len(members))
	}

	// duplicate username must fail
	if _, err := st.CreateUser(ctx, "alex", "Dup", "h"); err == nil {
		t.Error("duplicate username: want error")
	}
}

// TestBaseCurrency verifies the default, that all Space-scanning methods
// return base_currency, and that UpdateBaseCurrency persists the change and
// reports pgx.ErrNoRows for a space that doesn't exist.
func TestBaseCurrency(t *testing.T) {
	st, ctx := newStore(t)

	u, err := st.CreateUser(ctx, "alex", "Alex", "hash1")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	sp, err := st.CreateSpaceWithOwner(ctx, "Family", u.ID)
	if err != nil {
		t.Fatalf("CreateSpaceWithOwner: %v", err)
	}
	if sp.BaseCurrency != "RUB" {
		t.Fatalf("CreateSpaceWithOwner base_currency = %q, want RUB (migration default)", sp.BaseCurrency)
	}

	got, err := st.SpaceByID(ctx, sp.ID)
	if err != nil {
		t.Fatalf("SpaceByID: %v", err)
	}
	if got.BaseCurrency != "RUB" {
		t.Fatalf("SpaceByID base_currency = %q, want RUB", got.BaseCurrency)
	}

	if err := st.UpdateBaseCurrency(ctx, sp.ID, "USD"); err != nil {
		t.Fatalf("UpdateBaseCurrency: %v", err)
	}
	got, err = st.SpaceByID(ctx, sp.ID)
	if err != nil {
		t.Fatalf("SpaceByID after update: %v", err)
	}
	if got.BaseCurrency != "USD" {
		t.Fatalf("base_currency after update = %q, want USD", got.BaseCurrency)
	}

	// CreateFirstUserWithSpace also scans base_currency.
	_, sp2, err := st.CreateFirstUserWithSpace(ctx, "Other", "bob", "Bob", "hash2")
	if err != nil {
		t.Fatalf("CreateFirstUserWithSpace: %v", err)
	}
	if sp2.BaseCurrency != "RUB" {
		t.Fatalf("CreateFirstUserWithSpace base_currency = %q, want RUB", sp2.BaseCurrency)
	}

	// Nonexistent space id: ErrNoRows.
	if err := st.UpdateBaseCurrency(ctx, uuid.New(), "EUR"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("UpdateBaseCurrency on missing space = %v, want pgx.ErrNoRows", err)
	}
}

func TestRoleAtLeast(t *testing.T) {
	if !family.RoleOwner.AtLeast(family.RoleViewer) ||
		!family.RoleEditor.AtLeast(family.RoleEditor) ||
		family.RoleViewer.AtLeast(family.RoleEditor) {
		t.Error("AtLeast ordering broken")
	}
}
