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
    void queryClient.invalidateQueries({ queryKey: ["account-income"] });
    void queryClient.invalidateQueries({ queryKey: ["tax-year"] });
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

export type DepositProgramme = components["schemas"]["DepositProgramme"];
export type DepositVariant = components["schemas"]["DepositVariant"];
export type ProgrammeCondition = components["schemas"]["ProgrammeCondition"];
export type AccountProgrammeInput = components["schemas"]["AccountProgrammeInput"];
export type ConditionMarkInput = components["schemas"]["ConditionMarkInput"];

// The program's catalog of the banks' programmes (Р-52 Б, Р-53 Б).
export function useDepositProgrammes() {
  return useQuery({
    queryKey: ["deposit-programmes"],
    staleTime: Infinity,
    queryFn: async (): Promise<DepositProgramme[]> => {
      const { data, error, response } = await api.GET("/api/v1/deposit-programmes");
      if (!data) throw apiError(response, error);
      return data.programmes;
    },
  });
}

export function useSetProgramme(accountId: string) {
  const after = useAfterChange(accountId);
  return useMutation({
    mutationFn: async (body: AccountProgrammeInput): Promise<AccountRates> => {
      const { data, error, response } = await api.PUT("/api/v1/accounts/{accountId}/programme", {
        params: { path: { accountId } },
        body,
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: after,
  });
}

export function useDeleteProgramme(accountId: string) {
  const after = useAfterChange(accountId);
  return useMutation({
    mutationFn: async (): Promise<AccountRates> => {
      const { data, error, response } = await api.DELETE("/api/v1/accounts/{accountId}/programme", {
        params: { path: { accountId } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: after,
  });
}

export function useSetMark(accountId: string) {
  const after = useAfterChange(accountId);
  return useMutation({
    mutationFn: async (body: ConditionMarkInput): Promise<AccountRates> => {
      const { data, error, response } = await api.PUT("/api/v1/accounts/{accountId}/marks", {
        params: { path: { accountId } },
        body,
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: after,
  });
}

export function useDeleteMark(accountId: string) {
  const after = useAfterChange(accountId);
  return useMutation({
    mutationFn: async ({ condition, fromOn }: { condition: string; fromOn: string }): Promise<AccountRates> => {
      const { data, error, response } = await api.DELETE("/api/v1/accounts/{accountId}/marks/{condition}/{fromOn}", {
        params: { path: { accountId, condition, fromOn } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: after,
  });
}
