import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { OperationsTable } from "./operations-table";
import {
  ScreenCurrencyCountProvider,
  useHasMultipleScreenCurrencies,
} from "@/lib/screen-currencies";
import { formatMinor } from "@/lib/money";
import { formatDate, localToday } from "@/lib/dates";
import type { DisplayCurrencyMode } from "@/lib/display-currency";
import type { Operation } from "@/api/operations";

// The API client captures globalThis.fetch once, when @/api/client is first
// imported (openapi-fetch: `fetch: baseFetch = globalThis.fetch`), so the
// double has to be in place *before* that import — hence vi.hoisted, which
// runs ahead of the import statements above.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// NBSP-insensitive compare: Intl.NumberFormat uses non-breaking spaces
// (matches the helper in money.test.ts / positions-table.test.tsx). Written
// with explicit escapes so they can't silently get mangled into plain ASCII
// spaces by an editing tool.
const norm = (s: string) => s.replace(/[\u00A0\u202F]/g, " ");

// Serves the given endpoints and 404s everything else, so an unexpected
// request is loud rather than silent. Routes match on the path's *suffix*,
// not on a substring (see the same helper in detail.test.tsx).
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

function makeOperation(overrides: Partial<Operation> = {}): Operation {
  return {
    id: "op-1",
    account_id: "acc-1",
    instrument_id: null,
    type: "deposit",
    // A deliberately old date: the whole point of the journal's conversion is
    // that it uses the rate of the day the operation happened, so a test date
    // that could be confused with "today" would prove nothing.
    occurred_on: "2019-03-14",
    settled_on: null,
    quantity: null,
    price: null,
    amount_minor: 100_00,
    currency: "USD",
    fee_minor: 5_00,
    note: "",
    transfer_group_id: null,
    split_ratio: null,
    source: "manual",
    created_at: "2019-03-14T00:00:00Z",
    ...overrides,
  };
}

// Stand-in for the header's display-currency toggle: visible only when the
// provider says more than one currency is in play on this screen.
function ToggleProbe() {
  const visible = useHasMultipleScreenCurrencies();
  return <div data-testid="toggle">{visible ? "visible" : "hidden"}</div>;
}

function renderTable({
  operations,
  mode = "native",
  baseCurrency = "RUB",
}: {
  operations: Operation[];
  mode?: DisplayCurrencyMode;
  baseCurrency?: string;
}) {
  serve({ "/operations": { body: operations }, "/instruments": { body: [] } });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <ScreenCurrencyCountProvider>
        <ToggleProbe />
        <OperationsTable
          accountId="acc-1"
          canDelete={false}
          mode={mode}
          baseCurrency={baseCurrency}
        />
      </ScreenCurrencyCountProvider>
    </QueryClientProvider>,
  );
}

