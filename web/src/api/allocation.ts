import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type Allocation = components["schemas"]["Allocation"];
export type AllocationRow = components["schemas"]["AllocationRow"];
export type AllocationMode = components["schemas"]["AllocationMode"];
export type AllocationTargets = components["schemas"]["AllocationTargets"];

// The family's worth against its target shares (GET /api/v1/allocation,
// decision Р-37), papers at the valuation picked on «Структура».
export function useAllocation(valuation: "liquid" | "full" = "liquid") {
  return useQuery({
    queryKey: ["allocation", valuation],
    queryFn: async (): Promise<Allocation> => {
      const { data, error, response } = await api.GET("/api/v1/allocation", { params: { query: { valuation } } });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

// States the targets of a dimension anew; none clears them.
export function useSetTargets() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: AllocationTargets): Promise<Allocation> => {
      const { data, error, response } = await api.PUT("/api/v1/allocation/targets", { body });
      if (!data) throw apiError(response, error);
      return data;
    },
    // Read again at whichever valuation each view asks.
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["allocation"] }),
  });
}

// How the family brings its worth to the targets.
export function useSetAllocationMode() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (mode: AllocationMode): Promise<Allocation> => {
      const { data, error, response } = await api.PUT("/api/v1/allocation/mode", { body: { mode } });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["allocation"] }),
  });
}
