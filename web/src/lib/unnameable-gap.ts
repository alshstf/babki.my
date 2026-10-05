// Runtime backstop for a gap value this build cannot name, shared by the
// operations and positions tables. TypeScript proves their switches exhaustive,
// but the values are unvalidated JSON, so a newer server's value must degrade to
// a true sentence, not a blank or a crash. The fallback is the caller's, since
// what is still true differs by cell.
export function unnameableGap<T>(_: never, fallback: T): T {
  return fallback;
}
