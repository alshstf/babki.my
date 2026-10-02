package corporateaction_test

import (
	"net/http"
	"strings"
	"testing"

	"babki.my/babki/internal/corporateaction"
)

// A hand-recorded event's evidence link and note are bounded in characters:
// one past either ceiling is refused and nothing reaches the registry, and
// both at their ceilings — in Cyrillic, two bytes a character — go through.
func TestAHandRecordedEventsTextsHaveACeiling(t *testing.T) {
	f := newAPIFixture(t)
	event := func(sourceRef, note string) string {
		return `{"kind":"split","isin":"` + amazonISIN + `","effective_on":"2022-06-06","ratio_from":1,"ratio_to":20,` +
			`"source_ref":"` + sourceRef + `","note":"` + note + `"}`
	}
	ref := strings.Repeat("ж", corporateaction.MaxSourceRefRunes)
	note := strings.Repeat("ж", corporateaction.MaxNoteRunes)

	for name, body := range map[string]string{
		"source_ref": event(ref+"ж", "x"),
		"note":       event("https://ir.aboutamazon.com/", note+"ж"),
	} {
		if resp, got := f.do(t, http.MethodPost, "/api/v1/instrument-events", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("an event with a %s one character too long = %d, want 400: %s", name, resp.StatusCode, got)
		}
	}
	events, err := f.store.List(f.ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("the registry holds %d events after refused requests, want none", len(events))
	}
	if resp, got := f.do(t, http.MethodPost, "/api/v1/instrument-events", event(ref, note)); resp.StatusCode != http.StatusCreated {
		t.Errorf("an event with both texts at their ceilings = %d, want 201: %s", resp.StatusCode, got)
	}
}
