import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type DepositComparison = components["schemas"]["DepositComparison"];
export type DepositComparisonRow = components["schemas"]["DepositComparisonRow"];
export type DepositComparisonCondition = components["schemas"]["DepositComparisonCondition"];
export type DepositComparisonInput = components["schemas"]["DepositComparisonInput"];
export type DepositConditionChoice = components["schemas"]["DepositConditionChoice"];

// The catalog's deposits compared on a sum over a horizon (POST
// /api/v1/deposit-comparison, decision Р-57 Б). It keeps nothing: a query,
// the last answer shown while the next is reckoned.
export function useDepositComparison(input: DepositComparisonInput | null) {
  return useQuery({
    queryKey: ["deposit-comparison", input],
    enabled: input !== null,
    placeholderData: keepPreviousData,
    queryFn: async (): Promise<DepositComparison> => {
      const { data, error, response } = await api.POST("/api/v1/deposit-comparison", { body: input! });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}
