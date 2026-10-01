package instrument_test

import (
	"encoding/json"
	"io"
	"testing"
)

// TestAnISINIsStoredInOneSpelling: the catalog and the corporate-actions
// registry match an ISIN by string equality, so the spelling a person typed must
// not be the spelling that is stored. Lower case and stray spaces used to make a
// second paper out of the same one (#202).
func TestAnISINIsStoredInOneSpelling(t *testing.T) {
	url, c := newAPI(t)

	resp := do(t, c, "POST", url+"/api/v1/instruments",
		`{"type":"share","name":"Amazon","ticker":"AMZN","isin":" us0231351067 ","currency":"USD"}`)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 201 {
		t.Fatalf("create = %d: %s", resp.StatusCode, body)
	}
	var created struct {
		ID   string `json:"id"`
		ISIN string `json:"isin"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if created.ISIN != "US0231351067" {
		t.Errorf("stored isin = %q, want US0231351067", created.ISIN)
	}

	// The same paper in the other spelling is the same paper, and is refused as
	// the duplicate it is rather than catalogued twice.
	resp = do(t, c, "POST", url+"/api/v1/instruments",
		`{"type":"share","name":"Amazon again","ticker":"AMZN2","isin":"US0231351067","currency":"USD"}`)
	if resp.StatusCode == 201 {
		t.Error("a second row was catalogued under the same ISIN")
	}

	resp = do(t, c, "PATCH", url+"/api/v1/instruments/"+created.ID, `{"isin":"us0378331005"}`)
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("update = %d: %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if created.ISIN != "US0378331005" {
		t.Errorf("updated isin = %q, want US0378331005", created.ISIN)
	}
}

// TestSomethingThatIsNotAnISINIsRefused, on both doors that take one.
func TestSomethingThatIsNotAnISINIsRefused(t *testing.T) {
	url, c := newAPI(t)
	for _, bad := range []string{"AMZN", "US023135106", "US02313510677", "1S0231351067", "US023135106X", "US0231351 67"} {
		resp := do(t, c, "POST", url+"/api/v1/instruments",
			`{"type":"share","name":"X","ticker":"X","isin":"`+bad+`","currency":"USD"}`)
		if resp.StatusCode != 400 {
			t.Errorf("create with isin %q = %d, want 400", bad, resp.StatusCode)
		}
	}

	resp := do(t, c, "POST", url+"/api/v1/instruments", `{"type":"share","name":"Y","ticker":"Y","currency":"USD"}`)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil || resp.StatusCode != 201 {
		t.Fatalf("create without an isin = %d, %v — an instrument need not have one", resp.StatusCode, err)
	}
	resp = do(t, c, "PATCH", url+"/api/v1/instruments/"+created.ID, `{"isin":"not-an-isin"}`)
	if resp.StatusCode != 400 {
		t.Errorf("update with a malformed isin = %d, want 400", resp.StatusCode)
	}
}
