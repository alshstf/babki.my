// What a screen may honestly show about its queries. It has an answer once
// every query holds one, and a failed refetch does not take it away; isError
// alone cannot tell (a waking laptop replaced a screen with «Ошибка»), and
// isLoading is false while paused offline (a screen said «пока нет ни одного
// счёта» about a server nobody asked, #200). The same rule as Gate in
// router.tsx.

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
