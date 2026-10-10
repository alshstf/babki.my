import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type CreditCard = components["schemas"]["CreditCard"];
export type CreditCardTerms = components["schemas"]["CreditCardTerms"];
export type CreditCardStatus = components["schemas"]["CreditCardStatus"];
export type CreditCardSummary = components["schemas"]["CreditCardSummary"];

// A credit card's terms and what is due on it (GET
// /api/v1/accounts/{id}/credit-card); null while no terms are stated.
export function useCreditCard(accountId: string, enabled: boolean) {
  return useQuery({
    queryKey: ["credit-card", accountId],
    enabled,
    queryFn: async (): Promise<CreditCard | null> => {
      const { data, error, response } = await api.GET("/api/v1/accounts/{accountId}/credit-card", {
        params: { path: { accountId } },
      });
      if (response.status === 404) return null;
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

export function useSetCreditCard(accountId: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: CreditCardTerms): Promise<CreditCard> => {
      const { data, error, response } = await api.PUT("/api/v1/accounts/{accountId}/credit-card", {
        params: { path: { accountId } },
        body,
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["credit-card", accountId] });
      void queryClient.invalidateQueries({ queryKey: ["credit-cards"] });
    },
  });
}

// Every card with terms, for the reminders on the accounts screen.
export function useCreditCards() {
  return useQuery({
    queryKey: ["credit-cards"],
    queryFn: async (): Promise<CreditCardSummary[]> => {
      const { data, error, response } = await api.GET("/api/v1/credit-cards");
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}
