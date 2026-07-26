import { useQuery } from "@tanstack/react-query";
import { api } from "./client";
import type { components } from "./schema";

export type AccountWithBalance = components["schemas"]["AccountWithBalance"];
export type Summary = components["schemas"]["Summary"];
export type AccountType = components["schemas"]["AccountType"];

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
