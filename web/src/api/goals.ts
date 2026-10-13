import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type Goals = components["schemas"]["Goals"];
export type Goal = components["schemas"]["Goal"];
export type GoalInput = components["schemas"]["GoalInput"];

// The family's savings goals, each against its target (GET /api/v1/goals,
// decision Р-44).
export function useGoals() {
  return useQuery({
    queryKey: ["goals"],
    queryFn: async (): Promise<Goals> => {
      const { data, error, response } = await api.GET("/api/v1/goals");
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

// Sets a goal down: a new one without id, else that one changed. Every
// answer is the whole list, put straight into the cache.
export function useSaveGoal() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ id, goal }: { id?: string; goal: GoalInput }): Promise<Goals> => {
      const { data, error, response } = id
        ? await api.PUT("/api/v1/goals/{goalId}", { params: { path: { goalId: id } }, body: goal })
        : await api.POST("/api/v1/goals", { body: goal });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: (data) => queryClient.setQueryData(["goals"], data),
  });
}

// Takes a goal away; the money it stood on stays where it is.
export function useDeleteGoal() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (id: string): Promise<Goals> => {
      const { data, error, response } = await api.DELETE("/api/v1/goals/{goalId}", { params: { path: { goalId: id } } });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: (data) => queryClient.setQueryData(["goals"], data),
  });
}
