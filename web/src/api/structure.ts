import { useQuery } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type Structure = components["schemas"]["Structure"];
export type StructureSlice = components["schemas"]["StructureSlice"];
export type StructureValuation = Structure["valuation"];

// The family's worth taken apart (GET /api/v1/structure).
export function useStructure(valuation: StructureValuation) {
  return useQuery({
    queryKey: ["structure", valuation],
    queryFn: async (): Promise<Structure> => {
      const { data, error, response } = await api.GET("/api/v1/structure", { params: { query: { valuation } } });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}
