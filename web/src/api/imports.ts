import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError, useInvalidateJournal } from "./operations";
import type { components } from "./schema";

export type ImportMapping = components["schemas"]["ImportMapping"];
export type ImportPreview = components["schemas"]["ImportPreview"];
export type ImportRow = components["schemas"]["ImportRow"];
export type ImportField = components["schemas"]["ImportField"];
export type TableImport = components["schemas"]["TableImport"];
export type ImportTableResult = components["schemas"]["ImportTableResult"];

// What importing a table would do, row by row. Nothing is written; without a
// mapping the server guesses one from the header and returns it.
export function usePreviewImport(accountId: string) {
  return useMutation({
    mutationFn: async (body: { content: string; mapping?: ImportMapping }): Promise<ImportPreview> => {
      const { data, error, response } = await api.POST(
        "/api/v1/accounts/{accountId}/imports/preview",
        { params: { path: { accountId } }, body },
      );
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

export type ImportPapersResult = components["schemas"]["ImportPapersResult"];

// Files the papers a table names but the catalog lacks, from the exchange.
export function useAddImportPapers() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (codes: string[]): Promise<ImportPapersResult> => {
      const { data, error, response } = await api.POST("/api/v1/imports/papers", { body: { codes } });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["instruments"] }),
  });
}

export function useTableImports(accountId: string) {
  return useQuery({
    queryKey: ["imports", accountId],
    queryFn: async (): Promise<TableImport[]> => {
      const { data, error, response } = await api.GET("/api/v1/accounts/{accountId}/imports", {
        params: { path: { accountId } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

function useInvalidateImports() {
  const queryClient = useQueryClient();
  const invalidateJournal = useInvalidateJournal();
  return (accountId: string) => {
    invalidateJournal([accountId]);
    void queryClient.invalidateQueries({ queryKey: ["imports", accountId] });
  };
}

export function useImportTable(accountId: string) {
  const invalidate = useInvalidateImports();
  return useMutation({
    mutationFn: async (body: {
      content: string;
      mapping: ImportMapping;
      file_name: string;
    }): Promise<ImportTableResult> => {
      const { data, error, response } = await api.POST("/api/v1/accounts/{accountId}/imports", {
        params: { path: { accountId } },
        body,
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: () => invalidate(accountId),
  });
}

export function useRollBackImport(accountId: string) {
  const invalidate = useInvalidateImports();
  return useMutation({
    mutationFn: async (importId: string): Promise<TableImport> => {
      const { data, error, response } = await api.DELETE("/api/v1/imports/{importId}", {
        params: { path: { importId } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: () => invalidate(accountId),
  });
}
