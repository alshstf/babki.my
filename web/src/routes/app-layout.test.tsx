import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
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

// The API client captures globalThis.fetch once, when @/api/client is first
// imported (openapi-fetch: `fetch: baseFetch = globalThis.fetch`), so the double
// has to be in place *before* that import — hence vi.hoisted, which runs ahead
// of the import statements above.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// Serves the given endpoints (matched on the path's suffix) and 404s the rest,
// so an unexpected request is loud rather than silently hanging. A fresh
// Response per call: a single one handed to mockResolvedValue works once and
// then throws, because a body can only be consumed once.
function serve(routes: Record<string, { status?: number; body?: unknown }>) {
  const paths = Object.keys(routes);
  fetchMock.mockImplementation((input: RequestInfo | URL) => {
    const url = input instanceof Request ? input.url : String(input);
    const path = new URL(url, "http://localhost").pathname;
    const match = paths.find((route) => path.endsWith(route));
    const route = match ? routes[match] : undefined;
    const status = route ? (route.status ?? 200) : 404;
    // 204 is the sign-out's own success status, and a Response may not carry a
    // body with it at all — JSON.stringify(null) there throws in undici.
    if (status === 204) return Promise.resolve(new Response(null, { status }));
    return Promise.resolve(
      new Response(JSON.stringify(route?.body ?? null), {
        status,
        headers: { "Content-Type": "application/json" },
      }),
    );
  });
}

// AppLayout renders <Outlet/> (needs a router context) and reads the
// session via useSession (needs a QueryClient with ["session"] seeded —
// same pattern as settings/index.test.tsx). The mounted "screen" here is a
// stand-in that reports whatever currency set the test wants, standing in
// for /accounts, /accounts/$id, or a screen (like /family) that never
// reports at all.
function ScreenStub({ currencies }: { currencies: string[] | null }) {
  // The hook itself must always be called (Rules of Hooks) — null just
  // means "report nothing", which is equivalent to a screen (like /family)
  // that never calls useReportScreenCurrencies at all: both leave the
  // provider's count at 0.
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
  // Where a completed sign-out lands. Present so that "did the screen believe
  // the sign-out happened?" is answerable by what is on screen.
  const loginRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/login",
    component: () => <div data-testid="login-screen" />,
  });
  const routeTree = rootRoute.addChildren([indexRoute, loginRoute]);
  const router = createRouter({ routeTree, history: createMemoryHistory({ initialEntries: ["/"] }) });
  return render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

function makeSession(overrides: Partial<SessionInfo> = {}): SessionInfo {
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
    ...overrides,
  };
}

const signOutButton = () => screen.getByRole("button", { name: "Выйти" });

afterEach(() => {
  fetchMock.mockReset();
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

// #88: sign-out was the one mutation in this application that never looked at
// its own answer. Whatever the server said — 500, a dropped connection, nothing
// at all — the screen cleared its caches and went to the login form, and the
// session on the server went on living (handleLogout in internal/family/http.go
// only destroys it when it is actually reached). Someone walks away from a
// shared computer believing they are out.
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
    // The login screen is the picture of a completed sign-out. It must not be
    // the picture of a failed one.
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

  it("counts a session the server no longer knows as signed out", async () => {
    // 401 from this endpoint means RequireAuth found no usable session to
    // destroy (internal/family/session.go) — there is nothing left to sign out
    // of, so refusing to leave the screen would report a failure that did not
    // happen. useSession already reads 401 on /auth/me the same way.
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
