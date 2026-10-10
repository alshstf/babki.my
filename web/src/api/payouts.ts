import { useQuery } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type PayoutsForecast = components["schemas"]["PayoutsForecast"];
export type Payout = components["schemas"]["Payout"];

// What the papers held today will pay over the next months (GET
// /api/v1/payouts), of one account or of the whole family.
export function usePayouts(months: number, accountId?: string) {
  return useQuery({
    queryKey: ["payouts", months, accountId ?? null],
    queryFn: async (): Promise<PayoutsForecast> => {
      const { data, error, response } = await api.GET("/api/v1/payouts", {
        params: { query: { months, account_id: accountId } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}
