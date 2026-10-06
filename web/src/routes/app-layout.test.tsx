import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider, onlineManager } from "@tanstack/react-query";
import {
  RouterProvider,
  createRootRoute,
  createRoute,
  createRouter,
  createMemoryHistory,
} from "@tanstack/react-router";
import "@/i18n";
import { AppLayout } from "./app-layout";
import { useReportScreenCurrencies } from "@/lib/screen-currencies";
import type { SessionInfo } from "@/api/session";

// The API client captures globalThis.fetch on first import, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// Serves the given endpoints by path suffix and 404s the rest; a fresh
// Response per call. `networkError` makes fetch reject, as a browser with
// no connection does.
function serve(
  routes: Record<string, { status?: number; body?: unknown; networkError?: boolean }>,
) {
  const paths = Object.keys(routes);
  fetchMock.mockImplementation((input: RequestInfo | URL) => {
    const url = input instanceof Request ? input.url : String(input);
    const path = new URL(url, "http://localhost").pathname;
    const match = paths.find((route) => path.endsWith(route));
    const route = match ? routes[match] : undefined;
    if (route?.networkError) return Promise.reject(new TypeError("Failed to fetch"));
    const status = route ? (route.status ?? 200) : 404;
    // A 204 Response may carry no body at all.
    if (status === 204) return Promise.resolve(new Response(null, { status }));
    return Promise.resolve(
      new Response(JSON.stringify(route?.body ?? null), {
        status,
        headers: { "Content-Type": "application/json" },
      }),
    );
  });
}

// AppLayout needs a router and a seeded session; this stub screen reports
// the currency set a test wants, or nothing (like /family).
function ScreenStub({ currencies }: { currencies: string[] | null }) {
  // The hook is always called; null reports nothing.
  useReportScreenCurrencies(currencies ?? []);
  return <div data-testid="screen-stub" />;
}

function wrap(currencies: string[] | null, session: SessionInfo) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["session"], session);
  const rootRoute = createRootRoute({ component: AppLayout });
  const indexRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/",
    component: () => <ScreenStub currencies={currencies} />,
  });
  // Where a completed sign-out lands, so the outcome is visible.
  const loginRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/login",
    component: () => <div data-testid="login-screen" />,
  });
  const routeTree = rootRoute.addChildren([indexRoute, loginRoute]);
  const router = createRouter({ routeTree, history: createMemoryHistory({ initialEntries: ["/"] }) });
  render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  // Returned so a test can check what the browser still holds.
  return qc;
}

function makeSession(overrides: Partial<SessionInfo> = {}): SessionInfo {
  return {
    user: { id: "user-1", username: "alex", display_name: "Alex" },
    role: "owner",
    space_id: "space-1",
    space_name: "Family",
    base_currency: "RUB",
    full_valuation: "nav_and_foreign",
    tax_residency: "RU",
    cost_basis_rules: {
      country: "RU",
      method: "fifo",
      perimeter: "account",
      supported: true,
      notices: [],
    },
    ...overrides,
  };
}

const signOutButton = () => screen.getByRole("button", { name: "Выйти" });

afterEach(() => {
  fetchMock.mockReset();
  onlineManager.setOnline(true);
});

describe("AppLayout — display-currency toggle visibility", () => {
  it("hides the toggle when the mounted screen never reports currencies", async () => {
    serve({ "/api/v1/auth/me": { body: makeSession() } });
    wrap(null, makeSession());
    await screen.findByTestId("screen-stub");
    expect(screen.queryByRole("group", { name: /показывать суммы/i })).not.toBeInTheDocument();
  });

  it("hides the toggle when the mounted screen reports exactly one currency", async () => {
    serve({ "/api/v1/auth/me": { body: makeSession() } });
    wrap(["RUB"], makeSession());
    await screen.findByTestId("screen-stub");
    expect(screen.queryByRole("group", { name: /показывать суммы/i })).not.toBeInTheDocument();
  });

  it("shows the toggle when the mounted screen reports more than one currency", async () => {
    serve({ "/api/v1/auth/me": { body: makeSession() } });
    wrap(["RUB", "USD"], makeSession());
    await screen.findByTestId("screen-stub");
    expect(screen.getByRole("group", { name: /показывать суммы/i })).toBeInTheDocument();
  });
});