describe("OperationsTable", () => {
  it("shows the operation's own amount and fee, with no conversion markers, in native mode", async () => {
    renderTable({
      operations: [
        makeOperation({
          currency: "USD",
          amount_minor: 100_00,
          fee_minor: 5_00,
          in_base: { amount_minor: 655_000, fee_minor: 32_750, currency: "RUB", rate_on: "2019-03-13" },
        }),
      ],
      mode: "native",
    });

    const amount = await screen.findByTestId("operation-amount");
    expect(norm(amount.textContent ?? "")).toBe(norm(formatMinor(100_00, "USD")));
    expect(norm(screen.getByTestId("operation-fee").textContent ?? "")).toBe(
      norm(formatMinor(5_00, "USD")),
    );
    // Nothing was converted, so neither an indicator nor a rate-date tooltip
    // has any business being here.
    expect(screen.queryByTestId("operation-amount-not-converted")).not.toBeInTheDocument();
    expect(screen.queryByTestId("operation-fee-not-converted")).not.toBeInTheDocument();
    expect(amount).not.toHaveAttribute("title");
  });

  it("shows no conversion marker in native mode even for an operation that could not be converted", async () => {
    renderTable({
      operations: [makeOperation({ currency: "USD", in_base: null })],
      mode: "native",
      baseCurrency: "RUB",
    });

    expect(await screen.findByTestId("operation-amount")).toBeInTheDocument();
    expect(screen.queryByTestId("operation-amount-not-converted")).not.toBeInTheDocument();
    expect(screen.queryByTestId("operation-fee-not-converted")).not.toBeInTheDocument();
    expect(screen.queryByTitle(/Нет курса/)).not.toBeInTheDocument();
  });

  describe("base mode", () => {
    it("shows the amount and the fee converted into the base currency", async () => {
      renderTable({
        operations: [
          makeOperation({
            currency: "USD",
            amount_minor: -100_00,
            fee_minor: 5_00,
            in_base: {
              amount_minor: -655_000,
              fee_minor: 32_750,
              currency: "RUB",
              rate_on: "2019-03-13",
            },
          }),
        ],
        mode: "base",
        baseCurrency: "RUB",
      });

      const amount = await screen.findByTestId("operation-amount");
      expect(norm(amount.textContent ?? "")).toBe(norm(formatMinor(-655_000, "RUB")));
      // The fee is its own independently converted figure, not a share of the
      // amount — it must come from in_base.fee_minor, not be derived.
      expect(norm(screen.getByTestId("operation-fee").textContent ?? "")).toBe(
        norm(formatMinor(32_750, "RUB")),
      );
      expect(screen.queryByTestId("operation-amount-not-converted")).not.toBeInTheDocument();
      expect(screen.queryByTestId("operation-fee-not-converted")).not.toBeInTheDocument();
    });

    it("claims the operation-date rate only when the fx rate actually matches the operation's date", async () => {
      renderTable({
        operations: [
          makeOperation({
            occurred_on: "2019-03-14",
            currency: "USD",
            in_base: {
              amount_minor: 655_000,
              fee_minor: 32_750,
              currency: "RUB",
              // A rate was found exactly on the operation's own date.
              rate_on: "2019-03-14",
            },
          }),
        ],
        mode: "base",
        baseCurrency: "RUB",
      });

      const amount = await screen.findByTestId("operation-amount");
      // Journal-specific wording: the rate is the one in effect back then.
      // The 4d wording ("Пересчитано по текущему курсу (на 14.03.2019)")
      // names a *current* rate and would misrepresent what this number is.
      expect(amount).toHaveAttribute(
        "title",
        "Пересчитано по курсу на дату операции — 14.03.2019",
      );
      expect(screen.getByTestId("operation-fee")).toHaveAttribute(
        "title",
        "Пересчитано по курсу на дату операции — 14.03.2019",
      );
      // The rate date lives in the tooltip only, never baked into the money
      // cell's own text (14.03.2019 legitimately appears elsewhere, in the
      // pre-existing Date column — that's not what this assertion is about).
      expect(norm(amount.textContent ?? "")).toBe(norm(formatMinor(655_000, "RUB")));
      // And it is emphatically not today's rate.
      const today = formatDate(localToday());
      expect(amount.getAttribute("title")).not.toContain(today);
    });

    it("tells the truth instead of claiming the operation-date rate when the fx rate is from an earlier date", async () => {
      renderTable({
        operations: [
          makeOperation({
            occurred_on: "2019-03-14",
            currency: "USD",
            in_base: {
              amount_minor: 655_000,
              fee_minor: 32_750,
              currency: "RUB",
              // 2019-03-14 is a gap (e.g. weekend/holiday the backfill never
              // queries — see internal/marketdata/jobs.go) — FxRateOn falls
              // back to the nearest earlier date that has a rate. Claiming
              // "on the operation's date" here would be false, and the Date
              // column right next to it (14.03.2019) would contradict it.
              rate_on: "2019-03-12",
            },
          }),
        ],
        mode: "base",
        baseCurrency: "RUB",
      });

      const amount = await screen.findByTestId("operation-amount");
      // Must never claim the rate is "on the operation's date" — it isn't.
      expect(amount.getAttribute("title")).not.toContain("на дату операции —");
      expect(amount).toHaveAttribute(
        "title",
        "На дату операции курса нет — пересчитано по ближайшему, на 12.03.2019",
      );
      expect(screen.getByTestId("operation-fee")).toHaveAttribute(
        "title",
        "На дату операции курса нет — пересчитано по ближайшему, на 12.03.2019",
      );
      // The rate date lives in the tooltip only — never as cell text (the
      // occurred_on date column is a separate, pre-existing thing).
      expect(norm(amount.textContent ?? "")).toBe(norm(formatMinor(655_000, "RUB")));
      expect(screen.queryByText(/12\.03\.2019/)).not.toBeInTheDocument();
      // And it is emphatically not today's rate.
      const today = formatDate(localToday());
      expect(amount.getAttribute("title")).not.toContain(today);
    });

    it("falls back to the operation's own amount plus a marker when it could not be converted", async () => {
      renderTable({
        operations: [
          makeOperation({
            currency: "USD",
            amount_minor: 100_00,
            fee_minor: 5_00,
            in_base: null,
          }),
        ],
        mode: "base",
        baseCurrency: "RUB",
      });

      // Honest native figures — never a dash, never a fabricated zero.
      const amount = await screen.findByTestId("operation-amount");
      expect(norm(amount.textContent ?? "")).toContain(norm(formatMinor(100_00, "USD")));
      expect(amount.textContent).not.toMatch(/₽/);
      expect(amount.textContent).not.toMatch(/—/);
      // "not preceded by a digit" excludes legitimate amounts ending in
      // "0,00" while still catching a fake zero.
      expect(amount.textContent).not.toMatch(/(?<!\d)0,00/);
      expect(norm(screen.getByTestId("operation-fee").textContent ?? "")).toContain(
        norm(formatMinor(5_00, "USD")),
      );

      // The marker names the operation's currency, not the account's: a
      // foreign-currency operation can sit on a base-currency account.
      expect(screen.getByTestId("operation-amount-not-converted")).toHaveAttribute(
        "title",
        "Нет курса на дату операции — показано в валюте операции",
      );
      expect(screen.getByTestId("operation-fee-not-converted")).toBeInTheDocument();
      // No conversion happened, so no rate date may be claimed.
      expect(amount).not.toHaveAttribute("title");
    });

    it("shows a plain amount with no marker when the operation is already in the base currency", async () => {
      renderTable({
        operations: [
          makeOperation({ currency: "RUB", amount_minor: 100_00, fee_minor: 5_00, in_base: null }),
        ],
        mode: "base",
        baseCurrency: "RUB",
      });

      const amount = await screen.findByTestId("operation-amount");
      expect(norm(amount.textContent ?? "")).toBe(norm(formatMinor(100_00, "RUB")));
      expect(screen.queryByTestId("operation-amount-not-converted")).not.toBeInTheDocument();
      expect(screen.queryByTestId("operation-fee-not-converted")).not.toBeInTheDocument();
    });

    it("keeps the dash for a zero fee instead of converting nothing into something", async () => {
      renderTable({
        operations: [
          makeOperation({
            currency: "USD",
            fee_minor: 0,
            in_base: { amount_minor: 655_000, fee_minor: 0, currency: "RUB", rate_on: "2019-03-13" },
          }),
        ],
        mode: "base",
        baseCurrency: "RUB",
      });

      await screen.findByTestId("operation-amount");
      expect(screen.queryByTestId("operation-fee")).not.toBeInTheDocument();
    });
  });

  describe("screen currency reporting", () => {
    it("makes the toggle appear when only the journal is multi-currency", async () => {
      // A USD operation on a RUB account with a RUB base currency: the
      // account and its positions report a single currency, so without the
      // journal reporting too, the user would have no way to switch.
      renderTable({
        operations: [makeOperation({ currency: "USD" })],
        baseCurrency: "RUB",
      });

      await screen.findByTestId("operation-amount");
      expect(screen.getByTestId("toggle")).toHaveTextContent("visible");
    });

    it("leaves the toggle hidden when every operation is already in the base currency", async () => {
      renderTable({
        operations: [makeOperation({ currency: "RUB" })],
        baseCurrency: "RUB",
      });

      await screen.findByTestId("operation-amount");
      expect(screen.getByTestId("toggle")).toHaveTextContent("hidden");
    });
  });
});
