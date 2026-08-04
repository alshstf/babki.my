import { describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { OperationsTable } from "./operations-table";
import {
  ScreenCurrencyCountProvider,
  useHasMultipleScreenCurrencies,
} from "@/lib/screen-currencies";
import { formatMinor } from "@/lib/money";
import type { Instrument } from "@/api/instruments";
import { formatDate, localToday } from "@/lib/dates";
import type { DisplayCurrencyMode } from "@/lib/display-currency";
import { JOURNAL_PAGE_SIZE, type Operation } from "@/api/operations";
import type { CostBasisRules } from "@/api/tax-residencies";

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
    // An ordinary operation's amount belongs to the day it happened, so there
    // are no purchase dates for it to be missing — see has_undated_lots in the
    // API contract. Only the transfer tests below set it.
    has_undated_lots: false,
    // Properties of the OPERATION, not of in_base (see the API contract) —
    // true only for a transfer whose parcel has a stored breakdown, which is
    // never the case for these ordinary defaults. The transfer tests below
    // set it explicitly, because for them it is the whole point.
    assembled_from_lots: false,
    ...overrides,
  };
}

type InBase = NonNullable<Operation["in_base"]>;
const inBase = (fields: InBase): InBase => fields;

// Stand-in for the header's display-currency toggle: visible only when the
// provider says more than one currency is in play on this screen.
function ToggleProbe() {
  const visible = useHasMultipleScreenCurrencies();
  return <div data-testid="toggle">{visible ? "visible" : "hidden"}</div>;
}

// A country whose rules are not what this application computes, in two
// separate ways at once — so the caveat, wherever it appears, has two
// sentences in it and dropping either would be visible.
const britain: CostBasisRules = {
  country: "GB",
  method: "average",
  perimeter: "owner",
  supported: false,
  notices: ["method_mismatch", "perimeter_mismatch"],
};

// A catalog entry, as the instruments endpoint returns it. Only the price
// tests need one: everything else renders operations with no instrument at
// all, or lets the name fall back to an id suffix.
function makeInstrument(overrides: Partial<Instrument> = {}): Instrument {
  return {
    id: "instr-1",
    type: "share",
    name: "Сбербанк",
    ticker: "SBER",
    isin: "",
    figi: "",
    currency: "RUB",
    frozen: false,
    ...overrides,
  };
}

