import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import type { components } from "./schema";

export type AccountWithBalance = components["schemas"]["AccountWithBalance"];
export type Summary = components["schemas"]["Summary"];
export type AccountType = components["schemas"]["AccountType"];
export type CreateAccountBody = components["schemas"]["CreateAccountRequest"];
export type UpdateAccountBody = components["schemas"]["UpdateAccountRequest"];

export function useAccounts() {
  return useQuery({
    queryKey: ["accounts"],
    queryFn: async (): Promise<AccountWithBalance[]> => {
      const { data, response } = await api.GET("/api/v1/accounts");
      if (!data) throw new Error(`accounts failed: ${response.status}`);
      return data;
    },
  });
}

export function useSummary() {
  return useQuery({
    queryKey: ["summary"],
    queryFn: async (): Promise<Summary> => {
      const { data, response } = await api.GET("/api/v1/summary");
      if (!data) throw new Error(`summary failed: ${response.status}`);
      return data;
    },
  });
}

function useInvalidateAccounts() {
  const queryClient = useQueryClient();
  return () => {
    void queryClient.invalidateQueries({ queryKey: ["accounts"] });
    void queryClient.invalidateQueries({ queryKey: ["summary"] });
  };
}

export function useCreateAccount() {
  const invalidate = useInvalidateAccounts();
  return useMutation({
    mutationFn: async (body: CreateAccountBody) => {
      const { data, error, response } = await api.POST("/api/v1/accounts", { body });
      if (!data) {
        throw new Error(
          (error as { error?: string } | undefined)?.error ?? `create failed: ${response.status}`,
        );
      }
      return data;
    },
    onSuccess: invalidate,
  });
}

export function useUpdateAccount() {
  const invalidate = useInvalidateAccounts();
  return useMutation({
    mutationFn: async ({ id, body }: { id: string; body: UpdateAccountBody }) => {
      const { data, error, response } = await api.PATCH("/api/v1/accounts/{accountId}", {
        params: { path: { accountId: id } },
        body,
      });
      if (!data) {
        throw new Error(
          (error as { error?: string } | undefined)?.error ?? `update failed: ${response.status}`,
        );
      }
      return data;
    },
    onSuccess: invalidate,
  });
}

export function useArchiveAccount() {
  const invalidate = useInvalidateAccounts();
  return useMutation({
    mutationFn: async (id: string) => {
      const { response, error } = await api.DELETE("/api/v1/accounts/{accountId}", {
        params: { path: { accountId: id } },
      });
      if (!response.ok) {
        throw new Error(
          (error as { error?: string } | undefined)?.error ?? `archive failed: ${response.status}`,
        );
      }
    },
    onSuccess: invalidate,
  });
}
