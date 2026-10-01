package family_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"babki.my/babki/internal/family"
)

func setUpAlex(t *testing.T, client *http.Client, url string) {
	t.Helper()
	resp := postJSON(t, client, url+"/api/v1/setup",
		`{"space_name":"Демо","username":"alex","display_name":"Alex","password":"secret123"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("setup = %d", resp.StatusCode)
	}
	if resp = postJSON(t, client, url+"/api/v1/auth/logout", ``); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout = %d", resp.StatusCode)
	}
}

func login(t *testing.T, client *http.Client, url, username, password string) *http.Response {
	t.Helper()
	return postJSON(t, client, url+"/api/v1/auth/login",
		`{"username":"`+username+`","password":"`+password+`"}`)
}

// Five wrong passwords in a row are each answered 401; after them the door
// stops looking: the next attempt is a 429 that says how long to wait, and so
// is the RIGHT password, because nothing is compared while the lock stands.
func TestWrongPasswordsLockTheSignInDoor(t *testing.T) {
	ts, client := newAPI(t)
	setUpAlex(t, client, ts.URL)

	for i := range 5 {
		if resp := login(t, client, ts.URL, "alex", "wrong-password"); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong password %d = %d, want 401", i+1, resp.StatusCode)
		}
	}

	resp := login(t, client, ts.URL, "alex", "wrong-password")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the sixth wrong password = %d, want 429", resp.StatusCode)
	}
	seconds, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || seconds < 1 || seconds > 60 {
		t.Errorf("Retry-After = %q, want the seconds left of a one-minute lock", resp.Header.Get("Retry-After"))
	}

	if resp := login(t, client, ts.URL, "alex", "secret123"); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the right password during the lock = %d, want 429 — nothing is compared while it stands", resp.StatusCode)
	}
	if resp, _ := client.Get(ts.URL + "/api/v1/auth/me"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("me = %d after a refused sign-in, want 401", resp.StatusCode)
	}

	// Another username at the same address is not locked: behind a proxy that
	// address is everybody's.
	if resp := login(t, client, ts.URL, "kate", "whatever-it-is"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("another username at the same address = %d, want 401", resp.StatusCode)
	}
}

// Signing in clears the count: four wrong passwords, the right one, and four
// more wrong ones are nine attempts and no lock.
func TestSigningInClearsTheWrongPasswordsBeforeIt(t *testing.T) {
	ts, client := newAPI(t)
	setUpAlex(t, client, ts.URL)

	for range 4 {
		login(t, client, ts.URL, "alex", "wrong-password")
	}
	if resp := login(t, client, ts.URL, "alex", "secret123"); resp.StatusCode != http.StatusOK {
		t.Fatalf("the right password after four wrong ones = %d, want 200", resp.StatusCode)
	}
	for i := range 4 {
		if resp := login(t, client, ts.URL, "alex", "wrong-password"); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong password %d after signing in = %d, want 401", i+1, resp.StatusCode)
		}
	}
}

// A password longer than one can be set to is refused where passwords are set,
// and is an ordinary wrong password where they are checked.
func TestAPasswordHasACeiling(t *testing.T) {
	ts, client := newAPI(t)
	long := strings.Repeat("я", family.MaxPasswordRunes+1)

	resp := postJSON(t, client, ts.URL+"/api/v1/setup",
		`{"space_name":"Демо","username":"alex","display_name":"Alex","password":"`+long+`"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("setup with a password of %d characters = %d, want 400", family.MaxPasswordRunes+1, resp.StatusCode)
	}
	atCeiling := strings.Repeat("я", family.MaxPasswordRunes)
	resp = postJSON(t, client, ts.URL+"/api/v1/setup",
		`{"space_name":"Демо","username":"alex","display_name":"Alex","password":"`+atCeiling+`"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("setup with a password of exactly %d characters = %d, want 201", family.MaxPasswordRunes, resp.StatusCode)
	}
	if resp := login(t, client, ts.URL, "alex", long); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("sign-in with an over-long password = %d, want 401", resp.StatusCode)
	}
	if resp := login(t, client, ts.URL, "alex", atCeiling); resp.StatusCode != http.StatusOK {
		t.Errorf("sign-in with the password at the ceiling = %d, want 200", resp.StatusCode)
	}
}