function renderTable({
  operations,
  instruments = [],
  hasMore = false,
  mode = "native",
  baseCurrency = "RUB",
  costBasisRules,
}: {
  operations: Operation[];
  // The catalog page the table looks names and types up in. Empty by default,
  // which is also what a real page looks like for an instrument past the
  // fiftieth (see useInstruments).
  instruments?: Instrument[];
  // Whether the server says the journal continues past this page. Defaults to
  // false — every test that is not about paging is looking at a whole journal.
  hasMore?: boolean;
  mode?: DisplayCurrencyMode;
  baseCurrency?: string;
  // Omitted by every test that is not about the cost basis caveat, exactly as
  // the screen omits it while the session is still loading.
  costBasisRules?: CostBasisRules;
}) {
  serve({
    "/operations": { body: { operations, has_more: hasMore } },
    "/instruments": { body: instruments },
  });
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
          costBasisRules={costBasisRules}
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
          in_base: inBase({
            amount_minor: 655_000,
            fee_minor: 32_750,
            currency: "RUB",
            rate_on: "2019-03-13",
            dated_on: "2019-03-14",
          }),
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
              dated_on: "2019-03-14",
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
              // A rate was found exactly on the operation's own date, so the
              // date asked for and the date it came from are the same.
              rate_on: "2019-03-14",
              dated_on: "2019-03-14",
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
              dated_on: "2019-03-14",
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
            assembled_from_lots: true,
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
              // The purchase this figure is dated by. Equal to rate_on here
              // because that day had a rate of its own.
              dated_on: "2026-06-15",
            }),
          }),
        ],
        mode: "base",
        baseCurrency: "RUB",
      });

      const amount = await screen.findByTestId("operation-amount");
      expect(amount).toHaveAttribute(
        "title",
        "Это стоимость покупок, и каждая её часть пересчитана по курсу дня своей покупки. Самый поздний из них — на 15.06.2026",
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
            assembled_from_lots: true,
            in_base: inBase({
              amount_minor: 12_000_000,
              fee_minor: 0,
              currency: "RUB",
              rate_on: "2026-07-20",
              dated_on: "2026-07-20",
            }),
          }),
        ],
        mode: "base",
        baseCurrency: "RUB",
      });

      const amount = await screen.findByTestId("operation-amount");
      expect(amount).toHaveAttribute(
        "title",
        "Это стоимость покупок, и каждая её часть пересчитана по курсу дня своей покупки. Самый поздний из них — на 20.07.2026",
      );
      expect(amount.getAttribute("title")).not.toContain("Пересчитано по курсу на дату операции");
    });

    it("stays true when every piece of the parcel was bought on the transfer's own day (#same-day)", async () => {
      // The falsity FINDING 1 caught: the sibling test above only puts the
      // NEWEST purchase on the transfer's own day — the flag decides the
      // branch, not the dates, so that fixture proves the branch selection but
      // not the sentence's truth. Here EVERY piece, and the transfer itself,
      // share one day. internal/portfolio/engine.go's CheckTransferLots
      // rejects only a purchase date AFTER the transfer, so buy-then-transfer
      // on the same calendar day is a row a user can actually create — a
      // RUB-based space, a USD account, 10 shares bought on 2026-03-10 and the
      // whole parcel moved that same afternoon. Both legs publish
      // assembled_from_lots true with dated_on === rate_on === occurred_on,
      // and the figure genuinely WAS struck at that one day's rate. The old
      // wording ("...сделанных в другие дни ... а не по курсу дня перевода")
      // asserted the opposite of both facts; the rule-naming form makes no
      // claim about which day the purchase fell on, so it stays true here too.
      renderTable({
        operations: [
          makeOperation({
            type: "transfer_in",
            occurred_on: "2026-03-10",
            currency: "USD",
            amount_minor: 190_000,
            fee_minor: 0,
            assembled_from_lots: true,
            in_base: inBase({
              amount_minor: 1_805_000,
              fee_minor: 0,
              currency: "RUB",
              rate_on: "2026-03-10",
              dated_on: "2026-03-10",
            }),
          }),
        ],
        mode: "base",
        baseCurrency: "RUB",
      });

      const amount = await screen.findByTestId("operation-amount");
      expect(amount).toHaveAttribute(
        "title",
        "Это стоимость покупок, и каждая её часть пересчитана по курсу дня своей покупки. Самый поздний из них — на 10.03.2026",
      );
      // Neither false claim from the old wording may reappear.
      expect(amount.getAttribute("title")).not.toContain("в другие дни");
      expect(amount.getAttribute("title")).not.toContain("а не по курсу дня перевода");
    });

    it("names the day the purchase happened, not the day the rate came from (#80)", async () => {
      // The two dates come apart for about a third of the calendar: the CBR
      // publishes nothing at weekends and holidays, so a parcel whose newest
      // purchase fell on a Sunday is valued at Friday's rate. The sentence
      // this caption ends with is about a PURCHASE — «самый поздний из них» —
      // and `dated_on` is the field that publishes purchase dates, while
      // `rate_on` publishes the day the rate that answered came from. Naming
      // the latter as the former printed a day nothing was bought on.
      renderTable({
        operations: [
          makeOperation({
            type: "transfer_in",
            occurred_on: "2026-07-20",
            currency: "USD",
            amount_minor: 190_000,
            fee_minor: 0,
            assembled_from_lots: true,
            in_base: inBase({
              amount_minor: 11_800_000,
              fee_minor: 0,
              currency: "RUB",
              // 14.06.2026 is a Sunday: no rate of its own, so the newest
              // purchase in the parcel was valued at the 12th's.
              rate_on: "2026-06-12",
              dated_on: "2026-06-14",
            }),
          }),
        ],
        mode: "base",
        baseCurrency: "RUB",
      });

      const amount = await screen.findByTestId("operation-amount");
      expect(amount).toHaveAttribute(
        "title",
        "Это стоимость покупок, и каждая её часть пересчитана по курсу дня своей покупки. Самый поздний из них — на 14.06.2026",
      );
      // The rate's own day has no business being called a purchase date.
      expect(amount.getAttribute("title")).not.toContain("12.06.2026");
    });

    it("decides «rate of that very day» against dated_on, not against occurred_on", async () => {
      // WHICH FIELD IS READ, not what this payload deserves: the contract
      // makes dated_on equal to occurred_on on every row that is not
      // assembled from lots (see OperationInBase.dated_on), so no payload the
      // server can produce tells the two comparisons apart, and this one
      // cannot occur. It is here because the pair means different things —
      // dated_on is the day the figure is VALUED at, occurred_on is a
      // property of the row — and a comparison against the second is a second
      // source for one answer, which is how the caption came to compare a
      // purchase date's rate against the day the paperwork moved. The wording
      // it produces below is therefore not endorsed as true of this
      // impossible fixture; the assertion is about which of the two fields
      // the code consulted.
      renderTable({
        operations: [
          makeOperation({
            occurred_on: "2019-03-15",
            currency: "USD",
            in_base: inBase({
              amount_minor: 655_000,
              fee_minor: 32_750,
              currency: "RUB",
              rate_on: "2019-03-14",
              dated_on: "2019-03-14",
            }),
          }),
        ],
        mode: "base",
        baseCurrency: "RUB",
      });

      const amount = await screen.findByTestId("operation-amount");
      expect(amount).toHaveAttribute(
        "title",
        "Пересчитано по курсу на дату операции — 14.03.2019",
      );
    });

    it("falls back to the operation's own amount plus a marker when it could not be converted", async () => {
      renderTable({
        operations: [
          makeOperation({
            currency: "USD",
            amount_minor: 100_00,
            fee_minor: 5_00,
            in_base: null,
            in_base_gap: "no_rate_operation_date",
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
        "Нет курса на дату операции, а сумма считается по курсу того дня. Если курс появится при обновлении курсов, операция посчитается сама. Поэтому пока числа этой строки показаны в валюте операции",
      );
      // FINDING 2 of the caption-truth review: nothing previously pinned the
      // fee cell to the SAME per-cause sentence as the amount cell. in_base is
      // published as a whole or not at all (see rowGapTitle's block comment
      // above), so a single unvaluable term withholds both money cells of the
      // row together, and both must carry the one true explanation — never
      // the fee cell silently downgraded to the vague general phrase while the
      // amount cell next to it keeps the specific, true one.
      expect(screen.getByTestId("operation-fee-not-converted")).toHaveAttribute(
        "title",
        "Нет курса на дату операции, а сумма считается по курсу того дня. Если курс появится при обновлении курсов, операция посчитается сама. Поэтому пока числа этой строки показаны в валюте операции",
      );
      // No conversion happened, so no rate date may be claimed.
      expect(amount).not.toHaveAttribute("title");
    });

    it("blames the missing purchase dates, not a missing rate, on a transfer that has none", async () => {
      // The twin of the test above, and the reason the server publishes a
      // cause at all. Both rows are unconverted; only one of them is
      // unconverted because no rate has been fetched yet. A transfer whose
      // parcel was never broken down carries a cost basis assembled on days
      // nobody recorded — and the transfer's OWN date usually does have a rate
      // (the demo instance has one for 2026-07-20, the day this fixture is
      // dated), so "нет курса на дату операции" here is not a vague
      // explanation but a false one, promising a figure that will never
      // arrive. The positions screen was taught to tell these two apart in an
      // earlier plan; the journal says it about the very same shares.
      renderTable({
        operations: [
          makeOperation({
            id: "op-transfer-out",
            type: "transfer_out",
            occurred_on: "2026-07-20",
            currency: "USD",
            amount_minor: 190_00,
            fee_minor: 0,
            in_base: null,
            has_undated_lots: true,
            in_base_gap: "undated_lot",
          }),
        ],
        mode: "base",
        baseCurrency: "RUB",
      });

      expect(await screen.findByTestId("operation-amount-not-converted")).toHaveAttribute(
        "title",
        "Не записано, когда куплена эта партия или часть её, а её стоимость считается по курсам на дни покупок. Восстановить эти даты уже неоткуда: в базовой валюте эта операция сама не посчитается. Поэтому числа этой строки показаны в валюте операции",
      );
      // The figure itself is untouched: an unknown date costs no money.
      expect(norm(screen.getByTestId("operation-amount").textContent ?? "")).toContain(
        norm(formatMinor(190_00, "USD")),
      );
    });

    it("says nothing rather than half a sentence when the rate date does not parse", async () => {
      // Unreachable through the server — rate_on is a date or the object is
      // not published — but every journal wording ends in the rate date, and a
      // wording handed nothing to end with produces "…пересчитано по
      // ближайшему, на " with the sentence cut off mid-air. The rule was in
      // MoneyCell until callers began supplying their own wordings, at which
      // point it quietly stopped applying to them; it now belongs to the
      // caller, which is the only one that knows whether its sentence needs a
      // date at all.
      renderTable({
        operations: [
          makeOperation({
            currency: "USD",
            in_base: inBase({
              amount_minor: 655_000,
              fee_minor: 32_750,
              currency: "RUB",
              rate_on: "2019-13-99",
              dated_on: "2019-03-14",
            }),
          }),
        ],
        mode: "base",
        baseCurrency: "RUB",
      });

      const amount = await screen.findByTestId("operation-amount");
      expect(amount).not.toHaveAttribute("title");
      expect(screen.getByTestId("operation-fee")).not.toHaveAttribute("title");
      // The figure itself is published as usual: an unreadable caption is a
      // reason to drop the caption, not the number.
      expect(norm(amount.textContent ?? "")).toBe(norm(formatMinor(655_000, "RUB")));
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
            in_base: inBase({
              amount_minor: 655_000,
              fee_minor: 0,
              currency: "RUB",
              rate_on: "2019-03-13",
              dated_on: "2019-03-14",
            }),
          }),
        ],
        mode: "base",
        baseCurrency: "RUB",
      });

      await screen.findByTestId("operation-amount");
      expect(screen.queryByTestId("operation-fee")).not.toBeInTheDocument();
    });
  });

  // Issues #79 and #80. One phrase — «Нет курса на дату операции» — used to
  // hang over every unconverted row in this table, including the transfers
  // whose gap has nothing to do with the operation's date and the ones whose
  // figure is not waiting for a rate at all. The server now says which term it
  // stopped on (Operation.in_base_gap) and this table says what the server
  // said, exactly as the positions screen does.
  describe("the unconverted caption", () => {
    // The one sentence here that is not the server's answer but the absence of
    // one. Spelled out in full rather than read back out of ru.json, like every
    // other caption in this file: what is being tested is WHICH sentence a
    // marker gets, and a test that fetched it through the component's own
    // lookup would agree with the component whatever it picked.
    //
    // Its twin on the positions screen (CAPTION.general there) says the same
    // thing about a position, in the same two clauses and the same order. The
    // two differ only where they must — «операция»/«позиция», and each naming
    // its own screen's native currency, exactly as the four named sentences
    // already differ between the two tables. They are edited together for the
    // reason the components' own comments give: both tables are stacked on one
    // account page, so a reader meets both wordings in one glance.
    const GENERAL_CAPTION =
      "В базовой валюте эта операция не посчиталась, а причина не названа. Поэтому числа этой строки показаны в валюте операции";

    // Everything below is the same unconverted row seen in base mode; only the
    // cause differs, which is the whole point.
    const unconverted = (overrides: Partial<Operation>): Operation =>
      makeOperation({
        currency: "USD",
        amount_minor: 190_00,
        fee_minor: 5_00,
        in_base: null,
        ...overrides,
      });

    // Unmounts whatever a previous call rendered: a case that asks for several
    // causes in a row would otherwise leave two tables in one document and
    // every query below would find two markers.
    const captionFor = async (overrides: Partial<Operation>): Promise<string> => {
      cleanup();
      renderTable({
        operations: [unconverted(overrides)],
        mode: "base",
        baseCurrency: "RUB",
      });
      const marker = await screen.findByTestId("operation-amount-not-converted");
      return marker.getAttribute("title") ?? "";
    };

    it("blames the purchase day, never the operation's day, when a lot's rate is the one missing", async () => {
      // The whole of #79. This row's amount is a cost basis assembled from
      // purchases made on other days; the transfer's own date usually has a
      // perfectly good rate and is simply not a rate that may value shares
      // bought on other days.
      const title = await captionFor({
        type: "transfer_in",
        occurred_on: "2026-07-20",
        assembled_from_lots: true,
        in_base_gap: "no_rate_lot_date",
      });

      expect(title).toBe(
        "Сумма этой строки — стоимость покупок, и каждая её часть считается по курсу на день своей покупки. Нет курса на день одной из этих покупок. Если курс появится при обновлении курсов, операция посчитается сама. Поэтому пока числа этой строки показаны в валюте операции",
      );
      // Deliberately NOT «покупок, сделанных в другие дни»: a parcel can hold
      // a piece bought on the transfer's own day, and this row's rate is
      // missing for a PURCHASE day whichever day that turns out to be. The
      // sentence names the rule — each part at the rate of its own purchase —
      // which holds however the days fall.
      expect(title).not.toContain("в другие дни");
      // The sentence this replaces, in the exact shape it had.
      expect(title).not.toContain("Нет курса на дату операции");
    });

    it("names the operation's own day when that is the day the server stopped on", async () => {
      expect(await captionFor({ in_base_gap: "no_rate_operation_date" })).toBe(
        "Нет курса на дату операции, а сумма считается по курсу того дня. Если курс появится при обновлении курсов, операция посчитается сама. Поэтому пока числа этой строки показаны в валюте операции",
      );
    });

    it("says an unrecorded purchase date is never coming back", async () => {
      const title = await captionFor({
        type: "transfer_out",
        has_undated_lots: true,
        in_base_gap: "undated_lot",
      });

      expect(title).toBe(
        "Не записано, когда куплена эта партия или часть её, а её стоимость считается по курсам на дни покупок. Восстановить эти даты уже неоткуда: в базовой валюте эта операция сама не посчитается. Поэтому числа этой строки показаны в валюте операции",
      );
    });

    it("separates the gap that closes itself from the one that never will", async () => {
      // The difference this whole field exists for, asserted as a difference
      // rather than as three separate strings: a missing rate is a gap the
      // backfill closes and the figure appears later, an unrecorded purchase
      // date resolves never, because nobody wrote it down. A caption that
      // promised the second row a figure would be promising one that is not
      // coming.
      const undated = await captionFor({
        has_undated_lots: true,
        in_base_gap: "undated_lot",
      });
      expect(undated).toContain("уже неоткуда");
      expect(undated.toLowerCase()).not.toContain("курс появится");

      // #105's second half, worded exactly as the positions screen words it
      // (see the twin assertion there for the whole argument): the rate's
      // ARRIVAL is not this program's to promise — one source, its own list of
      // currencies, and a pair it publishes no leg of never gets a rate — so
      // the sentence states the consequence conditionally instead. Both
      // screens carry the same «Если», because a reader who sees the two
      // tables stacked on one account page reads one condition, not two.
      for (const gap of ["no_rate_operation_date", "no_rate_lot_date"] as const) {
        const temporary = await captionFor({ in_base_gap: gap });
        expect(temporary).toContain("Если курс появится при обновлении курсов");
        // The bare promise, in the exact shape it had.
        expect(temporary).not.toContain("Курс появится при обновлении курсов");
        expect(temporary).not.toContain("уже неоткуда");
      }
    });

    it("takes the cause from the gap the server published, not from the flag beside it", async () => {
      // has_undated_lots answers a coarser question and stays published for
      // the two responses that carry no gap at all, so the two fields are
      // both on this object and can be read for two different things. The
      // caption has exactly one source. A row whose parcel is fully dated but
      // whose FIRST piece has no rate yet is the shape that tells them apart:
      // the flag says "dateless" is not the story, and the gap says which day
      // is.
      const title = await captionFor({
        type: "transfer_in",
        assembled_from_lots: true,
        has_undated_lots: false,
        in_base_gap: "no_rate_lot_date",
      });

      expect(title).toContain("Нет курса на день одной из этих покупок");
      expect(title).not.toContain("уже неоткуда");
    });

    it("degrades to a phrase that names no cause at all for one this build cannot name", async () => {
      // A server newer than this client. The value is not in the union this
      // build was compiled against, and the row must still explain itself:
      // vague but true beats a blank tooltip, and beats a thrown render even
      // more.
      const title = await captionFor({
        in_base_gap: "no_rate_next_tuesday" as Operation["in_base_gap"],
      });

      expect(title).toBe(GENERAL_CAPTION);
      // Vague on purpose: an unnameable cause may be about any day at all, so
      // the fallback names none.
      expect(title).not.toContain("дату операции");
      expect(title).not.toContain("покуп");
      // #105, and the reason this sentence changed. It used to open «Нет
      // курса», naming a RATE — on the one path that is reached precisely
      // because the cause is unknown to this build. `undated_lot` is in
      // TODAY's enum and is not about a rate at all, so the next date-shaped
      // cause the server adds would read «нет курса» on every client one
      // release behind: the defect (#66) the four named sentences exist to
      // end, returning through the path built to degrade safely. Checked both
      // as the old opening verbatim and as the word in any form, since what
      // must be absent is the CAUSE, not one phrasing of it.
      expect(title).not.toContain("Нет курса");
      expect(title.toLowerCase()).not.toContain("курс");
      expect(title).toContain("не посчиталась");
    });

    it("degrades to the same phrase for a server that publishes no cause", async () => {
      // An older server: the field is absent from the payload entirely.
      const title = await captionFor({});

      expect(title).toBe(GENERAL_CAPTION);
    });
  });

  // Issues #61 and the review of its first fix. The statement "the queue that
  // picked this cost basis is not your country's" is true of a transferred
  // parcel's amount and of nothing else in the journal, so it hangs on those
  // amounts. It used to be a banner over the whole table, which put it above
  // every deposit, purchase and dividend in the window as well.
  describe("the cost basis caveat", () => {
    // The shape the demo data actually has: a parcel with a recorded, dated
    // breakdown, so the server converts it piece by piece and says so.
    const assembledTransfer = (overrides: Partial<Operation> = {}): Operation =>
      makeOperation({
        id: "op-transfer",
        type: "transfer_in",
        occurred_on: "2026-07-20",
        currency: "USD",
        amount_minor: 190_000,
        fee_minor: 0,
        assembled_from_lots: true,
        in_base: inBase({
          amount_minor: 11_800_000,
          fee_minor: 0,
          currency: "RUB",
          rate_on: "2026-06-15",
          dated_on: "2026-06-15",
        }),
        ...overrides,
      });

    it("hangs the caveat on the arriving leg's own amount, not over the table", async () => {
      renderTable({
        operations: [assembledTransfer()],
        mode: "base",
        baseCurrency: "RUB",
        costBasisRules: britain,
      });

      const caveat = await screen.findByTestId("operation-amount-caveat");
      const title = caveat.getAttribute("title") ?? "";
      // Both divergences: reporting one hides the other.
      expect(title).toContain("не самая ранняя покупка");
      expect(title).toContain("сразу по всем счетам владельца");
      // The country, so "в этой стране" has a referent, and what the figure
      // is, which a tooltip on a single cell has to supply for itself.
      expect(title).toContain("Великобритания");
      expect(title).toContain("стоимость бумаг");
      // Not a block of prose over the table any more.
      expect(screen.queryByTestId("cost-basis-notice")).not.toBeInTheDocument();
      // And nothing leaked into the cell's text: the caveat is a tooltip.
      expect(norm(screen.getByTestId("operation-amount").textContent ?? "")).toBe(
        norm(formatMinor(11_800_000, "RUB")),
      );
    });

    it("hangs it on the departing leg exactly as on the arriving one", async () => {
      // The contract raises the flag on BOTH legs — they describe one parcel,
      // one basis, one set of purchases (see Operation.in_base). The departing
      // leg is where the cost actually leaves the account, and until this test
      // existed, a rule that recognised only the arriving one broke nothing.
      renderTable({
        operations: [assembledTransfer({ id: "op-transfer-out", type: "transfer_out" })],
        mode: "base",
        baseCurrency: "RUB",
        costBasisRules: britain,
      });

      const title = (await screen.findByTestId("operation-amount-caveat")).getAttribute("title");
      expect(title).toContain("не самая ранняя покупка");
      expect(title).toContain("сразу по всем счетам владельца");
    });

    it("leaves the rows it is not true of unqualified", async () => {
      // The whole point of moving it off the table header. A deposit and a
      // dividend are money that moved on the day they are dated; no queue
      // picked either of them, and a caveat over them is a false statement
      // about them, not a cautious one.
      renderTable({
        operations: [
          assembledTransfer(),
          makeOperation({ id: "op-deposit", type: "deposit" }),
          makeOperation({ id: "op-dividend", type: "dividend" }),
        ],
        mode: "base",
        baseCurrency: "RUB",
        costBasisRules: britain,
      });

      await screen.findByTestId("operation-amount-caveat");
      expect(screen.getAllByTestId("operation-amount-caveat")).toHaveLength(1);
      // Three rows on screen, one qualified figure.
      expect(screen.getAllByTestId("operation-amount")).toHaveLength(3);
      // The fee is never a cost basis either: it is a broker's charge on the
      // day it was charged. (A real transfer carries none; this row has one so
      // that there is a fee cell to check at all.)
      expect(screen.queryByTestId("operation-fee-caveat")).not.toBeInTheDocument();
    });

    it("asks the server which rows publish a cost basis instead of keeping a list of types", async () => {
      // assembled_from_lots is set from the presence of a stored breakdown, not
      // from the operation's type, so a type this screen has never heard of
      // carries the caveat the day the server starts deriving a basis for it. A
      // list of types kept here would silently stop matching instead — the
      // exact failure the caveat exists to prevent, committed by the code that
      // draws it.
      renderTable({
        operations: [assembledTransfer({ id: "op-conversion", type: "conversion" })],
        mode: "base",
        baseCurrency: "RUB",
        costBasisRules: britain,
      });

      expect(await screen.findByTestId("operation-amount-caveat")).toBeInTheDocument();
    });

    it("still qualifies a parcel whose purchase dates were never recorded", async () => {
      // Nothing converts here, so in_base is absent. has_undated_lots — a
      // property of the operation, not of in_base — says the amount is a
      // cost basis all the same, on every row whether or not a conversion was
      // attempted.
      renderTable({
        operations: [
          makeOperation({
            id: "op-transfer-undated",
            type: "transfer_out",
            currency: "USD",
            in_base: null,
            has_undated_lots: true,
            in_base_gap: "undated_lot",
          }),
        ],
        mode: "base",
        baseCurrency: "RUB",
        costBasisRules: britain,
      });

      expect(await screen.findByTestId("operation-amount-caveat")).toBeInTheDocument();
      // Two separate statements about one figure, each with its own indicator:
      // which currency it is shown in, and what the number is.
      expect(screen.getByTestId("operation-amount-not-converted")).toHaveAttribute(
        "title",
        "Не записано, когда куплена эта партия или часть её, а её стоимость считается по курсам на дни покупок. Восстановить эти даты уже неоткуда: в базовой валюте эта операция сама не посчитается. Поэтому числа этой строки показаны в валюте операции",
      );
    });

    it("still qualifies a parcel that has a full breakdown but is already in the base currency (#67)", async () => {
      // The exact hole #67 tracked: the parcel's breakdown is complete and
      // every purchase date is known (has_undated_lots false), yet in_base is
      // null for the most ordinary reason there is — currency already equals
      // baseCurrency, so nothing gets converted and no rate is even asked
      // for. Before assembled_from_lots moved onto the operation, it lived
      // only inside in_base and vanished right along with it here, so this
      // exact row — a RUB transfer in a RUB-based space, the product owner's
      // own case — published no signal at all that its amount was a cost
      // basis, and the caveat silently failed to appear.
      renderTable({
        operations: [
          makeOperation({
            id: "op-transfer-same-currency",
            type: "transfer_in",
            currency: "RUB",
            amount_minor: 11_800_000,
            fee_minor: 0,
            in_base: null,
            has_undated_lots: false,
            assembled_from_lots: true,
          }),
        ],
        mode: "base",
        baseCurrency: "RUB",
        costBasisRules: britain,
      });

      expect(await screen.findByTestId("operation-amount-caveat")).toBeInTheDocument();
      // Already in the base currency, so nothing is "not converted" either —
      // the caveat is the only marker this row carries.
      expect(screen.queryByTestId("operation-amount-not-converted")).not.toBeInTheDocument();
    });

    it("says nothing when the queue this application computes is the country's own", async () => {
      renderTable({
        operations: [assembledTransfer()],
        mode: "base",
        baseCurrency: "RUB",
        costBasisRules: {
          country: "RU",
          method: "fifo",
          perimeter: "account",
          supported: true,
          notices: [],
        },
      });

      await screen.findByTestId("operation-amount");
      expect(screen.queryByTestId("operation-amount-caveat")).not.toBeInTheDocument();
    });

    it("waits rather than guesses while the session has not arrived", async () => {
      renderTable({ operations: [assembledTransfer()], mode: "base", baseCurrency: "RUB" });

      await screen.findByTestId("operation-amount");
      expect(screen.queryByTestId("operation-amount-caveat")).not.toBeInTheDocument();
    });
  });

  // Issue #75. «Цена» means two different quantities in this application: in
  // the journal it is the money one unit cost, on the positions screen a
  // bond's is the percentage of face value the exchange quotes. Both are
  // right, the word is one, and the journal used to print its number bare —
  // straight off the wire, unformatted, with nothing saying which of the two
  // it is.
  describe("the price cell", () => {
    const trade = (overrides: Partial<Operation> = {}): Operation =>
      makeOperation({
        id: "op-buy",
        type: "buy",
        instrument_id: "instr-1",
        quantity: "100",
        price: "950.00",
        currency: "RUB",
        amount_minor: -9_500_000,
        fee_minor: 9_500,
        ...overrides,
      });

    it("names the unit in the column header, where every row can read it", async () => {
      renderTable({ operations: [trade()] });

      await screen.findByTestId("operation-price");
      expect(screen.getByRole("columnheader", { name: "Кол-во × цена за единицу" }))
        .toBeInTheDocument();
    });

    it("formats the price instead of printing the wire string", async () => {
      // A thousands separator and two fraction digits, as everywhere else a
      // price is shown (formatPrice, the positions screen's quote). "1234.5"
      // used to reach the screen exactly as typed here.
      renderTable({ operations: [trade({ quantity: "1000", price: "1234.5" })] });

      const price = await screen.findByTestId("operation-price");
      expect(norm(price.textContent ?? "")).toBe("1 234,50");
      expect(price.textContent).not.toContain("1234.5");
    });

    it("shows a sub-cent price as itself rather than as a fake zero", async () => {
      // Two fraction digits would print «0,00» here: neither the price nor
      // zero, one column away from figures this program refuses to fake (#30).
      // No seeded journal row is this small yet — the demo's WeWork buy is at
      // $0,40 and the $0,0025 that made #30 is its QUOTE, which the positions
      // table draws — but a delisted share is bought at the same digits it is
      // quoted at, and this column takes whatever price was recorded.
      renderTable({
        operations: [trade({ quantity: "500", price: "0.0025", currency: "USD" })],
      });

      const price = await screen.findByTestId("operation-price");
      // Compared whole: «0,00» is a substring of the right answer too.
      expect(norm(price.textContent ?? "")).toBe("0,0025");
    });

    it("keeps a price it cannot format rather than dropping the number", async () => {
      // formatPrice answers null for anything outside a plain non-negative
      // decimal. Nothing on the wire is supposed to look like this, and if
      // something does, the row still records it: an unstyled number says more
      // than a dash, and hiding it would hide the operation's own data.
      renderTable({ operations: [trade({ price: "1e-12" })] });

      expect((await screen.findByTestId("operation-price")).textContent).toBe("1e-12");
    });

    // FINDING 4 of the caption-truth review: the tooltip lives on the
    // TableCell (`<td>`), not on the price span, precisely so the "quantity
    // ×" area — most of the cell's width for a large quantity — is a hover
    // target too. `.closest("td")` reaches it from the span the content
    // assertions elsewhere in this block already key off of.

    it("says the number is money per unit, in the operation's currency", async () => {
      renderTable({ operations: [trade()], instruments: [makeInstrument()] });

      const price = await screen.findByTestId("operation-price");
      expect(price.closest("td")).toHaveAttribute(
        "title",
        "Цена за единицу — деньги за одну штуку, в валюте операции",
      );
    });

    it("says a bond's price here is money and not the percentage of face", async () => {
      // The row the demo stand has: 100 ОФЗ at 950,00 ₽ apiece, whose position
      // is captioned «95,20 %» in the table directly above this one on the
      // same screen. One word, two quantities, both on screen at once.
      renderTable({
        operations: [trade()],
        instruments: [makeInstrument({ type: "bond", name: "ОФЗ 26238", ticker: "OFZ26238" })],
      });

      const price = await screen.findByTestId("operation-price");
      expect(price.closest("td")).toHaveAttribute(
        "title",
        "Цена за единицу — деньги за одну штуку, в валюте операции\nУ облигации это не процент от номинала: биржа котирует облигацию в процентах, и цена в таблице позиций — та самая котировка. Здесь — деньги за одну бумагу",
      );
      // And the figure is the money one, untouched — never the percentage,
      // which this screen has no face value to derive and no business deriving.
      expect(norm(price.textContent ?? "")).toBe("950,00");
    });

    it("says nothing about bonds over a share", async () => {
      renderTable({ operations: [trade()], instruments: [makeInstrument({ type: "share" })] });

      const price = await screen.findByTestId("operation-price");
      const title = price.closest("td")?.getAttribute("title") ?? "";
      expect(title).not.toContain("облигаци");
      expect(title).not.toContain("номинал");
    });

    it("still says what the number is when the catalog page has no such instrument", async () => {
      // Past the fiftieth instrument the lookup finds nothing (see
      // useInstruments), and the sentence that survives is the one true of
      // every priced row whatever the instrument turns out to be.
      renderTable({ operations: [trade({ instrument_id: "instr-off-page" })] });

      const price = await screen.findByTestId("operation-price");
      expect(price.closest("td")).toHaveAttribute(
        "title",
        "Цена за единицу — деньги за одну штуку, в валюте операции",
      );
    });

    it("puts the tooltip on the whole cell, not only the price number", async () => {
      // FINDING 4's reproduction: a large quantity makes "100 ×" most of the
      // cell's width, and before this fix the title sat only on the price
      // span — hovering the quantity prefix showed nothing. The cell itself
      // must carry the title so the whole printed area explains itself.
      renderTable({ operations: [trade()], instruments: [makeInstrument()] });

      const price = await screen.findByTestId("operation-price");
      const cell = price.closest("td");
      expect(cell?.textContent).toContain("100");
      expect(cell).toHaveAttribute(
        "title",
        "Цена за единицу — деньги за одну штуку, в валюте операции",
      );
      // The number span itself carries no title of its own any more — a
      // second, redundant title there would not be wrong, but this pins that
      // the cell is genuinely the one place the tooltip lives.
      expect(price).not.toHaveAttribute("title");
    });

    it("keeps the dash, and no claim about a price, on a row that has none", async () => {
      renderTable({ operations: [makeOperation({ type: "deposit", quantity: null, price: null })] });

      await screen.findByTestId("operation-amount");
      expect(screen.queryByTestId("operation-price")).not.toBeInTheDocument();
    });
  });

  // Issue #86. The control that reaches older entries used to appear or not
  // according to whether the page came back as long as the client asked for —
  // a comparison that is right until the server clamps the limit, which it
  // does silently at 200. At the ceiling the journal therefore looked complete
  // and the older rows had no route in the interface at all.
  describe("show more", () => {
    it("offers to load more when the server says the journal continues", async () => {
      renderTable({
        // One row and has_more — the shape the length test gets wrong: a page
        // far shorter than asked for, with the journal continuing behind it.
        operations: [makeOperation()],
        hasMore: true,
      });

      await screen.findByTestId("operation-amount");
      expect(screen.getByRole("button", { name: "Показать еще" })).toBeInTheDocument();
    });

    it("offers nothing more when the server says this is the whole journal", async () => {
      renderTable({ operations: [makeOperation()], hasMore: false });

      await screen.findByTestId("operation-amount");
      expect(screen.queryByRole("button", { name: "Показать еще" })).not.toBeInTheDocument();
    });

    it("appends the next page instead of refetching a wider window", async () => {
      // Two pages served by offset. A client that grew a single window would
      // ask for [0, 100) on the second request and get page one again — which
      // is what the ceiling makes permanent once the window reaches it.
      const asked: { limit: string | null; offset: string | null }[] = [];
      fetchMock.mockImplementation((input: RequestInfo | URL) => {
        const url = input instanceof Request ? input.url : String(input);
        const parsed = new URL(url, "http://localhost");
        let body: unknown = null;
        if (parsed.pathname.endsWith("/instruments")) {
          body = [];
        } else if (parsed.pathname.endsWith("/operations")) {
          asked.push({
            limit: parsed.searchParams.get("limit"),
            offset: parsed.searchParams.get("offset"),
          });
          const offset = Number(parsed.searchParams.get("offset") ?? "0");
          body =
            offset === 0
              ? {
                  operations: Array.from({ length: JOURNAL_PAGE_SIZE }, (_, i) =>
                    makeOperation({ id: `op-newer-${i}` }),
                  ),
                  has_more: true,
                }
              : { operations: [makeOperation({ id: "op-older" })], has_more: false };
        }
        return Promise.resolve(
          new Response(JSON.stringify(body), {
            status: 200,
            headers: { "Content-Type": "application/json" },
          }),
        );
      });
      const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
      render(
        <QueryClientProvider client={queryClient}>
          <ScreenCurrencyCountProvider>
            <OperationsTable accountId="acc-1" canDelete={false} mode="native" baseCurrency="RUB" />
          </ScreenCurrencyCountProvider>
        </QueryClientProvider>,
      );

      const more = await screen.findByRole("button", { name: "Показать еще" });
      expect(screen.getAllByTestId("operation-amount")).toHaveLength(JOURNAL_PAGE_SIZE);
      more.click();

      // Both pages on screen at once: the second is added to the first, not
      // put in its place.
      await waitFor(() =>
        expect(screen.getAllByTestId("operation-amount")).toHaveLength(JOURNAL_PAGE_SIZE + 1),
      );
      // And nothing further is offered, because the second page said so.
      expect(screen.queryByRole("button", { name: "Показать еще" })).not.toBeInTheDocument();
      // The second request starts where the first one ended. An offset that is
      // anything else either repeats rows already shown or steps over rows
      // nobody will ever see.
      expect(asked).toEqual([
        { limit: String(JOURNAL_PAGE_SIZE), offset: "0" },
        { limit: String(JOURNAL_PAGE_SIZE), offset: String(JOURNAL_PAGE_SIZE) },
      ]);
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
