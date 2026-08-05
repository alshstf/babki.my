import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from "@tanstack/react-router";
import "@/i18n";
import { ConnectionDetailPage } from "./detail";
import type { SessionInfo } from "@/api/session";

// openapi-fetch captures globalThis.fetch at import time, so the double has
// to be installed before the imports above run.
vi.hoisted(() => {
  globalThis.fetch = vi.fn() as unknown as typeof fetch;
});

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

// Renders the stub under the route id it reads its param from
// ("/app/settings/connections/$connectionId", see router.tsx) inside a
// pathless "app" layout matching that id, the way detail.test.tsx does for
// the accounts screen.
function renderPage(session: SessionInfo) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["session"], session);
  const rootRoute = createRootRoute();
  const layoutRoute = createRoute({ getParentRoute: () => rootRoute, id: "app" });
  const detailRoute = createRoute({
    getParentRoute: () => layoutRoute,
    path: "/settings/connections/$connectionId",
    component: ConnectionDetailPage,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([layoutRoute.addChildren([detailRoute])]),
    history: createMemoryHistory({ initialEntries: ["/settings/connections/conn-42"] }),
  });
  return render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

describe("ConnectionDetailPage (stub)", () => {
  it("names the connection it was reached for", async () => {
    renderPage(makeSession());

    expect(
      await screen.findByText("Идентификатор подключения: conn-42"),
    ).toBeInTheDocument();
  });

  it("shows the owner-only notice for a non-owner", async () => {
    renderPage(makeSession({ role: "editor" }));

    expect(
      await screen.findByText("Настройки доступны только владельцу пространства"),
    ).toBeInTheDocument();
  });
});
