import { useQuery } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type TaxYear = components["schemas"]["TaxYear"];
export type TaxYearPerson = components["schemas"]["TaxYearPerson"];
export type TaxYearAccount = components["schemas"]["TaxYearAccount"];

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
