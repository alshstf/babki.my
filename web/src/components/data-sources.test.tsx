import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from "@tanstack/react-router";
import "@/i18n";
import { DataSourcesList, StaleSourcesNotice } from "./data-sources";

const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

function serve(body: unknown) {
  fetchMock.mockImplementation(() =>
    Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } })),
  );
}

function source(kind: string, overrides: Record<string, unknown> = {}) {
  return {
    kind,
    every_seconds: 1800,
    last_success_at: "2026-10-03T00:30:00Z",
    last_failure_at: null,
    last_error: "",
    stale: false,
    ...overrides,
  };
}

function renderIn(ui: React.ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const root = createRootRoute({ component: () => <>{ui}</> });
  const settings = createRoute({ getParentRoute: () => root, path: "/settings", component: () => null });
  const router = createRouter({
    routeTree: root.addChildren([settings]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  return render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

afterEach(cleanup);

describe("data sources", () => {
  it("names each source, says when it last worked, and shows a failure only while it is the latest word", async () => {
    serve([
      source("marketdata.refresh_quotes"),
      source("marketdata.refresh_fx", {
        last_success_at: "2026-09-28T00:00:00Z",
        last_failure_at: "2026-10-03T00:00:00Z",
        last_error: "cbr: unexpected status 503",
        stale: true,
      }),
      source("tinvest.sync", { last_success_at: "2026-10-03T00:00:00Z", last_failure_at: "2026-10-01T00:00:00Z", last_error: "old" }),
      source("tinvest.refresh_quotes", { last_success_at: null }),
      source("tinvest.refresh_dividends"),
    ]);
    renderIn(<DataSourcesList />);
    const list = await screen.findByTestId("data-sources");
    expect(list.textContent).toContain("Мосбиржа — текущие цены");
    expect(list.textContent).toContain("Т-Инвестиции — календарь дивидендов");
    expect(list.textContent).toContain("ЦБ — курсы валют");
    expect(list.textContent).toContain("cbr: unexpected status 503");
    expect(list.textContent).not.toContain("old");
    expect(list.textContent).toContain("ещё не запускалось");
    expect(screen.getAllByText("давно не обновлялось")).toHaveLength(1);
  });

  it("warns over the figures when a price or rate source has stopped, and only then", async () => {
    serve([source("marketdata.refresh_fx", { stale: true }), source("marketdata.backfill_fx", { stale: true })]);
    const first = renderIn(<StaleSourcesNotice canOpenSettings />);
    const notice = await screen.findByTestId("stale-sources");
    expect(notice.textContent).toContain("ЦБ — курсы валют");
    expect(notice.textContent).not.toContain("история");
    expect(screen.getByRole("link", { name: "Подробнее — в настройках" })).toHaveAttribute("href", "/settings");
    first.unmount();

    serve([source("marketdata.backfill_fx", { stale: true })]);
    renderIn(<StaleSourcesNotice canOpenSettings={false} />);
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByTestId("stale-sources")).toBeNull();
  });
});
