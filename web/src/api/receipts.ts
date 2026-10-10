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
