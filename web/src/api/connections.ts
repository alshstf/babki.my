import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { api } from "./client";
import { apiError, ApiError } from "./operations";
import type { components } from "./schema";

export type TinvestConnection = components["schemas"]["TinvestConnection"];
export type TinvestConnectionStatus = components["schemas"]["TinvestConnectionStatus"];
export type TinvestBrokerAccount = components["schemas"]["TinvestBrokerAccount"];
export type TinvestTokenCheckResponse = components["schemas"]["TinvestTokenCheckResponse"];
export type CreateConnectionBody = components["schemas"]["CreateTinvestConnectionRequest"];
export type UpdateConnectionBody = components["schemas"]["UpdateTinvestConnectionRequest"];
export type TinvestLinkedAccount = components["schemas"]["TinvestLinkedAccount"];
export type TinvestReconcileMismatch = components["schemas"]["TinvestReconcileMismatch"];
export type TinvestAccountReconcile = components["schemas"]["TinvestAccountReconcile"];
export type TinvestReconcileStatus = components["schemas"]["TinvestReconcileStatus"];
export type TinvestSyncAcceptedResponse = components["schemas"]["TinvestSyncAcceptedResponse"];
export type TinvestSyncRun = components["schemas"]["TinvestSyncRun"];
export type TinvestSyncRunStatus = components["schemas"]["TinvestSyncRunStatus"];
export type TinvestSyncRunsPage = components["schemas"]["TinvestSyncRunsResponse"];
export type TinvestUnparsedOperation = components["schemas"]["TinvestUnparsedOperation"];
export type TinvestUnparsedPage = components["schemas"]["TinvestUnparsedResponse"];

/**
 * How often the open connection screen polls. A sync takes minutes, so
 * fifteen seconds asks no more than needed: four requests a minute to one's own
 * server, and quick enough that the page does not look stuck.
 */
const CONNECTION_POLL_MS = 15_000;

export function useConnections() {
  return useQuery({
    queryKey: ["tinvest-connections"],
    queryFn: async (): Promise<TinvestConnection[]> => {
      const { data, error, response } = await api.GET("/api/v1/tinvest/connections");
      if (!data) throw apiError(response, error);
      return data.connections;
    },
  });
}

export function useConnection(id: string) {
  return useQuery({
    queryKey: ["tinvest-connections", id],
    queryFn: async (): Promise<TinvestConnection> => {
      const { data, error, response } = await api.GET(
        "/api/v1/tinvest/connections/{connectionId}",
        { params: { path: { connectionId: id } } },
      );
      if (!data) throw apiError(response, error);
      return data;
    },
    enabled: id !== "",
    // Polled: the screen reports on a background sync that runs minutes after the
    // owner arrives, and refetchOnWindowFocus is off globally, so without a poll its
    // sentences stay frozen at arrival.
    refetchInterval: CONNECTION_POLL_MS,
  });
}

// useInvalidateConnections refetches the list and every single connection:
// react-query matches keys by prefix, so ["tinvest-connections"] covers
// ["tinvest-connections", id].
function useInvalidateConnections() {
  const queryClient = useQueryClient();
  return () => {
    void queryClient.invalidateQueries({ queryKey: ["tinvest-connections"] });
  };
}

