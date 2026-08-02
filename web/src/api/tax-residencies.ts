import { useQuery } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type CostBasisRules = components["schemas"]["CostBasisRules"];

// The countries this instance has cost basis rules for, each with what those
// rules are and whether the application's own computation matches them.
//
// It comes from the server rather than from a list kept here, and that is the
// entire point of the endpoint: the rules live in ONE table
// (internal/family/taxresidency.go), and a second copy in the frontend would
// drift from it the first time a country is added or corrected there — a
// dropdown offering a country the server rejects, or hiding one it accepts.
// The same objects also carry `supported`/`notices`, so the settings screen
// can state what a country means for the figures BEFORE it is saved.
export function useTaxResidencies(enabled = true) {
  return useQuery({
    queryKey: ["tax-residencies"],
    queryFn: async (): Promise<CostBasisRules[]> => {
      const { data, error, response } = await api.GET("/api/v1/tax-residencies");
      if (!data) throw apiError(response, error);
      return data;
    },
    // The table is compiled into the server: it cannot change while this
    // page is open, only across a deploy.
    staleTime: Infinity,
    enabled,
  });
}
