import { useQuery } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type Forecast = components["schemas"]["Forecast"];
export type ForecastEvent = components["schemas"]["ForecastEvent"];

// The family's everyday money ahead (GET /api/v1/forecast), days from today.
export function useForecast(days: number) {
  return useQuery({
    queryKey: ["forecast", days],
    queryFn: async (): Promise<Forecast> => {
      const { data, error, response } = await api.GET("/api/v1/forecast", { params: { query: { days } } });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}