// useCheckToken asks which accounts a read-only token sees (the wizard's step
// 2) and stores nothing. networkMode "always": the default would hold the request
// silently while the browser thinks it is offline (see useLogin).
export function useCheckToken() {
  return useMutation({
    networkMode: "always",
    mutationFn: async (token: string): Promise<TinvestTokenCheckResponse> => {
      const { data, error, response } = await api.POST("/api/v1/tinvest/token-check", {
        body: { token },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

// useCreateConnection stores the token, links the picked accounts to new babki
// accounts and queues the first sync in one request, then navigates to the new
// connection. networkMode "always" as above.
export function useCreateConnection() {
  const invalidate = useInvalidateConnections();
  const navigate = useNavigate();
  return useMutation({
    networkMode: "always",
    mutationFn: async (body: CreateConnectionBody): Promise<TinvestConnection> => {
      const { data, error, response } = await api.POST("/api/v1/tinvest/connections", { body });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: (data) => {
      invalidate();
      void navigate({
        to: "/settings/connections/$connectionId",
        params: { connectionId: data.id },
      });
    },
  });
}

// useUpdateConnection replaces the token, switches the connection on or off, or
// both. networkMode "always" matters most here: replacing a token is a repair of a
// connection parked at token_revoked, and a request held offline would leave the
// button doing nothing (#111).
export function useUpdateConnection() {
  const invalidate = useInvalidateConnections();
  return useMutation({
    networkMode: "always",
    mutationFn: async ({
      id,
      body,
    }: {
      id: string;
      body: UpdateConnectionBody;
    }): Promise<TinvestConnection> => {
      const { data, error, response } = await api.PATCH(
        "/api/v1/tinvest/connections/{connectionId}",
        { params: { path: { connectionId: id } }, body },
      );
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: () => invalidate(),
  });
}

// useDeleteConnection withdraws the connection; the accounts and operations
// stay. networkMode "always" as above.
export function useDeleteConnection() {
  const invalidate = useInvalidateConnections();
  return useMutation({
    networkMode: "always",
    mutationFn: async (id: string): Promise<void> => {
      const { response, error } = await api.DELETE(
        "/api/v1/tinvest/connections/{connectionId}",
        { params: { path: { connectionId: id } } },
      );
      if (!response.ok) throw apiError(response, error);
    },
    onSuccess: () => invalidate(),
  });
}

// useTriggerSync asks for a sync now (networkMode "always"). queued=false does
// not mean «синхронизация уже идёт»: a sync waiting out a backoff of hours holds
// the same slot (TinvestSyncAcceptedResponse); callers word it themselves.
export function useTriggerSync() {
  const invalidate = useInvalidateConnections();
  const queryClient = useQueryClient();
  return useMutation({
    networkMode: "always",
    mutationFn: async (id: string): Promise<TinvestSyncAcceptedResponse> => {
      const { data, error, response } = await api.POST(
        "/api/v1/tinvest/connections/{connectionId}/sync",
        { params: { path: { connectionId: id } } },
      );
      if (!data) throw apiError(response, error);
      return data;
    },
    // The run log too: its key is not under the connections prefix, and the new
    // run appears there when it starts.
    onSuccess: (_data, id) => {
      invalidate();
      void queryClient.invalidateQueries({ queryKey: ["tinvest-sync-runs", id] });
    },
  });
}

// SYNC_RUNS_PAGE_SIZE and useSyncRuns page like the journal (JOURNAL_PAGE_SIZE,
// useOperations): has_more is fetched, each page a request at the next offset
// (the endpoint's ceiling is 200).
export const SYNC_RUNS_PAGE_SIZE = 50;

export function useSyncRuns(connectionId: string, pageSize = SYNC_RUNS_PAGE_SIZE) {
  return useInfiniteQuery({
    queryKey: ["tinvest-sync-runs", connectionId, pageSize],
    initialPageParam: 0,
    queryFn: async ({ pageParam }): Promise<TinvestSyncRunsPage> => {
      const { data, error, response } = await api.GET(
        "/api/v1/tinvest/connections/{connectionId}/runs",
        { params: { path: { connectionId }, query: { limit: pageSize, offset: pageParam } } },
      );
      if (!data) throw apiError(response, error);
      return data;
    },
    getNextPageParam: (lastPage, allPages) =>
      lastPage.has_more
        ? allPages.reduce((rows, page) => rows + page.runs.length, 0)
        : undefined,
    enabled: connectionId !== "",
    // Polled like the connection, refetching every loaded page; readers are
    // almost always on the first, and refetching only the head would renumber
    // the pages below.
    refetchInterval: CONNECTION_POLL_MS,
  });
}

export const UNPARSED_PAGE_SIZE = 50;

export function useUnparsed(connectionId: string, pageSize = UNPARSED_PAGE_SIZE) {
  return useInfiniteQuery({
    queryKey: ["tinvest-unparsed", connectionId, pageSize],
    initialPageParam: 0,
    queryFn: async ({ pageParam }): Promise<TinvestUnparsedPage> => {
      const { data, error, response } = await api.GET(
        "/api/v1/tinvest/connections/{connectionId}/unparsed",
        { params: { path: { connectionId }, query: { limit: pageSize, offset: pageParam } } },
      );
      if (!data) throw apiError(response, error);
      return data;
    },
    getNextPageParam: (lastPage, allPages) =>
      lastPage.has_more
        ? allPages.reduce((rows, page) => rows + page.operations.length, 0)
        : undefined,
    enabled: connectionId !== "",
    // Polled with the counters beside it, or the list would contradict them
    // («не разобрано: 148» above «Неразобранных операций нет», as on the first
    // real import).
    refetchInterval: CONNECTION_POLL_MS,
  });
}

// Error helpers branch on status only (checkErrorTextInMarkup in
// scripts/check-i18n.mjs), never on the server's English.

// isTokenRejected: the broker refused the token (400 on token-check, create
// and update). Needs a different token, unlike isBrokerUnreachable.
export function isTokenRejected(err: unknown): boolean {
  return err instanceof ApiError && err.status === 400;
}

// isBrokerAccountNotImportable: a picked account this token cannot import
// (422, only on POST /api/v1/tinvest/connections). The token is fine: the broker's
// account list changed since the token check. 400 and 422 never coincide, so the
// two get different sentences.
export function isBrokerAccountNotImportable(err: unknown): boolean {
  return err instanceof ApiError && err.status === 422;
}

// isConnectionMissing: no such connection in this space (404). Its own
// sentence: deleted in another tab, nothing to fix or retry.
export function isConnectionMissing(err: unknown): boolean {
  return err instanceof ApiError && err.status === 404;
}

// isBrokerUnreachable: the server could not reach T-Invest or use its answer
// (502).
export function isBrokerUnreachable(err: unknown): boolean {
  return err instanceof ApiError && err.status === 502;
}

// A 409 is isConflict from api/operations.ts: on create, an account already
// imported; on sync, a connection not active.
