import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type LoyaltyProgramme = components["schemas"]["LoyaltyProgramme"];
export type CardCashback = components["schemas"]["CardCashback"];
export type CardPicksInput = components["schemas"]["CardPicksInput"];

// The catalog of the banks' loyalty programmes (decision Р-58).
export function useLoyaltyProgrammes(enabled: boolean) {
  return useQuery({
    queryKey: ["loyalty-programmes"],
    enabled,
    queryFn: async (): Promise<LoyaltyProgramme[]> => {
      const { data, error, response } = await api.GET("/api/v1/loyalty-programmes");
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

// A card's programme for the period a day falls in, today without one.
export function useCardCashback(accountId: string, on: string | null) {
  return useQuery({
    queryKey: ["card-cashback", accountId, on],
    queryFn: async (): Promise<CardCashback> => {
      const { data, error, response } = await api.GET("/api/v1/accounts/{accountId}/cashback", {
        params: { path: { accountId }, query: on ? { on } : {} },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

function useAfter(accountId: string) {
  const queryClient = useQueryClient();
  return () => void queryClient.invalidateQueries({ queryKey: ["card-cashback", accountId] });
}

export function useSetCardLoyalty(accountId: string) {
  const after = useAfter(accountId);
  return useMutation({
    mutationFn: async (body: { programme_id: string; variant_id: string }): Promise<CardCashback> => {
      const { data, error, response } = await api.PUT("/api/v1/accounts/{accountId}/cashback/programme", { params: { path: { accountId } }, body });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: after,
  });
}

export function useDeleteCardLoyalty(accountId: string) {
  const after = useAfter(accountId);
  return useMutation({
    mutationFn: async (): Promise<CardCashback> => {
      const { data, error, response } = await api.DELETE("/api/v1/accounts/{accountId}/cashback/programme", { params: { path: { accountId } } });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: after,
  });
}

export function useSetCardPicks(accountId: string) {
  const after = useAfter(accountId);
  return useMutation({
    mutationFn: async (body: CardPicksInput): Promise<CardCashback> => {
      const { data, error, response } = await api.PUT("/api/v1/accounts/{accountId}/cashback/picks", { params: { path: { accountId } }, body });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: after,
  });
}
