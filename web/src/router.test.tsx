import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider, onlineManager } from "@tanstack/react-query";
import {
  Outlet,
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from "@tanstack/react-router";
import "@/i18n";
import { Gate } from "./router";
import type { SessionInfo } from "@/api/session";

// openapi-fetch captures globalThis.fetch at import time
// (`fetch: baseFetch = globalThis.fetch`), so the double has to be installed
// *before* the imports above run — hence vi.hoisted.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// Serves the given endpoints (matched on the path's suffix) and 404s the rest.
// A fresh Response per call: a single one handed to mockResolvedValue works
// once and then throws, because a body can only be consumed once.
function serve(routes: Record<string, { status?: number; body?: unknown }>) {
  const paths = Object.keys(routes);
  fetchMock.mockImplementation((input: RequestInfo | URL) => {
    const url = input instanceof Request ? input.url : String(input);
    const path = new URL(url, "http://localhost").pathname;
    const match = paths.find((route) => path.endsWith(route));
    const route = match ? routes[match] : undefined;
    return Promise.resolve(
      new Response(JSON.stringify(route?.body ?? null), {
        status: route ? (route.status ?? 200) : 404,
        headers: { "Content-Type": "application/json" },
      }),
    );
  });
}

function makeSession(): SessionInfo {
  return {
    user: { id: "user-1", username: "alex", display_name: "Alex" },
    role: "owner",
    space_id: "space-1",
    space_name: "Family",
    base_currency: "RUB",
    tax_residency: "RU",
    cost_basis_rules: {
      country: "RU",
      method: "fifo",
      perimeter: "account",
      supported: true,
      notices: [],
    },
  };
}

// The gate under a router that has the three destinations it can send a reader
// to, each reduced to a marker: what the gate decided is then simply which
// marker is on screen.
//
// Returns the QueryClient alongside the render result so a test can drive a
// refetch directly (`qc.refetchQueries`) instead of only ever observing the
// gate's first answer.
function renderGate(wants: "app" | "login" | "setup" = "app") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const rootRoute = createRootRoute({ component: () => <Outlet /> });
  const indexRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/",
    component: () => (
      <Gate wants={wants}>
        <div data-testid="app-screen" />
      </Gate>
    ),
  });
  const loginRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/login",
    component: () => <div data-testid="login-screen" />,
  });
  const setupRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/setup",
    component: () => <div data-testid="setup-screen" />,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([indexRoute, loginRoute, setupRoute]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  return {
    ...render(
      <QueryClientProvider client={qc}>
        <RouterProvider router={router} />
      </QueryClientProvider>,
    ),
    qc,
  };
}

afterEach(() => {
  fetchMock.mockReset();
  onlineManager.setOnline(true);
});

