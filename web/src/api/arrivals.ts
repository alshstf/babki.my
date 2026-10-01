import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError, useInvalidateJournal } from "./operations";
import type { components } from "./schema";

export type Arrival = components["schemas"]["Arrival"];
export type StatedPurchase = components["schemas"]["StatedPurchase"];
export type CreateArrivalBody = components["schemas"]["CreateArrivalRequest"];

// useArrivals lists the occasions on which shares of a paper reached an account
// by a transfer, each with the purchases recorded behind it — what the screen
// asking for the price of shares that arrived without one reads.
export function useArrivals(accountId: string, instrumentId: string, enabled: boolean) {
  return useQuery({
    queryKey: ["arrivals", accountId, instrumentId],
    enabled,
    queryFn: async (): Promise<Arrival[]> => {
      const { data, error, response } = await api.GET(
        "/api/v1/accounts/{accountId}/instruments/{instrumentId}/arrivals",
        { params: { path: { accountId, instrumentId } } },
      );
      if (!data) throw apiError(response, error);
      return data.arrivals;
    },
  });
}

// Everything a change of an arrival's basis reaches: the journal, the
// positions and totals, and the list of arrivals itself.
function useInvalidateArrivals() {
  const queryClient = useQueryClient();
  const invalidateJournal = useInvalidateJournal();
  return (accountId: string) => {
    invalidateJournal([accountId]);
    void queryClient.invalidateQueries({ queryKey: ["arrivals", accountId] });
  };
}

// useStatePurchases states the purchases behind shares that arrived from
// another broker. The cost of each is struck by the server from the price.
export function useStatePurchases(accountId: string) {
  const invalidate = useInvalidateArrivals();
  return useMutation({
    mutationFn: async (vars: { operationId: string; purchases: StatedPurchase[] }) => {
      const { data, error, response } = await api.PUT("/api/v1/operations/{operationId}/purchases", {
        params: { path: { operationId: vars.operationId } },
        body: { purchases: vars.purchases },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: () => invalidate(accountId),
  });
}

// useCreateArrival records shares arriving from another broker by hand.
export function useCreateArrival() {
  const invalidate = useInvalidateArrivals();
  return useMutation({
    mutationFn: async (body: CreateArrivalBody) => {
      const { data, error, response } = await api.POST("/api/v1/operations/arrivals", { body });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: (_data, body) => invalidate(body.account_id),
  });
}
