import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type Forecast = components["schemas"]["Forecast"];
export type ForecastEvent = components["schemas"]["ForecastEvent"];

// The family's everyday money ahead (GET /api/v1/forecast), days from today;
// with budget, the spending the budget's limits expect is in it.
export function useForecast(days: number, budget = false) {
  return useQuery({
    queryKey: ["forecast", days, budget],
    // Another horizon or the budget ticked: the card stays as it was until
    // the new forecast comes, rather than going blank.
    placeholderData: keepPreviousData,
    queryFn: async (): Promise<Forecast> => {
      const { data, error, response } = await api.GET("/api/v1/forecast", { params: { query: { days, budget } } });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}
