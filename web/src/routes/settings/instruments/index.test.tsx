import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  Outlet,
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from "@tanstack/react-router";
import "@/i18n";
import { InstrumentsPage } from "./index";
import type { SessionInfo } from "@/api/session";
import type { Instrument } from "@/api/instruments";

// openapi-fetch captures globalThis.fetch at import time, so the double has to
// be installed before the imports above run — hence vi.hoisted.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

type Route = { path: string; method?: string; status?: number; body?: unknown };

// A fresh Response per call: mockResolvedValue's single object breaks on the
// second, since a body can only be read once.
function serve(routes: Route[]) {
  fetchMock.mockImplementation(
    (input: RequestInfo | URL, init?: RequestInit) => {
      const url = input instanceof Request ? input.url : String(input);
      const method =
        (input instanceof Request ? input.method : init?.method) ?? "GET";
      const path = new URL(url, "http://localhost").pathname;
      const match = routes.find(
        (r) =>
          path.endsWith(r.path) &&
          (r.method ?? "GET").toUpperCase() === method.toUpperCase(),
      );
      const status = match ? (match.status ?? 200) : 404;
      return Promise.resolve(
        new Response(JSON.stringify(match?.body ?? null), {
          status,
          headers: { "Content-Type": "application/json" },
        }),
      );
    },
  );
}

// The bodies actually sent by `method` to a path, in order — what the screen
// asked the server for, not what it drew afterwards.
async function bodiesSent(method: string): Promise<Record<string, unknown>[]> {
  const calls = fetchMock.mock.calls.filter(([input, init]) => {
    const sent =
      (input instanceof Request
        ? input.method
        : (init as RequestInit | undefined)?.method) ?? "GET";
    return sent.toUpperCase() === method.toUpperCase();
  });
  return Promise.all(
    calls.map(async ([input, init]) => {
      const raw =
        input instanceof Request
          ? await input.clone().text()
          : String((init as RequestInit).body);
      return JSON.parse(raw) as Record<string, unknown>;
    }),
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

function makeInstrument(overrides: Partial<Instrument> = {}): Instrument {
  return {
    id: "instr-1",
    type: "share",
    name: "Apple Inc.",
    ticker: "AAPL",
    isin: "",
    figi: "",
    currency: "USD",
    frozen: false,
    ...overrides,
  };
}

function renderPage(session: SessionInfo = makeSession()) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["session"], session);
  const rootRoute = createRootRoute({ component: () => <Outlet /> });
  const layoutRoute = createRoute({
    getParentRoute: () => rootRoute,
    id: "app",
    component: () => <Outlet />,
  });
  const page = createRoute({
    getParentRoute: () => layoutRoute,
    path: "/settings/instruments",
    component: InstrumentsPage,
  });
  const settings = createRoute({
    getParentRoute: () => layoutRoute,
    path: "/settings",
    component: () => <div>SETTINGS</div>,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      layoutRoute.addChildren([page, settings]),
    ]),
    history: createMemoryHistory({ initialEntries: ["/settings/instruments"] }),
  });
  return render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  fetchMock.mockReset();
});

describe("InstrumentsPage", () => {
  it("lists the catalog and warns on a paper that can never be priced", async () => {
    // THE REASON THIS SCREEN EXISTS. An ISIN is the field the quote worker
    // searches the broker by, so a paper without one is never valued — and an
    // unpriced holding goes into the account's total counted at nought,
    // dragging it down by whatever the paper cost. The owner's Apple and Tesla
    // are exactly this, and nothing in the interface could correct them.
    serve([
      {
        path: "/api/v1/instruments",
        body: { instruments: [makeInstrument()], has_more: false },
      },
    ]);

    renderPage();

    expect(await screen.findByText("Apple Inc.")).toBeInTheDocument();
    expect(screen.getByTestId("instrument-no-isin")).toBeInTheDocument();
  });

  it("says nothing about a missing ISIN on a paper this program never prices", async () => {
    // A currency has no quote coming either way, so the sentence would be true
    // about the field and false about the consequence — which is the whole of
    // what it says.
    serve([
      {
        path: "/api/v1/instruments",
        body: {
          instruments: [
            makeInstrument({ type: "currency", name: "Юань", ticker: "CNY" }),
          ],
          has_more: false,
        },
      },
    ]);

    renderPage();

    expect(await screen.findByText("Юань")).toBeInTheDocument();
    expect(screen.queryByTestId("instrument-no-isin")).not.toBeInTheDocument();
  });

  it("sends only the fields that changed", async () => {
    // A PATCH carrying every field would rewrite values nobody touched —
    // harmless while this form holds all of them, and a silent overwrite the
    // moment it does not. The face value pair is not on this form at all.
    serve([
      {
        path: "/api/v1/instruments",
        body: { instruments: [makeInstrument()], has_more: false },
      },
      {
        path: "/api/v1/instruments/instr-1",
        method: "PATCH",
        body: makeInstrument({ isin: "US0378331005" }),
      },
    ]);

    renderPage();
    fireEvent.click(await screen.findByTestId("instrument-edit-AAPL"));
    fireEvent.change(await screen.findByTestId("instrument-isin"), {
      target: { value: "US0378331005" },
    });
    fireEvent.click(screen.getByTestId("instrument-save"));

    await waitFor(async () => {
      expect(await bodiesSent("PATCH")).toEqual([{ isin: "US0378331005" }]);
    });
  });

  it("cannot save a name that is empty, and says so at the field", async () => {
    serve([
      {
        path: "/api/v1/instruments",
        body: { instruments: [makeInstrument()], has_more: false },
      },
    ]);

    renderPage();
    fireEvent.click(await screen.findByTestId("instrument-edit-AAPL"));
    fireEvent.change(await screen.findByTestId("instrument-name"), {
      target: { value: "  " },
    });

    expect(screen.getByTestId("instrument-name-empty")).toBeInTheDocument();
    expect(screen.getByTestId("instrument-save")).toBeDisabled();
  });

  it("cannot save when nothing was changed", async () => {
    // An empty PATCH is a round trip that can only fail or do nothing, and a
    // save button that answers a form nobody touched invites both.
    serve([
      {
        path: "/api/v1/instruments",
        body: { instruments: [makeInstrument()], has_more: false },
      },
    ]);

    renderPage();
    fireEvent.click(await screen.findByTestId("instrument-edit-AAPL"));

    expect(screen.getByTestId("instrument-save")).toBeDisabled();
  });

  it("offers no editing to anyone but the owner", async () => {
    // The catalog is instance-wide: correcting a row changes it for every
    // member, and the server allows only the owner (403 otherwise). Said by the
    // absence of the control rather than discovered by a save that fails.
    serve([
      {
        path: "/api/v1/instruments",
        body: { instruments: [makeInstrument()], has_more: false },
      },
    ]);

    renderPage(makeSession({ role: "editor" }));

    expect(await screen.findByText("Apple Inc.")).toBeInTheDocument();
    expect(
      screen.queryByTestId("instrument-edit-AAPL"),
    ).not.toBeInTheDocument();
  });
});
