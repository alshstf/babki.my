import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type Allocation = components["schemas"]["Allocation"];
export type AllocationRow = components["schemas"]["AllocationRow"];
export type AllocationMode = components["schemas"]["AllocationMode"];
export type AllocationTargets = components["schemas"]["AllocationTargets"];
export type AllocationTopUp = components["schemas"]["AllocationTopUp"];
export type AllocationRebalance = components["schemas"]["AllocationRebalance"];
export type AllocationSale = components["schemas"]["AllocationSale"];

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

// The money a top-up lays over the kinds: new money, and of the brokers'
// free money what is put in too.
export interface TopUpMoney {
  fresh: number;
  fromBrokers: number;
}

// Where money goes by the targets (GET /api/v1/allocation/topup, variant Б
// of Р-37); asked only for money above zero.
export function useTopUp(money: TopUpMoney | null, valuation: "liquid" | "full") {
  return useQuery({
    queryKey: ["allocation", valuation, "topup", money?.fresh, money?.fromBrokers],
    enabled: money !== null && money.fresh + money.fromBrokers > 0,
    // Another sum asked: the bars keep the last one until it comes.
    placeholderData: keepPreviousData,
    queryFn: async (): Promise<AllocationTopUp> => {
      const { data, error, response } = await api.GET("/api/v1/allocation/topup", {
        params: { query: { amount_minor: money?.fresh ?? 0, from_brokers_minor: money?.fromBrokers ?? 0, valuation } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

// What to sell and buy to the targets (GET /api/v1/allocation/rebalance,
// variant В of Р-37), with new money or none; null asks nothing.
export function useRebalance(fresh: number | null, valuation: "liquid" | "full") {
  return useQuery({ ...rebalanceQuery(fresh ?? 0, valuation), enabled: fresh !== null, placeholderData: keepPreviousData });
}

// rebalanceQuery is the rebalance with fresh new money, for useRebalance
// and for a one-off fetch (the what-if plan filled from it).
export function rebalanceQuery(fresh: number, valuation: "liquid" | "full") {
  return {
    queryKey: ["allocation", valuation, "rebalance", fresh],
    queryFn: async (): Promise<AllocationRebalance> => {
      const { data, error, response } = await api.GET("/api/v1/allocation/rebalance", {
        params: { query: { amount_minor: fresh, valuation } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
  };
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

export type AllocationWhatIf = components["schemas"]["AllocationWhatIf"];
export type AllocationPlanOperation = components["schemas"]["AllocationPlanOperation"];

// What a plan of operations would do against the targets and ceilings
// (POST /api/v1/allocation/what-if, decision Р-48); nothing is written.
export function useWhatIf(valuation: "liquid" | "full") {
  return useMutation({
    mutationFn: async (operations: AllocationPlanOperation[]): Promise<AllocationWhatIf> => {
      const { data, error, response } = await api.POST("/api/v1/allocation/what-if", {
        params: { query: { valuation } },
        body: { operations },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

export type AllocationSavedPlan = components["schemas"]["AllocationSavedPlan"];

// The what-if plans the family kept (decision Р-48, step 3).
export function useSavedPlans() {
  return useQuery({
    queryKey: ["allocation-plans"],
    queryFn: async (): Promise<AllocationSavedPlan[]> => {
      const { data, error, response } = await api.GET("/api/v1/allocation/plans");
      if (!data) throw apiError(response, error);
      return data.plans;
    },
  });
}

// Keeps a plan under a name, replacing one of the same name; the answer is
// the whole list, put straight into the cache.
export function useSavePlan() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: { name: string; operations: AllocationPlanOperation[] }): Promise<AllocationSavedPlan[]> => {
      const { data, error, response } = await api.POST("/api/v1/allocation/plans", { body });
      if (!data) throw apiError(response, error);
      return data.plans;
    },
    onSuccess: (plans) => queryClient.setQueryData(["allocation-plans"], plans),
  });
}

// Forgets a saved plan.
export function useDeletePlan() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (id: string): Promise<AllocationSavedPlan[]> => {
      const { data, error, response } = await api.DELETE("/api/v1/allocation/plans/{planId}", { params: { path: { planId: id } } });
      if (!data) throw apiError(response, error);
      return data.plans;
    },
    onSuccess: (plans) => queryClient.setQueryData(["allocation-plans"], plans),
  });
}
