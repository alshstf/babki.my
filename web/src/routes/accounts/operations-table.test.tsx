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

// Every in_base object the backend publishes says whether its amount was
// struck at a single fx rate or assembled from several — see
// assembled_from_lots in the API contract. Ordinary operations are the single
// rate case, so that is the default here; the transfer tests below pass it
// explicitly, because for them it is the whole point.
type InBase = NonNullable<Operation["in_base"]>;
const inBase = (fields: Omit<InBase, "assembled_from_lots"> & Partial<InBase>): InBase => ({
  assembled_from_lots: false,
  ...fields,
});

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
          in_base: inBase({ amount_minor: 655_000, fee_minor: 32_750, currency: "RUB", rate_on: "2019-03-13" }),
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
            in_base: inBase({
              amount_minor: -655_000,
              fee_minor: 32_750,
              currency: "RUB",
              rate_on: "2019-03-13",
            }),
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
            in_base: inBase({
              amount_minor: 655_000,
              fee_minor: 32_750,
              currency: "RUB",
              // A rate was found exactly on the operation's own date.
              rate_on: "2019-03-14",
            }),
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
            in_base: inBase({
              amount_minor: 655_000,
              fee_minor: 32_750,
              currency: "RUB",
              // 2019-03-14 is a gap (e.g. weekend/holiday the backfill never
              // queries — see internal/marketdata/jobs.go) — FxRateOn falls
              // back to the nearest earlier date that has a rate. Claiming
              // "on the operation's date" here would be false, and the Date
              // column right next to it (14.03.2019) would contradict it.
              rate_on: "2019-03-12",
            }),
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

    // The two tests below are the case the tooltip had no wording for at all,
    // and they are written the way the demo data actually looks: a transfer of
    // shares bought on two earlier days, whose ruble figure is assembled from
    // the rates of those days. rate_on is the newest of them, so a tooltip that
    // decides its wording by comparing rate_on with occurred_on states
    // something false whichever way the comparison happens to land — which is
    // why assembled_from_lots exists and why it must be checked first.
    it("says a transfer's figure comes from the purchase dates instead of claiming a nearest-earlier rate", async () => {
      renderTable({
        operations: [
          makeOperation({
            type: "transfer_in",
            occurred_on: "2026-07-20",
            currency: "USD",
            amount_minor: 190_000,
            fee_minor: 0,
            in_base: inBase({
              // 118 000,00 ₽: two purchases, each at the rate of its own day —
              // never 149 150,00 ₽, which is the same shares priced on the day
              // they changed brokers.
              amount_minor: 11_800_000,
              fee_minor: 0,
              currency: "RUB",
              // The newest of the two purchase dates, NOT the transfer's own
              // date and NOT a fallback for a missing rate: 2026-07-20 has a
              // rate of its own and it was deliberately not used.
              rate_on: "2026-06-15",
              assembled_from_lots: true,
            }),
          }),
        ],
        mode: "base",
        baseCurrency: "RUB",
      });

      const amount = await screen.findByTestId("operation-amount");
      expect(amount).toHaveAttribute(
        "title",
        "Это стоимость покупок, сделанных в другие дни — пересчитана по курсам тех дней, а не по курсу дня перевода. Самый поздний из них — на 15.06.2026",
      );
      // The two wordings that would both be lies here: there IS a rate on the
      // operation's date, and this figure was not converted at one rate at all.
      expect(amount.getAttribute("title")).not.toContain("курса нет");
      expect(amount.getAttribute("title")).not.toContain("по ближайшему");
      expect(amount.getAttribute("title")).not.toContain("на дату операции —");
    });

    it("keeps saying so when the newest purchase happens to fall on the transfer's own date", async () => {
      // rate_on === occurred_on here, which is exactly the shape of an ordinary
      // "converted at the rate of its own day" row. It is still a sum struck at
      // several rates, so the ordinary wording would be just as false as the
      // other one — the flag, not the dates, decides.
      renderTable({
        operations: [
          makeOperation({
            type: "transfer_out",
            occurred_on: "2026-07-20",
            currency: "USD",
            amount_minor: 190_000,
            fee_minor: 0,
            in_base: inBase({
              amount_minor: 12_000_000,
              fee_minor: 0,
              currency: "RUB",
              rate_on: "2026-07-20",
              assembled_from_lots: true,
            }),
          }),
        ],
        mode: "base",
        baseCurrency: "RUB",
      });

      const amount = await screen.findByTestId("operation-amount");
      expect(amount).toHaveAttribute(
        "title",
        "Это стоимость покупок, сделанных в другие дни — пересчитана по курсам тех дней, а не по курсу дня перевода. Самый поздний из них — на 20.07.2026",
      );
      expect(amount.getAttribute("title")).not.toContain("Пересчитано по курсу на дату операции");
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
            in_base: inBase({ amount_minor: 655_000, fee_minor: 0, currency: "RUB", rate_on: "2019-03-13" }),
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
