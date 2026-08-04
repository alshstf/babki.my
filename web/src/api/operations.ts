import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import type { components } from "./schema";

export type Operation = components["schemas"]["Operation"];
export type OperationsPage = components["schemas"]["OperationsResponse"];
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

// JOURNAL_PAGE_SIZE is how many entries one page of the journal holds. It lives
// here rather than in the table because it is a property of the request, and it
// is comfortably under the endpoint's own ceiling (200) so that no page this
// hook asks for is ever clamped — the offsets below are computed from what came
// back, but a clamped page would still cost a needless round trip.
export const JOURNAL_PAGE_SIZE = 50;

// useOperations reads the journal one page at a time and keeps the pages it has
// read, so "show more" appends rather than refetches.
//
// It used to grow a single window instead — one query for [0, limit) with limit
// climbing by 50 — which needed no merge bookkeeping but could never see past
// the endpoint's own ceiling: at limit 250 the server clamps to 200 and returns
// the same 200 rows forever. Worse, the client decided whether to offer "show
// more" by comparing the page's length against what it asked for, and that
// comparison reads a clamped page as the end of the journal, so the control
// disappeared exactly where it was needed and the older entries had no other
// route in the interface (#86).
//
// Both halves of that are gone. Whether there is more is `has_more`, which only
// the server can answer (see OperationsResponse in the API contract), and each
// further page is a request of its own at the next offset — always a page the
// ceiling comfortably admits.
export function useOperations(accountId: string, pageSize = JOURNAL_PAGE_SIZE) {
  return useInfiniteQuery({
    queryKey: ["operations", accountId, pageSize],
    initialPageParam: 0,
    queryFn: async ({ pageParam }): Promise<OperationsPage> => {
      const { data, error, response } = await api.GET(
        "/api/v1/accounts/{accountId}/operations",
        { params: { path: { accountId }, query: { limit: pageSize, offset: pageParam } } },
      );
      if (!data) throw apiError(response, error);
      return data;
    },
    // The next offset is where the rows already in hand end, and it is asked
    // for only when the server says something is there. It counts the rows
    // received rather than multiplying the page count by pageSize: those agree
    // for as long as every page comes back full, which at 50 against a ceiling
    // of 200 it does, and stop agreeing the moment a pageSize above that
    // ceiling is passed here — at which point the multiplication would skip
    // whatever the clamp withheld.
    getNextPageParam: (lastPage, allPages) =>
      lastPage.has_more
        ? allPages.reduce((rows, page) => rows + page.operations.length, 0)
        : undefined,
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
