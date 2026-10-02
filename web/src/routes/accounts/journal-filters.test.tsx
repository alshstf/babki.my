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
import { OperationsTable } from "./operations-table";

const asked = vi.hoisted(() => [] as URL[]);
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});
fetchMock.mockImplementation(async (input: Request) => {
  const url = new URL(input.url);
  if (url.pathname.endsWith("/operations")) asked.push(url);
  const body = url.pathname.endsWith("/operations")
    ? { operations: [], has_more: false }
    : { instruments: [], has_more: false };
  return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
});

afterEach(() => {
  cleanup();
  asked.length = 0;
});

function wrap() {
  const rootRoute = createRootRoute();
  const page = createRoute({
    getParentRoute: () => rootRoute,
    path: "/",
    component: () => (
      <OperationsTable
        accountId="acc-1"
        canDelete
        mode="native"
        baseCurrency="RUB"
        papers={[{ id: "sber", name: "Сбербанк" }]}
      />
    ),
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([page]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

describe("the journal's filter", () => {
  it("asks for the period chosen, says when nothing matches, and resets", async () => {
    wrap();
    await waitFor(() => expect(asked).toHaveLength(1));
    expect(asked[0].searchParams.get("from")).toBeNull();

    fireEvent.change(await screen.findByLabelText("С"), { target: { value: "2026-03-01" } });
    await waitFor(() => expect(asked.at(-1)?.searchParams.get("from")).toBe("2026-03-01"));
    expect(await screen.findByText("Нет операций по этим условиям")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Сбросить" }));
    await waitFor(() => expect(asked.at(-1)?.searchParams.get("from")).toBeNull());
    expect(screen.queryByRole("button", { name: "Сбросить" })).toBeNull();
  });
});
