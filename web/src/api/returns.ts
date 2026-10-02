import { useQuery } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type PeriodReturn = components["schemas"]["PeriodReturn"];
export type FamilyReturn = components["schemas"]["FamilyReturn"];

// What an account earned between two days (see GET .../return).
export function useAccountReturn(accountId: string, from: string, to: string, enabled = true) {
  return useQuery({
    queryKey: ["return", accountId, from, to],
    enabled,
    queryFn: async (): Promise<PeriodReturn> => {
      const { data, error, response } = await api.GET("/api/v1/accounts/{accountId}/return", {
        params: { path: { accountId }, query: { from, to } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

// What the family's brokerage accounts earned between two days (see GET
// /api/v1/return).
export function useFamilyReturn(from: string, to: string, enabled = true) {
  return useQuery({
    queryKey: ["return", "family", from, to],
    enabled,
    queryFn: async (): Promise<FamilyReturn> => {
      const { data, error, response } = await api.GET("/api/v1/return", {
        params: { query: { from, to } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

// What one paper earned the family between two days, across every account it
// has been on (see GET /api/v1/instruments/{instrumentId}/return).
export function useInstrumentReturn(instrumentId: string, from: string, to: string) {
  return useQuery({
    queryKey: ["return", "instrument", instrumentId, from, to],
    queryFn: async (): Promise<PeriodReturn> => {
      const { data, error, response } = await api.GET("/api/v1/instruments/{instrumentId}/return", {
        params: { path: { instrumentId }, query: { from, to } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}
