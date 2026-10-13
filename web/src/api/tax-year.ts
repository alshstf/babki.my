import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type TaxYear = components["schemas"]["TaxYear"];
export type TaxYearPerson = components["schemas"]["TaxYearPerson"];
export type TaxYearAccount = components["schemas"]["TaxYearAccount"];
export type TaxYearWagePayment = components["schemas"]["TaxYearWagePayment"];

// The family's income tax for a year, an estimate (GET /api/v1/tax-year,
// decisions Р-41, Р-42).
export function useTaxYear(year: number) {
  return useQuery({
    queryKey: ["tax-year", year],
    queryFn: async (): Promise<TaxYear> => {
      const { data, error, response } = await api.GET("/api/v1/tax-year", { params: { query: { year } } });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

// A wage payment's gross and withheld tax as its payslip says, over the
// program's reckoning (PUT /api/v1/tax-year/wages/{operationId}); null drops
// what was written, the reckoning stands again (DELETE).
export function useWageEntry() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ operationId, entry }: { operationId: string; entry: { gross_minor: number; withheld_minor: number } | null }) => {
      const path = { params: { path: { operationId } } };
      const { error, response } = entry
        ? await api.PUT("/api/v1/tax-year/wages/{operationId}", { ...path, body: entry })
        : await api.DELETE("/api/v1/tax-year/wages/{operationId}", path);
      if (!response.ok) throw apiError(response, error);
    },
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["tax-year"] }),
  });
}

export type TaxYearForeign = components["schemas"]["TaxYearForeign"];
export type TaxYearForeignAccount = components["schemas"]["TaxYearForeignAccount"];

// The year of each account at a broker abroad line by line, as the tax
// return lists it (GET /api/v1/tax-year/foreign, decision Р-45).
export function useTaxYearForeign(year: number) {
  return useQuery({
    queryKey: ["tax-year", year, "foreign"],
    queryFn: async (): Promise<TaxYearForeign> => {
      const { data, error, response } = await api.GET("/api/v1/tax-year/foreign", { params: { query: { year } } });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

export type TaxYearForeignReport = components["schemas"]["TaxYearForeignReport"];

// The yearly report on the money's moves of each account abroad
// (GET /api/v1/tax-year/foreign-report, decision Р-45).
export function useTaxYearForeignReport(year: number) {
  return useQuery({
    queryKey: ["tax-year", year, "foreign-report"],
    queryFn: async (): Promise<TaxYearForeignReport> => {
      const { data, error, response } = await api.GET("/api/v1/tax-year/foreign-report", { params: { query: { year } } });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}
