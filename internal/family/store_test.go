package family_test

import (
	"context"
	"testing"

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

func TestRoleAtLeast(t *testing.T) {
	if !family.RoleOwner.AtLeast(family.RoleViewer) ||
		!family.RoleEditor.AtLeast(family.RoleEditor) ||
		family.RoleViewer.AtLeast(family.RoleEditor) {
		t.Error("AtLeast ordering broken")
	}
}
