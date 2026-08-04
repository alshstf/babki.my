// Runtime backstop for a gap value this build cannot name. Shared by
// operations-table.tsx and positions-table.tsx, whose `in_base_gap` /
// `market_value_gap` switches each read a different enum from the API
// contract but hit exactly the same problem: TypeScript proves a switch over
// such a union exhaustive at compile time — a NEW member added to the
// contract's enum without a case there fails to compile, because the
// argument would no longer narrow to `never`. But these values are JSON off
// the wire, typed by assertion rather than validated, so a client running
// behind the server it talks to can still receive a literal outside that
// union; that must degrade to a sentence that is still true, never to a
// blank tooltip and never to a thrown render.
//
// The fallback is the caller's, never chosen here, because what "still true"
// means differs between call sites: the general phrase for a whole row is
// not the same string as the fallback a nearer, more specific cell wants (see
// valuationGapTitle in positions-table.tsx, which falls back to the ROW's
// sentence rather than to its own general one).
//
// Kept as one function rather than two copies for the reason this project
// names as a defect everywhere it finds it: two independent computations of
// one thing eventually drift. This one never computes anything — it only
// returns what it was handed — so there is nothing here FOR two copies to
// disagree about yet, but a future edit (a log line, a metric) added to one
// copy and not the other would be exactly that drift, silently.
export function unnameableGap<T>(_: never, fallback: T): T {
  return fallback;
}
