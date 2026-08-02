import { useQuery } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type Position = components["schemas"]["Position"];
export type PositionsResponse = components["schemas"]["PositionsResponse"];

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
