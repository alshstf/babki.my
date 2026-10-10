import { useQuery } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type RecurringPayment = components["schemas"]["RecurringPayment"];

// The regular payments the journals show (GET /api/v1/recurring).
export function useRecurring() {
  return useQuery({
    queryKey: ["recurring"],
    queryFn: async (): Promise<RecurringPayment[]> => {
      const { data, error, response } = await api.GET("/api/v1/recurring");
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}
