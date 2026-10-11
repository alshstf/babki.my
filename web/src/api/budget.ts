import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type Budget = components["schemas"]["Budget"];
export type BudgetLine = components["schemas"]["BudgetLine"];
export type BudgetLimit = components["schemas"]["BudgetLimit"];

// The month's budget, YYYY-MM (GET /api/v1/budget, decision Р-25).
export function useBudget(month: string) {
  return useQuery({
    queryKey: ["budget", month],
    queryFn: async (): Promise<Budget> => {
      const { data, error, response } = await api.GET("/api/v1/budget", { params: { query: { month } } });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

// States a category's limit from a month on; 0 without a копилка takes it
// off. The budget is read again.
export function useSetBudgetLimit() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (limit: BudgetLimit) => {
      const { error, response } = await api.PUT("/api/v1/budget/limits", { body: limit });
      if (!response.ok) throw apiError(response, error);
    },
    onSuccess: () => {
      for (const key of ["budget", "forecast"]) void queryClient.invalidateQueries({ queryKey: [key] });
    },
  });
}
