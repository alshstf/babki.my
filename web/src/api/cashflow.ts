import { useQuery } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type CashflowReport = components["schemas"]["CashflowReport"];
export type CashflowLine = components["schemas"]["CashflowLine"];
export type CashflowSection = components["schemas"]["CashflowSection"];
export type CashflowFlow = components["schemas"]["CashflowFlow"];

// What a report covers: the days, both included, and whose accounts — a
// member's id, "shared", or the whole family when absent.
export type CashflowQuery = { from: string; to: string; member?: string };

// The family's money over a period (GET /api/v1/cashflow). Read again with
// the journal: a filed or entered row changes it.
export function useCashflow(query: CashflowQuery) {
  return useQuery({
    queryKey: ["cashflow", query],
    queryFn: async (): Promise<CashflowReport> => {
      const { data, error, response } = await api.GET("/api/v1/cashflow", {
        params: { query: { from: query.from, to: query.to, member: query.member } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}
