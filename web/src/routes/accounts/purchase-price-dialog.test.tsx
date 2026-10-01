import type { ReactElement } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from "@tanstack/react-router";
import "@/i18n";
import { PurchasePriceDialog } from "./purchase-price-dialog";
import type { Arrival } from "@/api/arrivals";

const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

const fromAnotherBroker: Arrival = {
  operation_id: "op-arrival",
  occurred_on: "2026-06-15",
  quantity: "10",
  currency: "USD",
  cost_minor: 0,
  source: "tinvest",
  note: "",
  from_another_broker: true,
  from_account_id: null,
  purchases: [],
};

const fromOwnAccount: Arrival = {
  ...fromAnotherBroker,
  operation_id: "op-moved",
  occurred_on: "2026-07-01",
  quantity: "5",
  cost_minor: 30_000,
  source: "manual",
  from_another_broker: false,
  from_account_id: "acc-tbank",
  purchases: [{ quantity: "5", cost_minor: 30_000, acquired_on: "2025-01-10" }],
};

let putStatus = 200;
const putBodies: unknown[] = [];

function serve(arrivals: Arrival[]) {
  fetchMock.mockImplementation(async (input: RequestInfo | URL) => {
    const request = input instanceof Request ? input : new Request(String(input));
    const path = new URL(request.url, "http://localhost").pathname;
    const json = (status: number, body: unknown) =>
      new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
    if (path.endsWith("/arrivals")) return json(200, { arrivals });
    if (path.endsWith("/api/v1/accounts")) {
      return json(200, [{ id: "acc-tbank", name: "Брокерский Т-Банк", type: "brokerage", currency: "RUB", institution: "", status: "active", created_at: "2026-01-01T00:00:00Z", balance: null }]);
    }
    if (path.endsWith("/purchases") && request.method === "PUT") {
      putBodies.push(await request.json());
      if (putStatus !== 200) return json(putStatus, { error: "journal would become inconsistent" });
      return json(200, { id: "op-arrival" });
    }
    return json(404, null);
  });
}

function wrap(ui: ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const rootRoute = createRootRoute();
  const page = createRoute({ getParentRoute: () => rootRoute, path: "/", component: () => ui });
  const detail = createRoute({ getParentRoute: () => rootRoute, path: "/accounts/$accountId", component: () => null });
  const router = createRouter({
    routeTree: rootRoute.addChildren([page, detail]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  return render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

const paper = { id: "inst-ko", name: "Coca-Cola", ticker: "KO" };

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
  putBodies.length = 0;
  putStatus = 200;
});

describe("PurchasePriceDialog", () => {
  it("sends the purchases behind shares from another broker, with the price per share for the server to strike", async () => {
    serve([fromAnotherBroker]);
    wrap(<PurchasePriceDialog open onOpenChange={() => {}} accountId="acc-freedom" paper={paper} />);

    expect(await screen.findByText(/Цена покупки не указана/)).toBeInTheDocument();
    const prefix = "arrival-op-arrival-0";
    // The one row starts with every share that arrived.
    expect((screen.getByLabelText(/Количество/) as HTMLInputElement).value).toBe("10");
    fireEvent.change(document.getElementById(`${prefix}-price`)!, { target: { value: "60,10" } });
    fireEvent.change(document.getElementById(`${prefix}-date`)!, { target: { value: "2025-02-03" } });
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));

    await waitFor(() => expect(putBodies).toHaveLength(1));
    expect(putBodies[0]).toEqual({
      purchases: [{ quantity: "10", price: "60.10", cost_minor: null, fee_minor: 0, acquired_on: "2025-02-03" }],
    });
    expect(await screen.findByText(/Сохранено/)).toBeInTheDocument();
  });

  it("does not send purchases that are not exactly the shares that arrived", async () => {
    serve([fromAnotherBroker]);
    wrap(<PurchasePriceDialog open onOpenChange={() => {}} accountId="acc-freedom" paper={paper} />);
    await screen.findByText(/Цена покупки не указана/);
    const prefix = "arrival-op-arrival-0";
    fireEvent.change(document.getElementById(`${prefix}-quantity`)!, { target: { value: "9" } });
    fireEvent.change(document.getElementById(`${prefix}-price`)!, { target: { value: "60" } });

    expect(screen.getByTestId("arrival-op-arrival-counted").textContent).toContain("9 из 10");
    expect(screen.getByRole("button", { name: "Сохранить" })).toBeDisabled();
  });

  it("says why the journal refused the purchases, by the status alone", async () => {
    serve([fromAnotherBroker]);
    putStatus = 409;
    wrap(<PurchasePriceDialog open onOpenChange={() => {}} accountId="acc-freedom" paper={paper} />);
    await screen.findByText(/Цена покупки не указана/);
    fireEvent.change(document.getElementById("arrival-op-arrival-0-price")!, { target: { value: "60" } });
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));

    expect(await screen.findByText(/перевели на другой счёт/)).toBeInTheDocument();
    expect(screen.queryByText(/inconsistent/)).not.toBeInTheDocument();
  });

  it("points shares moved from another of the owner's accounts to that account", async () => {
    serve([fromAnotherBroker, fromOwnAccount]);
    wrap(<PurchasePriceDialog open onOpenChange={() => {}} accountId="acc-freedom" paper={paper} />);

    const moved = await screen.findByTestId("arrival-from-account");
    await waitFor(() => expect(moved.textContent).toContain("«Брокерский Т-Банк»"));
    expect(moved.querySelector("a")?.getAttribute("href")).toBe("/accounts/acc-tbank");
    expect(screen.getAllByTestId("arrival-from-another-broker")).toHaveLength(1);
  });

  it("says when the paper never arrived by a transfer at all", async () => {
    serve([]);
    wrap(<PurchasePriceDialog open onOpenChange={() => {}} accountId="acc-freedom" paper={paper} />);
    expect(await screen.findByTestId("no-arrivals")).toBeInTheDocument();
  });
});
