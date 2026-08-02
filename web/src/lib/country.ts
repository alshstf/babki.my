// Renders an ISO 3166-1 alpha-2 country code as its Russian name.
//
// This is formatting, not copy: the same class of thing as formatMinor and
// formatDate, which also turn a backend value into Russian without going
// through t(). The alternative — a { RU: "Россия", GB: "Великобритания", … }
// map in the frontend — would be a SECOND list of countries beside the
// server's table (see internal/family/taxresidency.go), and the whole reason
// the server publishes GET /api/v1/tax-residencies is that a client's own
// copy drifts from it the first time a country is added there. Intl knows
// every alpha-2 code there is, so it cannot drift.
//
// The formatter is built once: constructing an Intl.DisplayNames is the
// expensive part, and a settings dropdown asks it for a name per option on
// every render.
const regionNames = new Intl.DisplayNames(["ru"], { type: "region" });

// countryName falls back to the raw code for anything Intl cannot name — a
// structurally invalid code makes `of` throw, and an unassigned one makes it
// return the input. "GB" on screen is worse than "Великобритания" but it is
// still the truth; inventing a name, or rendering an empty cell in a
// dropdown the owner has to pick their country from, would not be.
export function countryName(code: string): string {
  try {
    return regionNames.of(code) ?? code;
  } catch {
    return code;
  }
}
