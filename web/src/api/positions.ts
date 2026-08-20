import { useQuery } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type Position = components["schemas"]["Position"];
export type PositionsResponse = components["schemas"]["PositionsResponse"];
// What the account's closed deals locked in, already added up by the server —
// by currency and in the base currency, with the reason when the base one
// could not be struck. The client renders it and adds nothing (see
// RealizedTotal in the API contract).
export type RealizedTotal = components["schemas"]["RealizedTotal"];
export type RealizedGap = components["schemas"]["RealizedGap"];
// What the whole account has made, all in: every position's total plus the
// account's own charges — interest, commissions booked on their own, and the
// tax taken from the account rather than from a payment. Added up by the
// server, with two counts beside it naming the assumptions it rests on (see
// AccountTotal in the API contract).
export type AccountTotal = components["schemas"]["AccountTotal"];
// The money the account holds, one entry per currency, computed from the journal
// — not the balance mark the broker named at the last reconciliation. Cash is a
// holding: it was acquired at some rate and is worth another today (see
// CashPosition in the API contract).
export type CashPosition = components["schemas"]["CashPosition"];
// Which TERM the server could not value, and so why a position carries no
// base-currency figures at all — and, separately, why its market valuation
// carries none of its own. The screen never re-derives either from the
// position's flags or from comparing two currency codes: only the server knows
// which term it actually stopped on (see Position.in_base_gap /
// Position.market_value_gap in the API contract).
export type InBaseGap = components["schemas"]["InBaseGap"];
export type MarketValueGap = components["schemas"]["MarketValueGap"];

// Returns the WHOLE response, not just the positions array: cost_basis_rules
// travels with these figures on purpose (see the API contract) — it says
// whether the cost and profit below are the ones the owner's country's rules
// produce — and a hook that dropped it would leave the screen showing the
// numbers unable to say what they are.
export function usePositions(accountId: string, enabled = true) {
  return useQuery({
    queryKey: ["positions", accountId],
    queryFn: async (): Promise<PositionsResponse> => {
      const { data, error, response } = await api.GET("/api/v1/accounts/{accountId}/positions", {
        params: { path: { accountId } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    enabled,
  });
}
