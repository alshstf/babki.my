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
import { PayoutsReceived } from "./received";
import type { PayoutCheck } from "@/api/payouts";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

const norm = (s: string) => s.replace(/[  ]/g, " ");

const check = (over: Partial<PayoutCheck>): PayoutCheck => ({
  on: "2026-09-02", record_on: "2026-09-01", kind: "coupon", instrument_id: "ofz", account_id: "acc",
  quantity: "15", per_unit: "35.4", amount_minor: 53_100, currency: "RUB", status: "missing",
  got_minor: 0, got_currency: null, got_on: null, ...over,
});

function show(list: PayoutCheck[]) {
  fetchMock.mockImplementation(() =>
    Promise.resolve(new Response(JSON.stringify(list), { status: 200, headers: { "Content-Type": "application/json" } })),
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const root = createRootRoute();
  const page = createRoute({
    getParentRoute: () => root, path: "/",
    component: () => <PayoutsReceived paperName={() => "ОФЗ 26238"} accountName={() => "Брокер"} />,
  });
  const paper = createRoute({ getParentRoute: () => root, path: "/instruments/$instrumentId", component: () => null });
  const router = createRouter({ routeTree: root.addChildren([page, paper]), history: createMemoryHistory({ initialEntries: ["/"] }) });
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("PayoutsReceived", () => {
  it("counts what came and what did not, and says what came of each", async () => {
    show([
      check({ status: "missing" }),
      check({ on: "2026-09-01", kind: "dividend", status: "short", amount_minor: 1_020, currency: "USD", got_minor: 500, got_currency: "USD", got_on: "2026-09-03" }),
      check({ on: "2026-08-05", status: "received", got_minor: 30_800, got_currency: "RUB", got_on: "2026-08-06" }),
    ]);
    expect(norm((await screen.findByTestId("payouts-received-summary")).textContent ?? "")).toBe(
      "пришло: 1 · меньше: 1 · не пришло: 1 · ждём: 0",
    );
    const statuses = screen.getAllByTestId("payout-check-status").map((c) => norm(c.textContent ?? ""));
    expect(statuses[0]).toBe("не пришло");
    expect(statuses[1]).toBe("меньше5,00 $ · 03.09.2026");
    expect(statuses[2]).toBe("пришло308,00 ₽ · 06.08.2026");
  });

  it("is not there while nothing was due", async () => {
    show([]);
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByTestId("payouts-received")).toBeNull();
  });
});
