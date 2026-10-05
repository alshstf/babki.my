package family_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/testdb"
)

func newStore(t *testing.T) (*family.Store, context.Context) {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
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

	// A second member, created as production does.
	u2, err := st.CreateUserInSpace(ctx, sp.ID, "kate", "Kate", "hash2", family.RoleEditor)
	if err != nil {
		t.Fatalf("CreateUserInSpace: %v", err)
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

// strp returns the address of s, for partial updates.
func strp(s string) *string { return &s }

// The default base currency, every space reader returning it, and
// UpdateSpaceSettings persisting it (pgx.ErrNoRows for a missing space).
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

	if err := st.UpdateSpaceSettings(ctx, sp.ID, strp("USD"), nil); err != nil {
		t.Fatalf("UpdateSpaceSettings: %v", err)
	}
	got, err = st.SpaceByID(ctx, sp.ID)
	if err != nil {
		t.Fatalf("SpaceByID after update: %v", err)
	}
	if got.BaseCurrency != "USD" {
		t.Fatalf("base_currency after update = %q, want USD", got.BaseCurrency)
	}

	// CreateFirstUserWithSpace refuses a set-up instance, so it gets its own
	// fixture.
	fresh, freshCtx := newStore(t)
	_, sp2, err := fresh.CreateFirstUserWithSpace(freshCtx, "Other", "bob", "Bob", "hash2")
	if err != nil {
		t.Fatalf("CreateFirstUserWithSpace: %v", err)
	}
	if sp2.BaseCurrency != "RUB" {
		t.Fatalf("CreateFirstUserWithSpace base_currency = %q, want RUB", sp2.BaseCurrency)
	}

	// Nonexistent space id: ErrNoRows.
	if err := st.UpdateSpaceSettings(ctx, uuid.New(), strp("EUR"), nil); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("UpdateSpaceSettings on missing space = %v, want pgx.ErrNoRows", err)
	}
}

// Spaces get RU by default and every space reader returns it; an empty country
// would read as unknown.
func TestTaxResidencyColumn(t *testing.T) {
	st, ctx := newStore(t)

	u, err := st.CreateUser(ctx, "alex", "Alex", "hash1")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	sp, err := st.CreateSpaceWithOwner(ctx, "Family", u.ID)
	if err != nil {
		t.Fatalf("CreateSpaceWithOwner: %v", err)
	}
	if sp.TaxResidency != family.DefaultTaxResidency {
		t.Fatalf("CreateSpaceWithOwner tax_residency = %q, want %s (migration default)", sp.TaxResidency, family.DefaultTaxResidency)
	}
	fresh, freshCtx := newStore(t)
	_, sp2, err := fresh.CreateFirstUserWithSpace(freshCtx, "Other", "bob", "Bob", "hash2")
	if err != nil {
		t.Fatalf("CreateFirstUserWithSpace: %v", err)
	}
	if sp2.TaxResidency != family.DefaultTaxResidency {
		t.Fatalf("CreateFirstUserWithSpace tax_residency = %q, want %s", sp2.TaxResidency, family.DefaultTaxResidency)
	}

	// Updating one setting leaves the other alone: nil means unchanged.
	if err := st.UpdateSpaceSettings(ctx, sp.ID, nil, strp("DE")); err != nil {
		t.Fatalf("UpdateSpaceSettings: %v", err)
	}
	got, err := st.SpaceByID(ctx, sp.ID)
	if err != nil {
		t.Fatalf("SpaceByID: %v", err)
	}
	if got.TaxResidency != "DE" || got.BaseCurrency != "RUB" {
		t.Fatalf("after residency-only update = %s/%s, want DE/RUB", got.TaxResidency, got.BaseCurrency)
	}
	if err := st.UpdateSpaceSettings(ctx, sp.ID, strp("EUR"), nil); err != nil {
		t.Fatalf("UpdateSpaceSettings: %v", err)
	}
	if got, err = st.SpaceByID(ctx, sp.ID); err != nil {
		t.Fatalf("SpaceByID: %v", err)
	}
	if got.TaxResidency != "DE" || got.BaseCurrency != "EUR" {
		t.Fatalf("after currency-only update = %s/%s, want DE/EUR", got.TaxResidency, got.BaseCurrency)
	}
}

// Base currencies are deduplicated and sorted; no spaces gives an empty
// slice.
func TestDistinctBaseCurrencies(t *testing.T) {
	st, ctx := newStore(t)

	got, err := st.DistinctBaseCurrencies(ctx)
	if err != nil {
		t.Fatalf("DistinctBaseCurrencies on an instance with no spaces: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("DistinctBaseCurrencies with no spaces = %v, want empty", got)
	}

	u, err := st.CreateUser(ctx, "alex", "Alex", "hash1")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// Two RUB spaces and one USD space.
	if _, err := st.CreateSpaceWithOwner(ctx, "Family", u.ID); err != nil {
		t.Fatalf("CreateSpaceWithOwner Family: %v", err)
	}
	if _, err := st.CreateSpaceWithOwner(ctx, "Second", u.ID); err != nil {
		t.Fatalf("CreateSpaceWithOwner Second: %v", err)
	}
	usdSpace, err := st.CreateSpaceWithOwner(ctx, "Abroad", u.ID)
	if err != nil {
		t.Fatalf("CreateSpaceWithOwner Abroad: %v", err)
	}
	if err := st.UpdateSpaceSettings(ctx, usdSpace.ID, strp("USD"), nil); err != nil {
		t.Fatalf("UpdateSpaceSettings: %v", err)
	}

	got, err = st.DistinctBaseCurrencies(ctx)
	if err != nil {
		t.Fatalf("DistinctBaseCurrencies: %v", err)
	}
	want := []string{"RUB", "USD"}
	if !slices.Equal(got, want) {
		t.Fatalf("DistinctBaseCurrencies = %v, want %v (deduplicated and sorted)", got, want)
	}
}

func TestRoleAtLeast(t *testing.T) {
	if !family.RoleOwner.AtLeast(family.RoleViewer) ||
		!family.RoleEditor.AtLeast(family.RoleEditor) ||
		family.RoleViewer.AtLeast(family.RoleEditor) {
		t.Error("AtLeast ordering broken")
	}
}
