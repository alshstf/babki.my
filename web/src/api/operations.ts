import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import type { components } from "./schema";

export type Operation = components["schemas"]["Operation"];
// Which term the server could not value, the only source of a row's «not
// converted» caption (Operation.in_base_gap).
export type OperationInBaseGap = components["schemas"]["OperationInBaseGap"];
export type OperationsPage = components["schemas"]["OperationsResponse"];
export type OperationType = components["schemas"]["OperationType"];
export type CreateOperationBody = components["schemas"]["CreateOperationRequest"];
export type TransferBody = components["schemas"]["TransferRequest"];
export type TransferResponse = components["schemas"]["TransferResponse"];
export type MoneyTransferBody = components["schemas"]["MoneyTransferRequest"];

// ApiError carries the HTTP status, so callers branch on it, not on the
// server's text.
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

// Whether the server refused for want of a valid identity, by status. False
// for a failure with no status (a dead connection), where «Неверный логин или
// пароль» would name an unknown cause.
export function isUnauthorized(err: unknown): boolean {
  return err instanceof ApiError && err.status === 401;
}

// apiError unwraps the typed error body into an ApiError with the status.
export function apiError(response: Response, error: unknown): ApiError {
  const message =
    (error as { error?: string } | undefined)?.error ?? `request failed: ${response.status}`;
  return new ApiError(message, response.status);
}

// JOURNAL_PAGE_SIZE is one journal page, well under the endpoint's ceiling of
// 200, past which the request is refused (#118).
export const JOURNAL_PAGE_SIZE = 50;

// What a journal listing is narrowed to; an absent field narrows nothing.
export type JournalFilter = {
  type?: OperationType;
  instrumentId?: string;
  from?: string;
  to?: string;
  // A category's id (with the ones inside it), or "none" for the rows still
  // waiting for one.
  category?: string;
};

// The operation types, in the order a journal's filter offers them.
export const OPERATION_TYPES: OperationType[] = [
  "buy", "sell", "deposit", "withdrawal", "dividend", "coupon", "interest", "tax", "fee",
  "amortization", "redemption", "transfer_in", "transfer_out", "split", "conversion",
  "exchange_out", "exchange_in", "spinoff_out", "spinoff_in",
];

// useOperations reads the journal a page at a time and keeps the pages, so "show
// more" appends. Whether there is more is the server's has_more (#86), and each
// page is a request at the next offset, always under the ceiling (#118).
export function useOperations(accountId: string, pageSize = JOURNAL_PAGE_SIZE, filter: JournalFilter = {}) {
  return useInfiniteQuery({
    queryKey: ["operations", accountId, pageSize, filter],
    initialPageParam: 0,
    queryFn: async ({ pageParam }): Promise<OperationsPage> => {
      const { data, error, response } = await api.GET(
        "/api/v1/accounts/{accountId}/operations",
        {
          params: {
            path: { accountId },
            query: {
              limit: pageSize,
              offset: pageParam,
              type: filter.type ? [filter.type] : undefined,
              instrument_id: filter.instrumentId,
              from: filter.from,
              to: filter.to,
              category: filter.category,
            },
          },
        },
      );
      if (!data) throw apiError(response, error);
      return data;
    },
    // The next offset is where the rows in hand end, asked only when the
    // server says more exists; counted from rows, not pages.
    getNextPageParam: (lastPage, allPages) =>
      lastPage.has_more
        ? allPages.reduce((rows, page) => rows + page.operations.length, 0)
        : undefined,
  });
}

// Invalidates everything a journal write can affect: the journal,
// positions, balances and the summary.
export function useInvalidateJournal() {
  const queryClient = useQueryClient();
  return (accountIds: string[]) => {
    for (const id of accountIds) {
      void queryClient.invalidateQueries({ queryKey: ["operations", id] });
      void queryClient.invalidateQueries({ queryKey: ["positions", id] });
      void queryClient.invalidateQueries({ queryKey: ["arrivals", id] });
    }
    void queryClient.invalidateQueries({ queryKey: ["accounts"] });
    void queryClient.invalidateQueries({ queryKey: ["summary"] });
    // Read from journals too: every return (an account's, the family's, a
    // paper's), the capital chart and a paper's own page. Without these a new
    // deposit left «вложено за период» as it was until the page was reloaded.
    void queryClient.invalidateQueries({ queryKey: ["return"] });
    void queryClient.invalidateQueries({ queryKey: ["capital"] });
    void queryClient.invalidateQueries({ queryKey: ["instrument-holdings"] });
    void queryClient.invalidateQueries({ queryKey: ["instrument-operations"] });
    void queryClient.invalidateQueries({ queryKey: ["cashflow"] });
    void queryClient.invalidateQueries({ queryKey: ["budget"] });
    // What is due on a card, the regular payments and the money ahead are
    // worked out from the journals as well.
    void queryClient.invalidateQueries({ queryKey: ["credit-card"] });
    void queryClient.invalidateQueries({ queryKey: ["credit-cards"] });
    void queryClient.invalidateQueries({ queryKey: ["recurring"] });
    void queryClient.invalidateQueries({ queryKey: ["forecast"] });
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

// Files a row under a category or takes it out (null). Only the journal and
// the money report show it, so only they are read again.
export function useSetOperationCategory() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ operationId, categoryId }: { operationId: string; categoryId: string | null }): Promise<Operation> => {
      const { data, error, response } = await api.PUT("/api/v1/operations/{operationId}/category", {
        params: { path: { operationId } },
        body: { category_id: categoryId },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: (data) => {
      void queryClient.invalidateQueries({ queryKey: ["operations", data.account_id] });
      void queryClient.invalidateQueries({ queryKey: ["cashflow"] });
      void queryClient.invalidateQueries({ queryKey: ["budget"] });
    },
  });
}

