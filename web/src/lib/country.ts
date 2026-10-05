// Renders an ISO alpha-2 code as its Russian name. Formatting, like
// formatDate, not copy: a frontend map of names would be a second country list
// beside the server's (GET /api/v1/tax-residencies); Intl knows every code. Built
// once, since construction is the expensive part.
const regionNames = new Intl.DisplayNames(["ru"], { type: "region" });

// Falls back to the raw code when Intl cannot name it: "GB" is still true.
export function countryName(code: string): string {
  try {
    return regionNames.of(code) ?? code;
  } catch {
    return code;
  }
}
