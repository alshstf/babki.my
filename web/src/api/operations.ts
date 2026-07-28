import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import type { components } from "./schema";

export type Operation = components["schemas"]["Operation"];
export type OperationType = components["schemas"]["OperationType"];
export type CreateOperationBody = components["schemas"]["CreateOperationRequest"];
export type TransferBody = components["schemas"]["TransferRequest"];
export type TransferResponse = components["schemas"]["TransferResponse"];

// ApiError carries the HTTP status so callers can branch on 409 (journal
// conflicts) without matching on backend message text.
export class ApiError extends Error {
  status: number;

  constructor(message: string, status: number) {
    super(message);
    this.status = status;
  }
}

export function isConflict(err: unknown): boolean {
  return err instanceof ApiError && err.status === 409;
}

// Shared error helper: unwraps the typed error body (if any) into an
// ApiError carrying the response status. Used across operations, positions
// and instruments hooks so all API errors are branchable by status code.
export function apiError(response: Response, error: unknown): ApiError {
  const message =
    (error as { error?: string } | undefined)?.error ?? `request failed: ${response.status}`;
  return new ApiError(message, response.status);
}

export function useOperations(accountId: string, limit = 50, offset = 0) {
  return useQuery({
    queryKey: ["operations", accountId, limit, offset],
    queryFn: async (): Promise<Operation[]> => {
      const { data, error, response } = await api.GET(
        "/api/v1/accounts/{accountId}/operations",
        { params: { path: { accountId }, query: { limit, offset } } },
      );
      if (!data) throw apiError(response, error);
      return data;
    },
    // Keep showing the previous page's data while loading a larger window, so
    // the table stays mounted and the user doesn't see a disruptive flash.
    placeholderData: keepPreviousData,
  });
}

// Invalidates every cache that a journal write (operation create/delete,
// transfer) can affect: the journal itself, computed positions, account
// balances and the space-wide summary.
export function useInvalidateJournal() {
  const queryClient = useQueryClient();
  return (accountIds: string[]) => {
    for (const id of accountIds) {
      void queryClient.invalidateQueries({ queryKey: ["operations", id] });
      void queryClient.invalidateQueries({ queryKey: ["positions", id] });
    }
    void queryClient.invalidateQueries({ queryKey: ["accounts"] });
    void queryClient.invalidateQueries({ queryKey: ["summary"] });
  };
}

export function useCreateOperation() {
  const invalidate = useInvalidateJournal();
  return useMutation({
    mutationFn: async (body: CreateOperationBody): Promise<Operation> => {
      const { data, error, response } = await api.POST("/api/v1/operations", { body });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: (data) => invalidate([data.account_id]),
  });
}

export function useDeleteOperation() {
  const invalidate = useInvalidateJournal();
  return useMutation({
    mutationFn: async (variables: {
      operationId: string;
      // The delete response has no body (204), so the account to invalidate
      // must be supplied by the caller.
      accountId: string;
    }): Promise<void> => {
      const { response, error } = await api.DELETE("/api/v1/operations/{operationId}", {
        params: { path: { operationId: variables.operationId } },
      });
      if (!response.ok) throw apiError(response, error);
    },
    onSuccess: (_data, variables) => invalidate([variables.accountId]),
  });
}

export function useCreateTransfer() {
  const invalidate = useInvalidateJournal();
  return useMutation({
    mutationFn: async (body: TransferBody): Promise<TransferResponse> => {
      const { data, error, response } = await api.POST("/api/v1/operations/transfer", { body });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: (_data, variables) =>
      invalidate([variables.from_account_id, variables.to_account_id]),
  });
}
