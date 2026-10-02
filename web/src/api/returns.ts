import { useQuery } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type PeriodReturn = components["schemas"]["PeriodReturn"];

// What an account earned between two days (see GET .../return).
export function useAccountReturn(accountId: string, from: string, to: string) {
  return useQuery({
    queryKey: ["return", accountId, from, to],
    queryFn: async (): Promise<PeriodReturn> => {
      const { data, error, response } = await api.GET("/api/v1/accounts/{accountId}/return", {
        params: { path: { accountId }, query: { from, to } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}
