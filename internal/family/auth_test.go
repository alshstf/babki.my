package family_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/testdb"
)

func newService(t *testing.T) (*family.Service, context.Context) {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	return family.NewService(family.NewStore(pool)), ctx
}

func TestSetupAndLogin(t *testing.T) {
	svc, ctx := newService(t)

	needed, err := svc.SetupNeeded(ctx)
	if err != nil || !needed {
		t.Fatalf("SetupNeeded = %v, %v; want true", needed, err)
	}

	u, p, err := svc.Setup(ctx, family.SetupParams{
		SpaceName: "Demo", Username: "alex", DisplayName: "Alex", Password: "secret123",
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if p.Role != family.RoleOwner || u.Username != "alex" {
		t.Errorf("setup result: u=%+v p=%+v", u, p)
	}

	// second setup forbidden
	if _, _, err := svc.Setup(ctx, family.SetupParams{
		SpaceName: "X", Username: "b", DisplayName: "B", Password: "12345678",
	}); !errors.Is(err, family.ErrAlreadySetUp) {
		t.Errorf("second Setup err = %v, want ErrAlreadySetUp", err)
	}

	if needed, _ = svc.SetupNeeded(ctx); needed {
		t.Error("SetupNeeded after setup = true")
	}

	// login ok
	if _, lp, err := svc.Login(ctx, "alex", "secret123"); err != nil || lp.Role != family.RoleOwner {
		t.Fatalf("Login: %v, %+v", err, lp)
	}
	// wrong password and unknown user → same error
	if _, _, err := svc.Login(ctx, "alex", "wrong"); !errors.Is(err, family.ErrInvalidCredentials) {
		t.Errorf("wrong password err = %v", err)
	}
	if _, _, err := svc.Login(ctx, "ghost", "secret123"); !errors.Is(err, family.ErrInvalidCredentials) {
		t.Errorf("unknown user err = %v", err)
	}
}

func TestSetupValidation(t *testing.T) {
	svc, ctx := newService(t)
	cases := []family.SetupParams{
		{SpaceName: "S", Username: "Bad Upper", DisplayName: "X", Password: "12345678"},
		{SpaceName: "S", Username: "ab", DisplayName: "X", Password: "12345678"},
		{SpaceName: "S", Username: "okname", DisplayName: "X", Password: "short"},
		{SpaceName: "", Username: "okname", DisplayName: "X", Password: "12345678"},
		{SpaceName: "S", Username: "okname", DisplayName: "", Password: "12345678"},
	}
	for i, c := range cases {
		if _, _, err := svc.Setup(ctx, c); !errors.Is(err, family.ErrValidation) {
			t.Errorf("case %d: err = %v, want ErrValidation", i, err)
		}
	}
}

// Password length is counted in characters, at both doors (#117): a
// seven-letter Cyrillic password is fourteen bytes and used to pass. Byte
// lengths are asserted, not derived from the function under test.
func TestPasswordLengthIsCountedInCharactersNotBytes(t *testing.T) {
	svc, ctx := newService(t)

	// Seven Cyrillic letters, fourteen bytes.
	const sevenChars = "паролям"
	// Eight of them, and the shortest password this door takes.
	const eightChars = "паролями"
	if len(sevenChars) != 14 || len(eightChars) != 16 {
		t.Fatalf("the fixtures are not what this test is about: len(%q) = %d and len(%q) = %d, want 14 and 16 bytes",
			sevenChars, len(sevenChars), eightChars, len(eightChars))
	}

	_, _, err := svc.Setup(ctx, family.SetupParams{
		SpaceName: "S", Username: "alex", DisplayName: "A", Password: sevenChars,
	})
	if !errors.Is(err, family.ErrValidation) {
		t.Fatalf("Setup with a seven-character password err = %v, want ErrValidation: "+
			"%q is 7 characters and 14 bytes, and counting the bytes is what let a password "+
			"the refusal calls too short through the door", err, sevenChars)
	}
	// The message's count is the one applied.
	if !strings.Contains(err.Error(), "at least 8 characters") {
		t.Errorf("Setup refusal = %q, want it to say «at least 8 characters»", err)
	}

	_, owner, err := svc.Setup(ctx, family.SetupParams{
		SpaceName: "S", Username: "alex", DisplayName: "A", Password: eightChars,
	})
	if err != nil {
		t.Fatalf("Setup with an eight-character password: %v — the floor is refused BELOW, not AT", err)
	}

	// The member door applies the same rule.
	if _, err := svc.CreateMember(ctx, owner, "kate", "Kate", sevenChars, family.RoleEditor); !errors.Is(err, family.ErrValidation) {
		t.Errorf("CreateMember with a seven-character password err = %v, want ErrValidation", err)
	}
	if _, err := svc.CreateMember(ctx, owner, "kate", "Kate", eightChars, family.RoleEditor); err != nil {
		t.Errorf("CreateMember with an eight-character password: %v", err)
	}
}

func TestCreateMemberRoles(t *testing.T) {
	svc, ctx := newService(t)
	_, owner, err := svc.Setup(ctx, family.SetupParams{
		SpaceName:   "S",
		Username:    "alex",
		DisplayName: "A",
		Password:    "secret123",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	m, err := svc.CreateMember(ctx, owner, "kate", "Kate", "password9", family.RoleEditor)
	if err != nil || m.Role != family.RoleEditor {
		t.Fatalf("CreateMember: %v, %+v", err, m)
	}

	_, kate, _ := svc.Login(ctx, "kate", "password9")
	// editor cannot create members
	if _, err := svc.CreateMember(ctx, kate, "x", "X", "password9", family.RoleViewer); !errors.Is(err, family.ErrForbidden) {
		t.Errorf("editor CreateMember err = %v, want ErrForbidden", err)
	}
	// owner role cannot be granted
	if _, err := svc.CreateMember(ctx, owner, "boss", "B", "password9", family.RoleOwner); !errors.Is(err, family.ErrValidation) {
		t.Errorf("grant owner err = %v, want ErrValidation", err)
	}
	// empty display name rejected
	if _, err := svc.CreateMember(ctx, owner, "nodisplay", "", "password9", family.RoleEditor); !errors.Is(err, family.ErrValidation) {
		t.Errorf("empty displayName err = %v, want ErrValidation", err)
	}
	// bad username rejected
	if _, err := svc.CreateMember(ctx, owner, "Bad Upper", "X", "password9", family.RoleEditor); !errors.Is(err, family.ErrValidation) {
		t.Errorf("bad username err = %v, want ErrValidation", err)
	}
	// short password rejected
	if _, err := svc.CreateMember(ctx, owner, "shortpw", "X", "short", family.RoleEditor); !errors.Is(err, family.ErrValidation) {
		t.Errorf("short password err = %v, want ErrValidation", err)
	}
}

// A user without a membership gets ErrInvalidCredentials, not the store's
// pgx.ErrNoRows.
func TestLoginOrphanedUser(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store := family.NewStore(pool)
	svc := family.NewService(store)

	hash, err := svc.HashPassword(context.Background(), "secret123")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	// Created through the store, so it has no membership.
	if _, err := store.CreateUser(ctx, "orphan", "Orphan", hash); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if _, _, err := svc.Login(ctx, "orphan", "secret123"); !errors.Is(err, family.ErrInvalidCredentials) {
		t.Errorf("login of orphaned user err = %v, want ErrInvalidCredentials", err)
	}
}

// An unknown residency is refused naming the countries whose rules are known,
// not ones the application "answers for": most of them carry notices.
func TestUnknownCountryRejectionNamesWhatItKnowsNotWhatItAnswersFor(t *testing.T) {
	svc, ctx := newService(t)
	_, owner, err := svc.Setup(ctx, family.SetupParams{
		SpaceName: "S", Username: "alex", DisplayName: "A", Password: "secret123",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	fr := "FR"
	_, err = svc.UpdateSpace(ctx, owner, family.SpaceSettings{TaxResidency: &fr})
	if !errors.Is(err, family.ErrValidation) {
		t.Fatalf("UpdateSpace(FR) err = %v, want ErrValidation", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "knows the rules of") {
		t.Errorf("error = %q, want it to name the list as countries this application KNOWS THE RULES OF", msg)
	}
	if strings.Contains(msg, "can only answer for") {
		t.Errorf("error = %q, still claims the application can answer for every listed country — five of them carry a mismatch notice", msg)
	}
}

// Two simultaneous setups make one owner: the check happens under a lock in
// the inserting transaction.
func TestTwoSetupsAtOnceMakeOneOwner(t *testing.T) {
	pool := testdb.New(t)
	svc := family.NewService(family.NewStore(pool))
	ctx := context.Background()

	const attempts = 6
	errs := make(chan error, attempts)
	for i := range attempts {
		go func() {
			_, _, err := svc.Setup(ctx, family.SetupParams{
				SpaceName: "S", Username: fmt.Sprintf("owner%d", i), DisplayName: "O", Password: "secret123",
			})
			errs <- err
		}()
	}
	succeeded := 0
	for range attempts {
		switch err := <-errs; {
		case err == nil:
			succeeded++
		case !errors.Is(err, family.ErrAlreadySetUp):
			t.Errorf("Setup: %v, want success or ErrAlreadySetUp", err)
		}
	}
	if succeeded != 1 {
		t.Errorf("%d setups succeeded, want exactly 1", succeeded)
	}
	var users, spaces int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM spaces)`).Scan(&users, &spaces); err != nil {
		t.Fatalf("count: %v", err)
	}
	if users != 1 || spaces != 1 {
		t.Errorf("the instance holds %d users and %d spaces, want one of each", users, spaces)
	}
}
