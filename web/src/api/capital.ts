import { useQuery } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type CapitalSeries = components["schemas"]["CapitalSeries"];
export type CapitalPoint = components["schemas"]["CapitalPoint"];

// The family's worth at each month's end from `from`, and today.
export function useCapital(from: string) {
  return useQuery({
    queryKey: ["capital", from],
    queryFn: async (): Promise<CapitalSeries> => {
      const { data, error, response } = await api.GET("/api/v1/capital", {
        params: { query: { from, step: "month" } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}
