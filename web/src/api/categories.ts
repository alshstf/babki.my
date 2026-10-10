import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type Category = components["schemas"]["Category"];
export type CategoryKind = components["schemas"]["CategoryKind"];
export type CreateCategoryBody = components["schemas"]["CreateCategoryRequest"];
export type UpdateCategoryBody = components["schemas"]["UpdateCategoryRequest"];

// The family's categories (GET /api/v1/categories); the first look files the
// default set.
export function useCategories() {
  return useQuery({
    queryKey: ["categories"],
    queryFn: async (): Promise<Category[]> => {
      const { data, error, response } = await api.GET("/api/v1/categories");
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

function useInvalidateCategories() {
  const queryClient = useQueryClient();
  return () => void queryClient.invalidateQueries({ queryKey: ["categories"] });
}

export function useCreateCategory() {
  const invalidate = useInvalidateCategories();
  return useMutation({
    mutationFn: async (body: CreateCategoryBody): Promise<Category> => {
      const { data, error, response } = await api.POST("/api/v1/categories", { body });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: invalidate,
  });
}

export function useUpdateCategory() {
  const invalidate = useInvalidateCategories();
  return useMutation({
    mutationFn: async ({ id, body }: { id: string; body: UpdateCategoryBody }): Promise<Category> => {
      const { data, error, response } = await api.PATCH("/api/v1/categories/{categoryId}", {
        params: { path: { categoryId: id } },
        body,
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: invalidate,
  });
}

export function useDeleteCategory() {
  const invalidate = useInvalidateCategories();
  return useMutation({
    mutationFn: async (id: string): Promise<void> => {
      const { error, response } = await api.DELETE("/api/v1/categories/{categoryId}", {
        params: { path: { categoryId: id } },
      });
      if (!response.ok) throw apiError(response, error);
    },
    onSuccess: invalidate,
  });
}

// A category with the ones under it, for a tree two levels deep.
export type CategoryNode = Category & { children: Category[] };

// treeOf arranges one kind's categories: top-level ones in their order, each
// with its children in theirs. A child whose parent is missing (or of another
// kind) stays out rather than float to the top.
export function treeOf(categories: Category[], kind: CategoryKind): CategoryNode[] {
  const ofKind = categories.filter((c) => c.kind === kind);
  const byPosition = (a: Category, b: Category) => a.position - b.position || a.name.localeCompare(b.name, "ru");
  return ofKind
    .filter((c) => c.parent_id === null)
    .sort(byPosition)
    .map((top) => ({ ...top, children: ofKind.filter((c) => c.parent_id === top.id).sort(byPosition) }));
}
