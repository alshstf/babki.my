package family_test

import (
	"context"
	"errors"
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

// TestPasswordLengthIsCountedInCharactersNotBytes is #117's half of the
// password rule, at both doors that apply it.
//
// The refusal has always said «at least 8 characters» and the code counted
// BYTES, so a seven-letter Cyrillic password was fourteen bytes and went
// straight through — the server taking what its own sentence said it would
// not. One of the two had to move, and it was the count (see
// family.MinPasswordRunes), because the sentence is what the person reads and
// because a byte count is not something api/openapi.yaml can state at all:
// `minLength` counts characters, so declaring 8 while the server measured
// bytes would have made the document refuse what the server accepted.
//
// The two literals below are written out, with their byte lengths asserted
// rather than assumed. Deriving either from utf8.RuneCountInString would take
// both sides of the comparison from the very function under test, and a test
// that does that agrees with any counting rule at all.
func TestPasswordLengthIsCountedInCharactersNotBytes(t *testing.T) {
	svc, ctx := newService(t)

	// Seven Cyrillic letters. Fourteen bytes — comfortably past a byte count of
	// eight, which is exactly why it used to be accepted.
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
	// The sentence the person reads, not merely some refusal: naming a count
	// the code does not apply is the whole of #117.
	if !strings.Contains(err.Error(), "at least 8 characters") {
		t.Errorf("Setup refusal = %q, want it to say «at least 8 characters»", err)
	}

	_, owner, err := svc.Setup(ctx, family.SetupParams{
		SpaceName: "S", Username: "alex", DisplayName: "A", Password: eightChars,
	})
	if err != nil {
		t.Fatalf("Setup with an eight-character password: %v — the floor is refused BELOW, not AT", err)
	}

	// The second door applies the same rule; it is a separate call and has its
	// own history of being forgotten.
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

// TestLoginOrphanedUser covers a user row that exists without any
// membership (e.g. left behind by a partial failure). Login must translate
// the underlying pgx.ErrNoRows from MembershipFor into ErrInvalidCredentials
// rather than leaking the raw store error.
func TestLoginOrphanedUser(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store := family.NewStore(pool)
	svc := family.NewService(store)

	hash, err := svc.HashPassword("secret123")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	// Create the user directly via the store, bypassing Setup/CreateMember,
	// so it has no space/membership.
	if _, err := store.CreateUser(ctx, "orphan", "Orphan", hash); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if _, _, err := svc.Login(ctx, "orphan", "secret123"); !errors.Is(err, family.ErrInvalidCredentials) {
		t.Errorf("login of orphaned user err = %v, want ErrInvalidCredentials", err)
	}
}

// TestUnknownCountryRejectionNamesWhatItKnowsNotWhatItAnswersFor is IMPORTANT
// finding 2 from the task-3 review: the rejection for a well-formed but
// unlisted tax residency named its list of countries as ones "this
// application can only answer for". That overclaims — five of those nine
// rows (GB, CA, AU, NL, CH) carry a mismatch notice, so the application
// cannot answer for them either; it can only say, honestly, that it cannot.
// The list must be named for what it actually is: countries whose rules this
// application knows.
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
