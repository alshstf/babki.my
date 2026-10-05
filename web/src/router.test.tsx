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
import { Gate, ScreenCrashed, routeTree, router as appRouter } from "./router";
import type { SessionInfo } from "@/api/session";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// Serves the given endpoints by path suffix and 404s the rest; a fresh
// Response per call, since a body can be read only once.
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

// The gate under a router whose three destinations are markers, so the
// decision is which marker shows. Returns the QueryClient so a test can
// drive a refetch.
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

// #88: an unanswered setup_needed must not read as "setup not needed",
// which showed a login form for a user who does not exist.
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
    // The server answered, with a 500, so the notice must not say it was
    // silent.
    expect(screen.queryByText(/сервер не ответил/i)).not.toBeInTheDocument();
  });

  it("does not decide nobody is signed in when the session query failed", async () => {
    serve({
      "/api/v1/setup/status": { body: { setup_needed: false } },
      "/api/v1/auth/me": { status: 500, body: { error: "internal error" } },
    });
    renderGate();

    // Setup has been answered, so the notice may not start at «первый
    // запуск».
    expect(await screen.findByText(/не удалось узнать, выполнен ли вход/i)).toBeInTheDocument();
    expect(screen.queryByTestId("login-screen")).not.toBeInTheDocument();
  });

  it("still goes to the wizard when the session query failed on a fresh instance", async () => {
    // No owner yet: setup is the only screen whatever the session says.
    serve({
      "/api/v1/setup/status": { body: { setup_needed: true } },
      "/api/v1/auth/me": { status: 500, body: { error: "internal error" } },
    });
    renderGate();

    expect(await screen.findByTestId("setup-screen")).toBeInTheDocument();
    expect(screen.queryByText(/не удалось узнать/i)).not.toBeInTheDocument();
  });

  it("does not decide anything while the browser is offline", async () => {
    // Paused, not failed: status "pending", fetchStatus "paused", and
    // isLoading false.
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

// The routing that already worked, pinned so waiting cannot stop it.
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

// With an earlier success cached, a failed refresh (a laptop waking, the
// stand restarting) keeps rendering from cache. isError alone is set by
// the last attempt and would throw a signed-in reader out.
describe("Gate — a failed refresh does not discard what it already knows", () => {
  it("keeps a signed-in reader on screen when a background refresh of the session fails", async () => {
    serve({
      "/api/v1/setup/status": { body: { setup_needed: false } },
      "/api/v1/auth/me": { body: makeSession() },
    });
    const { qc } = renderGate();
    expect(await screen.findByTestId("app-screen")).toBeInTheDocument();

    // Fail the next attempt without touching the cache, then refetch as a
    // background refresh would.
    serve({
      "/api/v1/setup/status": { body: { setup_needed: false } },
      "/api/v1/auth/me": { status: 500, body: { error: "internal error" } },
    });
    await qc.refetchQueries({ queryKey: ["session"] });

    // The rejection reaches the client's subscribers asynchronously, so the
    // assertion waits for React.
    await waitFor(() => {
      expect(screen.getByTestId("app-screen")).toBeInTheDocument();
    });
    expect(screen.queryByText(/не удалось узнать, выполнен ли вход/i)).not.toBeInTheDocument();
  });
});

// #15: screens load on first visit, so the risk is one that never
// appears (bad path, wrong export, endless boundary). This walks the
// application's own route tree.
describe("the router — screens fetched when they are visited", () => {
  it("renders a screen that is not part of the first download", async () => {
    serve({
      "/api/v1/setup/status": { body: { setup_needed: false } },
      "/api/v1/auth/me": { body: makeSession() },
      "/api/v1/members": {
        body: [{ id: "user-1", username: "alex", display_name: "Alex", role: "owner" }],
      },
    });
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const router = createRouter({
      routeTree,
      history: createMemoryHistory({ initialEntries: ["/family"] }),
    });
    render(
      <QueryClientProvider client={qc}>
        <RouterProvider router={router} />
      </QueryClientProvider>,
    );

    // The family screen's own heading, from the family screen's own chunk.
    expect(await screen.findByRole("heading", { name: "Семья" })).toBeInTheDocument();
    // The shell is not deferred: it is on the way to every signed-in screen.
    expect(screen.getByRole("link", { name: /Счета/ })).toBeInTheDocument();
  });
});

// #201: a tab on the old build asks for a chunk that is gone after an
// upgrade. The error screen says so and offers a reload instead of a
// blank page.
describe("the router — a screen that fails to load", () => {
  it("shows a reload offer instead of a blank page", async () => {
    const rootRoute = createRootRoute({ component: () => <Outlet /> });
    const brokenRoute = createRoute({
      getParentRoute: () => rootRoute,
      path: "/",
      component: () => {
        throw new TypeError("Failed to fetch dynamically imported module");
      },
    });
    const router = createRouter({
      routeTree: rootRoute.addChildren([brokenRoute]),
      history: createMemoryHistory({ initialEntries: ["/"] }),
      defaultErrorComponent: ScreenCrashed,
    });
    // React reports the throw on the console; that is not what is under test.
    const quiet = vi.spyOn(console, "error").mockImplementation(() => {});
    render(<RouterProvider router={router} />);

    expect(await screen.findByTestId("screen-crashed")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Обновить страницу" })).toBeInTheDocument();
    quiet.mockRestore();
  });

  it("is what the application's own router falls back to", () => {
    expect(appRouter.options.defaultErrorComponent).toBe(ScreenCrashed);
  });
});
