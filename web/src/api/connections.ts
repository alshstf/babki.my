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
export type TinvestSyncAcceptedResponse = components["schemas"]["TinvestSyncAcceptedResponse"];
export type TinvestSyncRun = components["schemas"]["TinvestSyncRun"];
export type TinvestSyncRunsPage = components["schemas"]["TinvestSyncRunsResponse"];
export type TinvestUnparsedOperation = components["schemas"]["TinvestUnparsedOperation"];
export type TinvestUnparsedPage = components["schemas"]["TinvestUnparsedResponse"];

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
  });
}

function useInvalidateConnections() {
  const queryClient = useQueryClient();
  return (id?: string) => {
    void queryClient.invalidateQueries({ queryKey: ["tinvest-connections"] });
    if (id) void queryClient.invalidateQueries({ queryKey: ["tinvest-connections", id] });
  };
}

// useCheckToken asks the broker which accounts a read-only token can see —
// the wizard's step 2 — and stores nothing: see POST /api/v1/tinvest/token-check
// in the API contract. networkMode "always" for the reason session.ts's
// useLogin documents at length: this is a "now" action, and react-query's
// default "online" mode would instead hold the request silently while the
// browser believes itself offline, leaving the button spinning forever with
// nothing on screen to say why.
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

// useCreateConnection stores the token, links the picked broker accounts to
// brand-new babki accounts and queues the first sync in one request (see
// POST /api/v1/tinvest/connections). It navigates to the new connection's own
// screen on success, the same way useLogin and useSetup already do for their
// own "create, then go there" actions — the wizard has nowhere else useful to
// send the owner once the connection exists. networkMode "always" for the
// same reason as useCheckToken.
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

export function useUpdateConnection() {
  const invalidate = useInvalidateConnections();
  return useMutation({
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
    onSuccess: (data) => invalidate(data.id),
  });
}

export function useDeleteConnection() {
  const invalidate = useInvalidateConnections();
  return useMutation({
    mutationFn: async (id: string): Promise<void> => {
      const { response, error } = await api.DELETE(
        "/api/v1/tinvest/connections/{connectionId}",
        { params: { path: { connectionId: id } } },
      );
      if (!response.ok) throw apiError(response, error);
    },
    onSuccess: (_data, id) => invalidate(id),
  });
}

// useTriggerSync asks for this connection to be synced now (POST .../sync).
// networkMode "always": the owner pressed a button expecting something to
// happen right now, not a request parked until the browser's own belief about
// the network changes its mind.
//
// The response's `queued` field is not «синхронизация уже идёт» when false —
// see TinvestSyncAcceptedResponse in the API contract: a sync already waiting
// out a failed attempt's backoff (which can run into hours) occupies the same
// uniqueness slot as one actually running, and this hook must not be read by
// a caller as saying more than the contract does. Callers render `queued`
// through their own copy, not through a caption this hook invents.
export function useTriggerSync() {
  const invalidate = useInvalidateConnections();
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
    onSuccess: (_data, id) => invalidate(id),
  });
}

// SYNC_RUNS_PAGE_SIZE / useSyncRuns follow the journal's own paging pattern
// (see JOURNAL_PAGE_SIZE / useOperations in operations.ts): `has_more` is
// fetched, never inferred from a page's length, and each further page is a
// fresh request at the next offset rather than one window that grows past the
// endpoint's own ceiling (200 here, same as the journal).
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
  });
}

// Error branching helpers — status only, per the .tsx rule
// (checkErrorTextInMarkup in scripts/check-i18n.mjs): a component picks its
// Russian sentence from the HTTP status the API contract declares, never from
// the English prose the server happened to write for its own log.

// isTokenRejected: the broker itself refused the token (token-check, create,
// update all answer this way — see api/openapi.yaml). Distinct from
// isBrokerUnreachable below, which is a different piece of news: one is worth
// retrying, the other needs a different token.
export function isTokenRejected(err: unknown): boolean {
  return err instanceof ApiError && err.status === 400;
}

// isBrokerUnreachable: this server could not reach T-Invest, or could not use
// what it answered (502 on the same three endpoints).
export function isBrokerUnreachable(err: unknown): boolean {
  return err instanceof ApiError && err.status === 502;
}

// A 409 is `isConflict` from api/operations.ts (no new helper here — the same
// status code, checked the same way): on POST .../connections it means a
// picked broker account is already imported by another connection; on
// POST .../sync it means the connection is not `active`. Which one a caller
// is looking at is not this module's to know, only its status.
