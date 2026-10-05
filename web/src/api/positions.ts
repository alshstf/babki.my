import { useQuery } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type Position = components["schemas"]["Position"];
export type PositionsResponse = components["schemas"]["PositionsResponse"];
// The account's realized total, added up by the server, with the reason when
// the base figure could not be struck.
export type RealizedTotal = components["schemas"]["RealizedTotal"];
export type RealizedGap = components["schemas"]["RealizedGap"];
// What the account has made, all in: positions plus the account's own charges,
// added up by the server with the counts of its assumptions.
export type AccountTotal = components["schemas"]["AccountTotal"];
// The account's money per currency, computed from the journal, not the
// broker's balance mark; a holding with a cost and a value.
export type CashPosition = components["schemas"]["CashPosition"];
// Which term the server could not value, for the row and for the valuation;
// never re-derived on screen (Position.in_base_gap, market_value_gap).
export type InBaseGap = components["schemas"]["InBaseGap"];
export type MarketValueGap = components["schemas"]["MarketValueGap"];

// The whole response: cost_basis_rules travels with the figures and says
// what they are.
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
