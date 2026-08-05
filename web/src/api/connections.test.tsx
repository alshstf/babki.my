import type { ReactNode } from "react";
import { describe, expect, it, vi, beforeEach } from "vitest";
import { renderHook, waitFor, act } from "@testing-library/react";
import { QueryClient, QueryClientProvider, onlineManager } from "@tanstack/react-query";
import {
  useDeleteConnection,
  useUpdateConnection,
  type TinvestConnection,
} from "./connections";

// openapi-fetch captures globalThis.fetch at import time
// (`fetch: baseFetch = globalThis.fetch`), so the double has to be installed
// *before* the imports above run — hence vi.hoisted.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// A fresh Response per call: a body can only be read once, so one shared
// object breaks on the second request. 204 carries no body at all, which
// Response refuses to be given.
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
  return { id: "conn-1", status: "active", token_last4: "3456", accounts: [] };
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

// Both mutations below are pressed on the connection's own screen and both are
// answers to «do this now». react-query's default for mutations, networkMode
// "online", PAUSES a mutation while the browser reports itself offline:
// nothing is sent, nothing fails, isError stays false — so no alert can appear
// — and isPending stays true, which disables the button that would try again.
// The owner presses and watches nothing happen, with nothing said about why;
// then, whenever the browser changes its mind, the held request goes out on its
// own. That is #111 on the setup form, and it is worse here, because REPLACING
// A TOKEN IS A REPAIR: it is reached from a connection the server has already
// parked at `token_revoked`, so it is used precisely when something is known to
// be broken.
describe("connection mutations are attempted when they are asked for", () => {
  it("sends the token replacement even while the browser reports no connection", async () => {
    onlineManager.setOnline(false);
    serve(200, makeConnection());
    const client = newClient();

    const { result } = renderHook(() => useUpdateConnection(), { wrapper: wrapper(client) });
    act(() => {
      result.current.mutate({ id: "conn-1", body: { token: "t.new-token" } });
    });

    // Attempted and answered, not held: the browser's own belief about the
    // network — wrong behind a captive portal, wrong again on a connection
    // that is up but going nowhere — does not get to decide this one.
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

// The claim the invalidation's own comment rests on: ["tinvest-connections"]
// is the PREFIX of ["tinvest-connections", id], and react-query matches a key
// by prefix unless told otherwise — so naming the single connection separately
// would invalidate nothing the list key did not. This is what makes the second
// line removable rather than merely redundant-looking, and it is checked here
// because it is a claim about a library's behaviour, not about ours.
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
