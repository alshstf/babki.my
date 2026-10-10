import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type ReceiptLookup = components["schemas"]["ReceiptLookup"];
export type ReceiptNew = components["schemas"]["ReceiptNew"];
export type ReceiptMatch = components["schemas"]["ReceiptMatch"];

export interface ReceiptQuery {
  fn: string;
  fd: string;
  kind: ReceiptNew["kind"];
  total_minor: number;
  issued_at: string;
}

// Where a receipt goes (GET /api/v1/receipts/match): written already, or the
// rows of its total near its day it may complete — the bank's row of the
// same card purchase.
export async function matchReceipt(q: ReceiptQuery): Promise<ReceiptLookup> {
  const { data, error, response } = await api.GET("/api/v1/receipts/match", { params: { query: q } });
  if (!data) throw apiError(response, error);
  return data;
}

// Records a receipt, completing the row operation_id names (POST /api/v1/receipts).
export async function createReceipt(body: ReceiptNew): Promise<void> {
  const { error, response } = await api.POST("/api/v1/receipts", { body });
  if (!response.ok) throw apiError(response, error);
}

export type Receipt = components["schemas"]["Receipt"];
export type ReceiptImportResult = components["schemas"]["ReceiptImportResult"];

// Takes a statement of «Проверка чеков» (POST /api/v1/receipts/import): the
// file as it came in the mail. The journal, the money report and the budget
// are read again — rows may have been split.
export function useImportReceipts() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (statement: unknown): Promise<ReceiptImportResult> => {
      const { data, error, response } = await api.POST("/api/v1/receipts/import", { body: statement as Record<string, never> });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: () => {
      for (const key of ["receipts", "operations", "cashflow", "budget"]) void queryClient.invalidateQueries({ queryKey: [key] });
    },
  });
}

// The receipts waiting for a row (GET /api/v1/receipts?waiting=true).
export function useWaitingReceipts() {
  return useQuery({
    queryKey: ["receipts", "waiting"],
    queryFn: async (): Promise<Receipt[]> => {
      const { data, error, response } = await api.GET("/api/v1/receipts", { params: { query: { waiting: true } } });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

// The receipts of the rows on a page of the journal, by row.
export function useReceiptsFor(operationIds: string[]) {
  const key = [...operationIds].sort().join(",");
  return useQuery({
    queryKey: ["receipts", "rows", key],
    enabled: operationIds.length > 0,
    queryFn: async (): Promise<Map<string, Receipt>> => {
      const { data, error, response } = await api.GET("/api/v1/receipts", { params: { query: { operation_ids: key } } });
      if (!data) throw apiError(response, error);
      return new Map(data.filter((r) => r.operation_id).map((r) => [r.operation_id as string, r]));
    },
  });
}

// Divides the row a receipt completes by the item rules as they are now
// (POST /api/v1/receipts/{id}/split); the journal, the money report and the
// budget are read again.
export function useResplitReceipt() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (receiptId: string): Promise<boolean> => {
      const { data, error, response } = await api.POST("/api/v1/receipts/{receiptId}/split", { params: { path: { receiptId } } });
      if (!data) throw apiError(response, error);
      return data.split;
    },
    onSuccess: () => {
      for (const key of ["operations", "cashflow", "budget"]) void queryClient.invalidateQueries({ queryKey: [key] });
    },
  });
}
