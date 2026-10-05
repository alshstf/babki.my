import type { ReactNode } from "react";
import { describe, expect, it, vi, beforeEach } from "vitest";
import { renderHook, waitFor, act } from "@testing-library/react";
import { QueryClient, QueryClientProvider, onlineManager } from "@tanstack/react-query";
import {
  useDeleteConnection,
  useUpdateConnection,
  type TinvestConnection,
} from "./connections";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// A fresh Response per call; a 204 carries no body.
function serve(status: number, body: unknown) {
  fetchMock.mockImplementation(() =>
    Promise.resolve(
      status === 204
        ? new Response(null, { status })
        : new Response(JSON.stringify(body), {
            status,
            headers: { "Content-Type": "application/json" },
          }),
    ),
  );
}

function makeConnection(): TinvestConnection {
  // No accounts, so no verdicts: the server sends one per linked account.
  return { id: "conn-1", status: "active", token_last4: "3456", accounts: [], reconciles: [] };
}

function newClient() {
  return new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
}

function wrapper(client: QueryClient) {
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
}

beforeEach(() => {
  fetchMock.mockReset();
  onlineManager.setOnline(true);
});

// Both mutations answer «do this now», and replacing a token is a
// repair. The default networkMode "online" would pause them offline
// with nothing said and send them later on its own (#111 on setup).
describe("connection mutations are attempted when they are asked for", () => {
  it("sends the token replacement even while the browser reports no connection", async () => {
    onlineManager.setOnline(false);
    serve(200, makeConnection());
    const client = newClient();

    const { result } = renderHook(() => useUpdateConnection(), { wrapper: wrapper(client) });
    act(() => {
      result.current.mutate({ id: "conn-1", body: { token: "t.new-token" } });
    });

    // Attempted and answered, not held on the browser's belief about the
    // network.
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(fetchMock).toHaveBeenCalled();
  });

  it("sends the disconnect even while the browser reports no connection", async () => {
    onlineManager.setOnline(false);
    serve(204, null);
    const client = newClient();

    const { result } = renderHook(() => useDeleteConnection(), { wrapper: wrapper(client) });
    act(() => {
      result.current.mutate("conn-1");
    });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(fetchMock).toHaveBeenCalled();
  });
});

// ["tinvest-connections"] is a prefix of ["tinvest-connections", id], and
// react-query matches by prefix, so one invalidation covers both. Checked
// because it is the library's behaviour.
describe("one invalidation reaches the list and every single connection", () => {
  it("marks both stale after a token replacement", async () => {
    serve(200, makeConnection());
    const client = newClient();
    client.setQueryData(["tinvest-connections"], [makeConnection()]);
    client.setQueryData(["tinvest-connections", "conn-1"], makeConnection());

    const { result } = renderHook(() => useUpdateConnection(), { wrapper: wrapper(client) });
    act(() => {
      result.current.mutate({ id: "conn-1", body: { token: "t.new-token" } });
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    expect(client.getQueryState(["tinvest-connections"])?.isInvalidated).toBe(true);
    expect(client.getQueryState(["tinvest-connections", "conn-1"])?.isInvalidated).toBe(true);
  });
});