// #88: sign-out ignored its answer and went to the login form on any
// failure, while the server session lived on: someone leaves a shared
// computer believing they are out.
describe("AppLayout — sign-out", () => {
  it("says so when the server did not confirm, and does not leave for the login screen", async () => {
    serve({
      "/api/v1/auth/me": { body: makeSession() },
      "/api/v1/auth/logout": { status: 500, body: { error: "internal error" } },
    });
    wrap(null, makeSession());
    await screen.findByTestId("screen-stub");

    fireEvent.click(signOutButton());

    expect(await screen.findByText(/сервер не подтвердил выход/i)).toBeInTheDocument();
    // The login screen means a completed sign-out, never a failed one.
    expect(screen.queryByTestId("login-screen")).not.toBeInTheDocument();
  });

  it("goes to the login screen when the server confirms", async () => {
    serve({
      "/api/v1/auth/me": { body: makeSession() },
      "/api/v1/auth/logout": { status: 204 },
    });
    wrap(null, makeSession());
    await screen.findByTestId("screen-stub");

    fireEvent.click(signOutButton());

    expect(await screen.findByTestId("login-screen")).toBeInTheDocument();
    expect(screen.queryByText(/сервер не подтвердил выход/i)).not.toBeInTheDocument();
  });

  it("says so when the browser reports no connection, and lets the reader try again", async () => {
    // Offline, the default networkMode "online" would pause the mutation:
    // no error banner, a disabled retry, and a delayed sign-out later.
    // useLogout uses networkMode "always", so a dead connection fails at once.
    onlineManager.setOnline(false);
    serve({
      "/api/v1/auth/me": { body: makeSession() },
      "/api/v1/auth/logout": { networkError: true },
    });
    wrap(null, makeSession());
    await screen.findByTestId("screen-stub");
    fetchMock.mockClear();

    fireEvent.click(signOutButton());

    expect(await screen.findByText(/сервер не подтвердил выход/i)).toBeInTheDocument();
    expect(screen.queryByTestId("login-screen")).not.toBeInTheDocument();
    // Attempted, not held.
    expect(fetchMock).toHaveBeenCalled();
    // And the reader can try again — a locked button is the same silence.
    expect(signOutButton()).not.toBeDisabled();
  });

  it("does not leave the previous person's data in this browser", async () => {
    serve({
      "/api/v1/auth/me": { body: makeSession() },
      "/api/v1/auth/logout": { status: 204 },
    });
    const qc = wrap(null, makeSession());
    // Data only the signing-out person could see, cached: only the sign-out
    // can remove it before the next person sees it.
    qc.setQueryData(["accounts"], [{ id: "acc-1", name: "Брокерский счёт" }]);
    await screen.findByTestId("screen-stub");

    fireEvent.click(signOutButton());

    await screen.findByTestId("login-screen");
    expect(qc.getQueryData(["accounts"])).toBeUndefined();
  });

  it("counts a session the server no longer knows as signed out", async () => {
    // A 401 means there was no session left to destroy, so leaving is right
    // (useSession reads a 401 on /auth/me the same way).
    serve({
      "/api/v1/auth/me": { body: makeSession() },
      "/api/v1/auth/logout": { status: 401, body: { error: "authentication required" } },
    });
    wrap(null, makeSession());
    await screen.findByTestId("screen-stub");

    fireEvent.click(signOutButton());

    expect(await screen.findByTestId("login-screen")).toBeInTheDocument();
    expect(screen.queryByText(/сервер не подтвердил выход/i)).not.toBeInTheDocument();
  });
});
