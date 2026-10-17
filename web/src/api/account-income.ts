import { useQuery } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type AccountIncome = components["schemas"]["AccountIncome"];

// A deposit's or savings account's income by its rate and its share of its
// person's tax on interest (GET /api/v1/accounts/{id}/income, decision Р-56 Б).
export function useAccountIncome(id: string) {
  return useQuery({
    queryKey: ["account-income", id],
    queryFn: async (): Promise<AccountIncome> => {
      const { data, error, response } = await api.GET("/api/v1/accounts/{id}/income", { params: { path: { id } } });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}
