import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type AccountRates = components["schemas"]["AccountRates"];
export type AccountRate = components["schemas"]["AccountRate"];
export type AccountRateInput = components["schemas"]["AccountRateInput"];

// A savings account's or a deposit's rates, the one in force today and the
// key rate (GET /api/v1/accounts/{id}/rates, decision Р-51 А).
export function useAccountRates(accountId: string) {
  return useQuery({
    queryKey: ["account-rates", accountId],
    queryFn: async (): Promise<AccountRates> => {
      const { data, error, response } = await api.GET("/api/v1/accounts/{accountId}/rates", {
        params: { path: { accountId } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

// A rate moves the goals that grow on the account.
function useAfterChange(accountId: string) {
  const queryClient = useQueryClient();
  return (data: AccountRates) => {
    queryClient.setQueryData(["account-rates", accountId], data);
    void queryClient.invalidateQueries({ queryKey: ["goals"] });
  };
}

export function useSetAccountRate(accountId: string) {
  const after = useAfterChange(accountId);
  return useMutation({
    mutationFn: async (body: AccountRateInput): Promise<AccountRates> => {
      const { data, error, response } = await api.PUT("/api/v1/accounts/{accountId}/rates", {
        params: { path: { accountId } },
        body,
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: after,
  });
}

export function useDeleteAccountRate(accountId: string) {
  const after = useAfterChange(accountId);
  return useMutation({
    mutationFn: async (fromOn: string): Promise<AccountRates> => {
      const { data, error, response } = await api.DELETE("/api/v1/accounts/{accountId}/rates/{fromOn}", {
        params: { path: { accountId, fromOn } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: after,
  });
}
