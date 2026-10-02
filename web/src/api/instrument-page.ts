import { useQuery } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type InstrumentHoldings = components["schemas"]["InstrumentHoldings"];
export type InstrumentHolding = components["schemas"]["InstrumentHolding"];
export type DayPrice = components["schemas"]["DayPrice"];

// One paper across the family: its catalog row and its position on every
// account whose journal names it, each the row that account's positions screen
// shows (see GET /api/v1/instruments/{instrumentId}/holdings).
export function useInstrumentHoldings(instrumentId: string) {
  return useQuery({
    queryKey: ["instrument-holdings", instrumentId],
    queryFn: async (): Promise<InstrumentHoldings> => {
      const { data, error, response } = await api.GET("/api/v1/instruments/{instrumentId}/holdings", {
        params: { path: { instrumentId } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

// The paper's daily prices from `from` to today, oldest first.
export function useInstrumentPrices(instrumentId: string, from: string) {
  return useQuery({
    queryKey: ["instrument-prices", instrumentId, from],
    queryFn: async (): Promise<DayPrice[]> => {
      const { data, error, response } = await api.GET("/api/v1/instruments/{instrumentId}/prices", {
        params: { path: { instrumentId }, query: { from } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}
