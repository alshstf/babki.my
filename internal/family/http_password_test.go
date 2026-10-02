package family_test

import (
	"net/http"
	"net/http/cookiejar"
	"testing"
)

func device(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func meStatus(t *testing.T, c *http.Client, url string) int {
	t.Helper()
	resp, err := c.Get(url + "/api/v1/auth/me")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// A new password ends every other session of the user — a laptop left signed
// in elsewhere — while the one that made the change goes on; the old password
// no longer signs in and the new one does.
func TestChangingThePasswordEndsTheOtherSessions(t *testing.T) {
	ts, here := newAPI(t)
	setUpAlex(t, here, ts.URL)
	if resp := login(t, here, ts.URL, "alex", "secret123"); resp.StatusCode != http.StatusOK {
		t.Fatalf("sign-in = %d", resp.StatusCode)
	}
	there := device(t)
	if resp := login(t, there, ts.URL, "alex", "secret123"); resp.StatusCode != http.StatusOK {
		t.Fatalf("second device sign-in = %d", resp.StatusCode)
	}

	resp := postJSON(t, here, ts.URL+"/api/v1/auth/password", `{"current_password":"secret123","new_password":"новый-пароль"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("change = %d, want 204", resp.StatusCode)
	}
	if code := meStatus(t, here, ts.URL); code != http.StatusOK {
		t.Errorf("the session that changed it = %d, want it to go on", code)
	}
	if code := meStatus(t, there, ts.URL); code != http.StatusUnauthorized {
		t.Errorf("the other device = %d, want its session over", code)
	}
	if resp := login(t, device(t), ts.URL, "alex", "secret123"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the old password = %d, want 401", resp.StatusCode)
	}
	if resp := login(t, device(t), ts.URL, "alex", "новый-пароль"); resp.StatusCode != http.StatusOK {
		t.Errorf("the new password = %d, want 200", resp.StatusCode)
	}
}

// A wrong current password changes nothing and is the field's fault, not the
// session's; five of them close the door as five wrong sign-ins do. A new
// password outside the rule is refused before anything is checked.
func TestAPasswordIsChangedOnlyWithTheCurrentOne(t *testing.T) {
	ts, here := newAPI(t)
	setUpAlex(t, here, ts.URL)
	if resp := login(t, here, ts.URL, "alex", "secret123"); resp.StatusCode != http.StatusOK {
		t.Fatalf("sign-in = %d", resp.StatusCode)
	}
	if resp := postJSON(t, here, ts.URL+"/api/v1/auth/password", `{"current_password":"secret123","new_password":"short"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a new password under eight = %d, want 400", resp.StatusCode)
	}
	for i := range 5 {
		if resp := postJSON(t, here, ts.URL+"/api/v1/auth/password", `{"current_password":"wrong-one","new_password":"новый-пароль"}`); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("wrong current password #%d = %d, want 400", i+1, resp.StatusCode)
		}
	}
	if code := meStatus(t, here, ts.URL); code != http.StatusOK {
		t.Errorf("after wrong attempts the session = %d, want it untouched", code)
	}
	if resp := postJSON(t, here, ts.URL+"/api/v1/auth/password", `{"current_password":"secret123","new_password":"новый-пароль"}`); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("after five wrong = %d, want 429", resp.StatusCode)
	}
}

// Signing out everywhere else ends the other sessions and keeps this one.
func TestSigningOutElsewhereKeepsThisSession(t *testing.T) {
	ts, here := newAPI(t)
	setUpAlex(t, here, ts.URL)
	if resp := login(t, here, ts.URL, "alex", "secret123"); resp.StatusCode != http.StatusOK {
		t.Fatalf("sign-in = %d", resp.StatusCode)
	}
	there := device(t)
	if resp := login(t, there, ts.URL, "alex", "secret123"); resp.StatusCode != http.StatusOK {
		t.Fatalf("second device sign-in = %d", resp.StatusCode)
	}
	if resp := postJSON(t, here, ts.URL+"/api/v1/auth/sign-out-elsewhere", `{}`); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("sign out elsewhere = %d", resp.StatusCode)
	}
	if code := meStatus(t, here, ts.URL); code != http.StatusOK {
		t.Errorf("this session = %d, want it to go on", code)
	}
	if code := meStatus(t, there, ts.URL); code != http.StatusUnauthorized {
		t.Errorf("the other session = %d, want 401", code)
	}
	if resp := login(t, there, ts.URL, "alex", "secret123"); resp.StatusCode != http.StatusOK {
		t.Errorf("signing in again = %d, want 200", resp.StatusCode)
	}
	if code := meStatus(t, there, ts.URL); code != http.StatusOK {
		t.Errorf("a session signed in afterwards = %d, want 200", code)
	}
}
