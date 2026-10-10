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

export type CreditCardInstallmentPlan = components["schemas"]["CreditCardInstallmentPlan"];

// Puts a purchase on the card in installments, or restates its plan
// (decision Р-33).
export function useSetInstallment(accountId: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ operationId, plan }: { operationId: string; plan: CreditCardInstallmentPlan }): Promise<CreditCard> => {
      const { data, error, response } = await api.PUT("/api/v1/accounts/{accountId}/credit-card/installments/{operationId}", {
        params: { path: { accountId, operationId } },
        body: plan,
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

export function useDeleteInstallment(accountId: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (operationId: string): Promise<void> => {
      const { error, response } = await api.DELETE("/api/v1/accounts/{accountId}/credit-card/installments/{operationId}", {
        params: { path: { accountId, operationId } },
      });
      if (!response.ok) throw apiError(response, error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["credit-card", accountId] });
      void queryClient.invalidateQueries({ queryKey: ["credit-cards"] });
    },
  });
}

export type CreditCardBankFigures = components["schemas"]["CreditCardBankFigures"];

// States what the bank itself says is due on the card (decision Р-32).
export function useSetBankFigures(accountId: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: CreditCardBankFigures): Promise<CreditCard> => {
      const { data, error, response } = await api.PUT("/api/v1/accounts/{accountId}/credit-card/bank", {
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

export function useDeleteBankFigures(accountId: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (): Promise<void> => {
      const { error, response } = await api.DELETE("/api/v1/accounts/{accountId}/credit-card/bank", {
        params: { path: { accountId } },
      });
      if (!response.ok) throw apiError(response, error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["credit-card", accountId] });
      void queryClient.invalidateQueries({ queryKey: ["credit-cards"] });
    },
  });
}

export type CreditCardCatalogProduct = components["schemas"]["CreditCardCatalogProduct"];
export type CreditCardCatalogVersion = components["schemas"]["CreditCardCatalogVersion"];

// The catalog of tariffs built into the program (decision Р-32).
export function useCardCatalog(enabled = true) {
  return useQuery({
    queryKey: ["credit-card-catalog"],
    enabled,
    staleTime: Infinity,
    queryFn: async (): Promise<CreditCardCatalogProduct[]> => {
      const { data, error, response } = await api.GET("/api/v1/credit-cards/catalog");
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

// mergeTerms lays a catalog version's fields over a card's terms, the nested
// ones (fees, cashback, installment) field by field.
export function mergeTerms(base: CreditCardTerms, over: Record<string, unknown>): CreditCardTerms {
  const out: Record<string, unknown> = { ...base };
  for (const [k, v] of Object.entries(over)) {
    const into = out[k];
    out[k] =
      v && typeof v === "object" && !Array.isArray(v) && into && typeof into === "object"
        ? { ...(into as Record<string, unknown>), ...(v as Record<string, unknown>) }
        : v;
  }
  return out as CreditCardTerms;
}

// versionFor is the version of a catalog card for a contract made on day
// (YYYY-MM-DD): the one whose bounds hold it; with no day, the only one.
export function versionFor(product: CreditCardCatalogProduct, day: string): CreditCardCatalogVersion | undefined {
  if (!day) return product.versions.length === 1 ? product.versions[0] : undefined;
  return product.versions.find(
    (v) => (v.contracts_from === null || v.contracts_from <= day) && (v.contracts_to === null || day <= v.contracts_to),
  );
}