export type OperationPart = components["schemas"]["OperationPart"];

// Splits a row across categories, or makes it one category's again (null;
// decision Р-36). The journal, the money report and the budget read it.
export function useSetOperationParts() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ operationId, parts }: { operationId: string; parts: OperationPart[] | null }): Promise<Operation> => {
      const { data, error, response } = parts
        ? await api.PUT("/api/v1/operations/{operationId}/parts", { params: { path: { operationId } }, body: { parts } })
        : await api.DELETE("/api/v1/operations/{operationId}/parts", { params: { path: { operationId } } });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: (data) => {
      void queryClient.invalidateQueries({ queryKey: ["operations", data.account_id] });
      void queryClient.invalidateQueries({ queryKey: ["cashflow"] });
      void queryClient.invalidateQueries({ queryKey: ["budget"] });
    },
  });
}

// Says whose a row is, or gives it back to the account's owner (null).
export function useSetOperationMember() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ operationId, memberId }: { operationId: string; memberId: string | null }): Promise<Operation> => {
      const { data, error, response } = await api.PUT("/api/v1/operations/{operationId}/member", {
        params: { path: { operationId } },
        body: { member_id: memberId },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: (data) => {
      void queryClient.invalidateQueries({ queryKey: ["operations", data.account_id] });
      void queryClient.invalidateQueries({ queryKey: ["cashflow"] });
      void queryClient.invalidateQueries({ queryKey: ["budget"] });
    },
  });
}

// Files the unfiled rows of one account, or of the whole family, by the
// family's rules; answers how many it filed.
export function useFileByRules() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (accountId?: string): Promise<number> => {
      const { data, error, response } = await api.POST("/api/v1/operations/file-by-rules", {
        body: { account_id: accountId ?? null },
      });
      if (!data) throw apiError(response, error);
      return data.filed;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["operations"] });
      void queryClient.invalidateQueries({ queryKey: ["cashflow"] });
      void queryClient.invalidateQueries({ queryKey: ["budget"] });
    },
  });
}

// Writes what a dialog holds: a new operation, or the row it was opened on,
// in place (PUT keeps its id and its place in its day).
export function useSaveOperation(editingId?: string) {
  const invalidate = useInvalidateJournal();
  return useMutation({
    mutationFn: async (body: CreateOperationBody): Promise<Operation> => {
      if (editingId) {
        const { data, error, response } = await api.PUT("/api/v1/operations/{operationId}", {
          params: { path: { operationId: editingId } },
          body,
        });
        if (!data) throw apiError(response, error);
        return data;
      }
      const { data, error, response } = await api.POST("/api/v1/operations", { body });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: (data) => invalidate([data.account_id]),
  });
}

// The dialog an operation is edited in, or null when it cannot be edited in
// place (a broker's row, one leg of a pair, an arrival, kinds no dialog records),
// as the server refuses the same.
export type EditDialog = "trade" | "cash" | "income";

// Whether a person may edit and delete a row: entered by hand or loaded from
// their table (operation.OwnedByHand).
export function ownedByHand(operation: Operation): boolean {
  return operation.source === "manual" || operation.source === "csv";
}

export function editDialogOf(operation: Operation): EditDialog | null {
  if (!ownedByHand(operation) || operation.transfer_group_id) return null;
  switch (operation.type) {
    case "buy":
    case "sell":
      return "trade";
    case "deposit":
    case "withdrawal":
    case "fee":
    case "tax":
    case "interest":
      return "cash";
    case "dividend":
    case "coupon":
    case "amortization":
      return "income";
    default:
      return null;
  }
}

export function useDeleteOperation() {
  const invalidate = useInvalidateJournal();
  return useMutation({
    mutationFn: async (variables: {
      operationId: string;
      // The delete response has no body, so the caller supplies the account.
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

// useStateWithheld states the tax withheld abroad from the dividend payment
// a row belongs to (Р-14), or clears it with null, bringing the estimate back.
export function useStateWithheld(operation: Operation) {
  const invalidate = useInvalidateJournal();
  return useMutation({
    mutationFn: async (taxMinor: number | null): Promise<void> => {
      const params = { path: { operationId: operation.id } };
      const { response, error } =
        taxMinor === null
          ? await api.DELETE("/api/v1/operations/{operationId}/withheld-abroad", { params })
          : await api.PUT("/api/v1/operations/{operationId}/withheld-abroad", { params, body: { tax_minor: taxMinor } });
      if (!response.ok) throw apiError(response, error);
    },
    onSuccess: () => invalidate([operation.account_id]),
  });
}

// useCreateMoneyTransfer moves money between two of the family's accounts as
// one transfer.
export function useCreateMoneyTransfer() {
  const invalidate = useInvalidateJournal();
  return useMutation({
    mutationFn: async (body: MoneyTransferBody): Promise<TransferResponse> => {
      const { data, error, response } = await api.POST("/api/v1/operations/money-transfer", { body });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: (_data, variables) =>
      invalidate([variables.from_account_id, variables.to_account_id]),
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