// #88: `setup_needed ?? false` turned «нам ещё не ответили» into «настройка не
// нужна», so an instance nobody had set up answered a login form for a user
// who does not exist — and the person on the other side has no way to guess
// that the right screen was the setup wizard.
describe("Gate — what it does before it knows", () => {
  it("does not decide the instance is set up when nothing answered", async () => {
    serve({
      "/api/v1/setup/status": { status: 500, body: { error: "internal error" } },
      "/api/v1/auth/me": { status: 401, body: { error: "authentication required" } },
    });
    renderGate();

    expect(await screen.findByText(/не знает, с какого экрана начать/i)).toBeInTheDocument();
    expect(screen.queryByTestId("login-screen")).not.toBeInTheDocument();
    expect(screen.queryByTestId("setup-screen")).not.toBeInTheDocument();
    // …and does not say the server was silent about it. The server answered
    // this test — with a 500. The reason in a notice has to be the real reason,
    // or the notice is the same kind of invention as the screen it prevents.
    expect(screen.queryByText(/сервер не ответил/i)).not.toBeInTheDocument();
  });

  it("does not decide nobody is signed in when the session query failed", async () => {
    serve({
      "/api/v1/setup/status": { body: { setup_needed: false } },
      "/api/v1/auth/me": { status: 500, body: { error: "internal error" } },
    });
    renderGate();

    // Which screen is unknown here is only the second one: whether the instance
    // is set up HAS been answered, so the notice may not say the choice starts
    // at «первый запуск».
    expect(await screen.findByText(/не удалось узнать, выполнен ли вход/i)).toBeInTheDocument();
    expect(screen.queryByTestId("login-screen")).not.toBeInTheDocument();
  });

  it("still goes to the wizard when the session query failed on a fresh instance", async () => {
    // Both answers are in, and they are not both needed: an instance with no
    // owner yet has one screen it can show whatever the session says, because
    // there is no account to be signed in to. Saying «не знает, с какого экрана
    // начать» here would be a false caption over a decision already made.
    serve({
      "/api/v1/setup/status": { body: { setup_needed: true } },
      "/api/v1/auth/me": { status: 500, body: { error: "internal error" } },
    });
    renderGate();

    expect(await screen.findByTestId("setup-screen")).toBeInTheDocument();
    expect(screen.queryByText(/не удалось узнать/i)).not.toBeInTheDocument();
  });

  it("does not decide anything while the browser is offline", async () => {
    // Paused, not failed and not loading: react-query holds the request
    // (networkMode "online", the default), so status stays "pending" while
    // fetchStatus is "paused" — and isLoading, which is isPending && isFetching,
    // is false. The old gate's only guard was isLoading.
    onlineManager.setOnline(false);
    serve({
      "/api/v1/setup/status": { body: { setup_needed: true } },
      "/api/v1/auth/me": { status: 401, body: { error: "authentication required" } },
    });
    renderGate();

    expect(await screen.findByText(/связи с сервером нет/i)).toBeInTheDocument();
    expect(screen.queryByTestId("login-screen")).not.toBeInTheDocument();
    expect(screen.queryByTestId("setup-screen")).not.toBeInTheDocument();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("asks again when asked to, and routes on the answer", async () => {
    serve({
      "/api/v1/setup/status": { status: 500, body: { error: "internal error" } },
      "/api/v1/auth/me": { status: 401, body: { error: "authentication required" } },
    });
    renderGate();
    await screen.findByText(/не знает, с какого экрана начать/i);

    serve({
      "/api/v1/setup/status": { body: { setup_needed: false } },
      "/api/v1/auth/me": { body: makeSession() },
    });
    fireEvent.click(screen.getByRole("button", { name: "Повторить" }));

    expect(await screen.findByTestId("app-screen")).toBeInTheDocument();
  });
});

// The routing that was already right, pinned here so that teaching the gate to
// wait cannot quietly stop it from ever moving.
describe("Gate — what it does once it knows", () => {
  it("sends a fresh instance to the setup wizard", async () => {
    serve({
      "/api/v1/setup/status": { body: { setup_needed: true } },
      "/api/v1/auth/me": { status: 401, body: { error: "authentication required" } },
    });
    renderGate();

    expect(await screen.findByTestId("setup-screen")).toBeInTheDocument();
  });

  it("sends a set-up instance with nobody signed in to the login form", async () => {
    serve({
      "/api/v1/setup/status": { body: { setup_needed: false } },
      "/api/v1/auth/me": { status: 401, body: { error: "authentication required" } },
    });
    renderGate();

    expect(await screen.findByTestId("login-screen")).toBeInTheDocument();
  });

  it("lets a signed-in reader through", async () => {
    serve({
      "/api/v1/setup/status": { body: { setup_needed: false } },
      "/api/v1/auth/me": { body: makeSession() },
    });
    renderGate();

    expect(await screen.findByTestId("app-screen")).toBeInTheDocument();
  });
});

// A regression on the base commit: the gate's guard there was
// `session.isLoading`, which goes false the moment data exists, so a failed
// background refresh fell straight through it and rendered from cache — by
// accident, not by a check that said so. `session.isError` alone is not that
// check either: react-query sets it whenever the LAST attempt failed, even
// with an earlier success still sitting in the cache, so a naive fix here
// would throw a signed-in reader out over a refresh failing — the laptop
// waking from sleep and firing `online` while the server is still coming up,
// or the owner restarting the docker stand with the tab open — with a caption
// that is false in exactly this state: the client did know a moment ago and
// still holds the answer.
describe("Gate — a failed refresh does not discard what it already knows", () => {
  it("keeps a signed-in reader on screen when a background refresh of the session fails", async () => {
    serve({
      "/api/v1/setup/status": { body: { setup_needed: false } },
      "/api/v1/auth/me": { body: makeSession() },
    });
    const { qc } = renderGate();
    expect(await screen.findByTestId("app-screen")).toBeInTheDocument();

    // The cache now holds a successful answer. Fail the next attempt without
    // touching that cache entry, then trigger the same refetch a background
    // refresh performs.
    serve({
      "/api/v1/setup/status": { body: { setup_needed: false } },
      "/api/v1/auth/me": { status: 500, body: { error: "internal error" } },
    });
    await qc.refetchQueries({ queryKey: ["session"] });

    // The refetch's rejection is handled by a subscriber callback the query
    // client notifies asynchronously, so the re-render it may trigger has not
    // necessarily landed yet the instant the promise above settles — hence
    // `waitFor` rather than a synchronous assertion, which would pass here
    // even against the bug this pins simply by observing the DOM before React
    // caught up.
    await waitFor(() => {
      expect(screen.getByTestId("app-screen")).toBeInTheDocument();
    });
    expect(screen.queryByText(/не удалось узнать, выполнен ли вход/i)).not.toBeInTheDocument();
  });
});
