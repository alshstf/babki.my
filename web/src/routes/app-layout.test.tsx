import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
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
  const routeTree = rootRoute.addChildren([indexRoute]);
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
    ...overrides,
  };
}

describe("AppLayout — display-currency toggle visibility", () => {
  it("hides the toggle when the mounted screen never reports currencies", async () => {
    wrap(null, makeSession());
    await screen.findByTestId("screen-stub");
    expect(screen.queryByRole("group", { name: /показывать суммы/i })).not.toBeInTheDocument();
  });

  it("hides the toggle when the mounted screen reports exactly one currency", async () => {
    wrap(["RUB"], makeSession());
    await screen.findByTestId("screen-stub");
    expect(screen.queryByRole("group", { name: /показывать суммы/i })).not.toBeInTheDocument();
  });

  it("shows the toggle when the mounted screen reports more than one currency", async () => {
    wrap(["RUB", "USD"], makeSession());
    await screen.findByTestId("screen-stub");
    expect(screen.getByRole("group", { name: /показывать суммы/i })).toBeInTheDocument();
  });
});
