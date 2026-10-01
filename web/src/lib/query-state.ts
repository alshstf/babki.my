// What a screen may honestly show about the queries it draws from.
//
// A screen has an answer to show as soon as every query HOLDS one — and a
// refetch that fails afterwards does not take it away: the data is still there,
// it is merely not fresh. `isError` alone cannot tell those apart, which is how
// a laptop waking from sleep replaced a whole screen with «Ошибка» over data it
// still held; and `isLoading` is false for a query paused offline, which is how
// a screen said «пока нет ни одного счёта» about a server nobody had asked
// (#200). The start-up gate learned both lessons first (see Gate in router.tsx);
// this is the same rule for every other screen.

type Queryish = {
  data: unknown;
  isError: boolean;
  fetchStatus: "fetching" | "paused" | "idle";
};

export type QueryState =
  // Every query holds an answer. It may be stale — see refreshFailed.
  | "ready"
  // An answer is missing and asking for it failed.
  | "failed"
  // An answer is missing and the request has not gone out: the browser reports
  // no network. It goes by itself when the network returns.
  | "offline"
  // An answer is missing and is on its way.
  | "loading";

export function queryState(...queries: Queryish[]): QueryState {
  const missing = queries.filter((q) => q.data === undefined);
  if (missing.length === 0) return "ready";
  if (missing.some((q) => q.isError)) return "failed";
  if (missing.some((q) => q.fetchStatus === "paused")) return "offline";
  return "loading";
}

// refreshFailed reports that what is on screen is an earlier answer: the latest
// attempt to refresh it failed. The screen keeps the data and says so.
export function refreshFailed(...queries: Queryish[]): boolean {
  return queries.some((q) => q.isError && q.data !== undefined);
}
