import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type Valuables = components["schemas"]["Valuables"];
export type ValuablesItem = components["schemas"]["ValuablesItem"];
export type ValuablesItemInput = components["schemas"]["ValuablesItemInput"];
export type ValuablesStorage = components["schemas"]["ValuablesStorage"];
export type ValuablesBankPrice = components["schemas"]["ValuablesBankPrice"];
export type Metal = components["schemas"]["Metal"];
export type ValuablesSold = components["schemas"]["ValuablesSold"];
export type ValuablesSale = components["schemas"]["ValuablesSale"];

// A valuables account's storage, bars and coins (GET /api/v1/accounts/{id}/
// valuables, decisions Р-39, Р-40).
export function useValuables(accountId: string) {
  return useQuery({
    queryKey: ["valuables", accountId],
    queryFn: async (): Promise<Valuables> => {
      const { data, error, response } = await api.GET("/api/v1/accounts/{accountId}/valuables", {
        params: { path: { accountId } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

// What a change to a storage moves: the storage, and every figure that counts
// it — the accounts and the total, the capital chart, the structure and the
// targets.
function useAfterChange(accountId: string) {
  const queryClient = useQueryClient();
  return (data: Valuables) => {
    queryClient.setQueryData(["valuables", accountId], data);
    for (const key of ["accounts", "summary", "capital", "structure", "allocation", "operations", "cashflow", "tax-year"]) {
      void queryClient.invalidateQueries({ queryKey: [key] });
    }
  };
}

export function useSetStorage(accountId: string) {
  const after = useAfterChange(accountId);
  return useMutation({
    mutationFn: async (body: ValuablesStorage): Promise<Valuables> => {
      const { data, error, response } = await api.PUT("/api/v1/accounts/{accountId}/valuables/storage", {
        params: { path: { accountId } },
        body,
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: after,
  });
}

// Adds a piece, or states one anew when id is given.
export function useSaveItem(accountId: string) {
  const after = useAfterChange(accountId);
  return useMutation({
    mutationFn: async ({ id, body }: { id?: string; body: ValuablesItemInput }): Promise<Valuables> => {
      const { data, error, response } = id
        ? await api.PUT("/api/v1/accounts/{accountId}/valuables/items/{itemId}", { params: { path: { accountId, itemId: id } }, body })
        : await api.POST("/api/v1/accounts/{accountId}/valuables/items", { params: { path: { accountId } }, body });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: after,
  });
}

export function useDeleteItem(accountId: string) {
  const after = useAfterChange(accountId);
  return useMutation({
    mutationFn: async (itemId: string): Promise<Valuables> => {
      const { data, error, response } = await api.DELETE("/api/v1/accounts/{accountId}/valuables/items/{itemId}", {
        params: { path: { accountId, itemId } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: after,
  });
}

export function useSetBankPrice(accountId: string) {
  const after = useAfterChange(accountId);
  return useMutation({
    mutationFn: async (body: ValuablesBankPrice): Promise<Valuables> => {
      const { data, error, response } = await api.PUT("/api/v1/accounts/{accountId}/valuables/bank-prices", {
        params: { path: { accountId } },
        body,
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: after,
  });
}

// Records a piece sold, the money to an account when one is named; or takes
// a sale back.
export function useSellItem(accountId: string) {
  const after = useAfterChange(accountId);
  return useMutation({
    mutationFn: async ({ itemId, body }: { itemId: string; body: ValuablesSale | null }): Promise<Valuables> => {
      const { data, error, response } = body
        ? await api.POST("/api/v1/accounts/{accountId}/valuables/items/{itemId}/sale", { params: { path: { accountId, itemId } }, body })
        : await api.DELETE("/api/v1/accounts/{accountId}/valuables/items/{itemId}/sale", { params: { path: { accountId, itemId } } });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: after,
  });
}
