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

export type CategoryRule = components["schemas"]["CategoryRule"];
export type CategoryRuleField = components["schemas"]["CategoryRuleField"];
export type CreateCategoryRuleBody = components["schemas"]["CreateCategoryRuleRequest"];
export type UpdateCategoryRuleBody = components["schemas"]["UpdateCategoryRuleRequest"];

// The family's filing rules in the order they are tried.
export function useCategoryRules() {
  return useQuery({
    queryKey: ["category-rules"],
    queryFn: async (): Promise<CategoryRule[]> => {
      const { data, error, response } = await api.GET("/api/v1/category-rules");
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

function useInvalidateRules() {
  const queryClient = useQueryClient();
  return () => void queryClient.invalidateQueries({ queryKey: ["category-rules"] });
}

export function useCreateCategoryRule() {
  const invalidate = useInvalidateRules();
  return useMutation({
    mutationFn: async (body: CreateCategoryRuleBody): Promise<CategoryRule> => {
      const { data, error, response } = await api.POST("/api/v1/category-rules", { body });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: invalidate,
  });
}

export function useUpdateCategoryRule() {
  const invalidate = useInvalidateRules();
  return useMutation({
    mutationFn: async ({ id, body }: { id: string; body: UpdateCategoryRuleBody }): Promise<CategoryRule> => {
      const { data, error, response } = await api.PATCH("/api/v1/category-rules/{ruleId}", {
        params: { path: { ruleId: id } },
        body,
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: invalidate,
  });
}

export function useDeleteCategoryRule() {
  const invalidate = useInvalidateRules();
  return useMutation({
    mutationFn: async (id: string): Promise<void> => {
      const { error, response } = await api.DELETE("/api/v1/category-rules/{ruleId}", {
        params: { path: { ruleId: id } },
      });
      if (!response.ok) throw apiError(response, error);
    },
    onSuccess: invalidate,
  });
}

export function useReorderCategoryRules() {
  const invalidate = useInvalidateRules();
  return useMutation({
    mutationFn: async (ids: string[]): Promise<void> => {
      const { error, response } = await api.PUT("/api/v1/category-rules/order", { body: { ids } });
      if (!response.ok) throw apiError(response, error);
    },
    onSuccess: invalidate,
  });
}

// The text as a rule compares it: case aside, «ё» read as «е» — the server's
// category.fold.
const fold = (s: string) => s.trim().toLocaleLowerCase("ru").replaceAll("ё", "е");

// matchRule is the category the first fitting rule names among the kind's
// active ones, as the server's category.Match decides; undefined when none
// fits. Used to suggest a category while a row is typed in.
export function matchRule(
  rules: CategoryRule[],
  categories: Category[],
  kind: CategoryKind,
  text: { counterparty: string; note: string },
): string | undefined {
  const counterparty = fold(text.counterparty);
  const note = fold(text.note);
  for (const rule of rules) {
    const category = categories.find((c) => c.id === rule.category_id);
    // A rule of a receipt's lines files no row by its own text.
    if (!category || category.kind !== kind || category.archived || rule.field === "item") continue;
    const pattern = fold(rule.pattern);
    const fits =
      rule.field === "counterparty"
        ? counterparty.includes(pattern)
        : rule.field === "note"
          ? note.includes(pattern)
          : counterparty.includes(pattern) || note.includes(pattern);
    if (fits) return rule.category_id;
  }
  return undefined;
}

// rulePattern is the text a rule remembered from a counterparty looks for:
// without the trailing words that carry digits — a shop's number, a card's
// tail — so «ПЯТЕРОЧКА 4411» files every Пятёрочка. The first word stays
// whatever it holds.
export function rulePattern(counterparty: string): string {
  const words = counterparty.trim().split(/\s+/);
  while (words.length > 1 && /\d/.test(words[words.length - 1])) words.pop();
  return words.join(" ");
}
