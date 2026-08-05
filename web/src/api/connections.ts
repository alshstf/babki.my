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
export type TinvestReconcileSnapshot = components["schemas"]["TinvestReconcileSnapshot"];
export type TinvestSyncAcceptedResponse = components["schemas"]["TinvestSyncAcceptedResponse"];
export type TinvestSyncRun = components["schemas"]["TinvestSyncRun"];
export type TinvestSyncRunStatus = components["schemas"]["TinvestSyncRunStatus"];
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

// useInvalidateConnections refetches everything keyed on connections: the list
// AND every single connection, because react-query matches a query key by
// PREFIX unless told otherwise, and ["tinvest-connections"] is the prefix of
// ["tinvest-connections", id]. Naming the second key as well would invalidate
// nothing the first did not, while reading as though one connection needed its
// own line.
function useInvalidateConnections() {
  const queryClient = useQueryClient();
  return () => {
    void queryClient.invalidateQueries({ queryKey: ["tinvest-connections"] });
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

// useUpdateConnection replaces the token, switches the connection on or off, or
// both (PATCH .../connections/{id}).
//
// networkMode "always" for the reason useCheckToken documents, and this is the
// place it matters most: replacing a token is a REPAIR — it is reached from a
// connection the server has already parked at `token_revoked` — so it is used
// exactly when something is known to be broken. react-query's default would
// park the request while the browser believes itself offline: nothing sent,
// isError false, isPending true, and a screen where pressing the button does
// nothing at all and says nothing about why. That is #111 on the setup form,
// and there is no reason it should read differently here.
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

// useDeleteConnection withdraws the connection (DELETE .../connections/{id}).
// The accounts and their operations stay — see the contract; what goes is the
// stored token and everything the import kept beside it.
//
// networkMode "always" for the same reason as useUpdateConnection: the owner
// pressed a button and is waiting for it to be over, and a request parked until
// the browser changes its mind about the network leaves the screen saying
// nothing while the connection is still there.
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
    onSuccess: () => invalidate(),
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

// isBrokerAccountNotImportable: a broker account that was picked is not one this
// token can import (422, and POST /api/v1/tinvest/connections is the only path
// that declares it — no other call takes picks).
//
// IT IS THE ONE ANSWER FROM THIS CALL THAT SAYS THE TOKEN IS FINE. The broker's
// account list moved under the wizard — an account closed, or a token's access
// narrowed, between the token-check and the create, which asks for that list
// afresh. It used to arrive as the same 400 a refused token does, where the only
// sentence a screen could show sent the owner to re-issue a token that never
// stopped working.
//
// Nothing about the ORDER of the checks at a call site rides on this: 400 and
// 422 are different codes, so isTokenRejected and this one can never both be
// true of one error. What rides on it is that the two get different sentences.
export function isBrokerAccountNotImportable(err: unknown): boolean {
  return err instanceof ApiError && err.status === 422;
}

// isConnectionMissing: there is no such connection in the caller's space (404 —
// every path under a connection id declares it). Its own sentence rather than
// the general one, because it is the one failure that is not a fault: a
// connection deleted in another tab, or a link followed after it was withdrawn,
// leaves nothing to fix and nothing to retry.
export function isConnectionMissing(err: unknown): boolean {
  return err instanceof ApiError && err.status === 404;
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
