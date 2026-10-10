import { describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { OperationsTable } from "./operations-table";
import {
  ScreenCurrencyCountProvider,
  useHasMultipleScreenCurrencies,
} from "@/lib/screen-currencies";
import { formatMinor } from "@/lib/money";
import { visibleText } from "@/test-utils";
import { pretendNarrow } from "@/test-narrow";
import type { Instrument } from "@/api/instruments";
import { formatDate, localToday } from "@/lib/dates";
import type { DisplayCurrencyMode } from "@/lib/display-currency";
import { JOURNAL_PAGE_SIZE, type Operation } from "@/api/operations";
import type { CostBasisRules } from "@/api/tax-residencies";

// The API client captures globalThis.fetch on first import, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// NBSP-insensitive compare, written with escapes.
const norm = (s: string) => s.replace(/[\u00A0\u202F]/g, " ");

// Serves the given endpoints by path suffix and 404s the rest, so an
// unexpected request is loud.
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
    // An old date, so "the operation's day" cannot be confused with today.
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
    // An ordinary operation has no purchase dates to miss; only transfers
    // set this.
    has_undated_lots: false,
    // True only for a transfer with a stored breakdown; set explicitly by
    // the transfer tests.
    assembled_from_lots: false,
    categorizable: false,
    counterparty: "",
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

// A country whose rules differ in two ways, so the caveat has two
// sentences and dropping either shows.
const britain: CostBasisRules = {
  country: "GB",
  method: "average",
  perimeter: "owner",
  supported: false,
  notices: ["method_mismatch", "perimeter_mismatch"],
};

// A catalog entry; only the price tests need one.
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
  canDelete = false,
  onPurchasePrice,
  accountName,
}: {
  operations: Operation[];
  // The catalog served as one page; empty by default.
  instruments?: Instrument[];
  // Whether the server says the journal continues past this page. Defaults to
  // false — every test that is not about paging is looking at a whole journal.
  hasMore?: boolean;
  mode?: DisplayCurrencyMode;
  baseCurrency?: string;
  // Omitted by every test that is not about the cost basis caveat, exactly as
  // the screen omits it while the session is still loading.
  costBasisRules?: CostBasisRules;
  // The role gate on the delete action (editor+). False by default, which is
  // what a viewer sees; only the tests about who may delete a row turn it on.
  canDelete?: boolean;
  // What «цена покупки» on an arrival from another broker opens; absent for a
  // reader who cannot write, as on the screen.
  onPurchasePrice?: (paper: { id: string; name: string; ticker: string }) => void;
  accountName?: (id: string) => string | undefined;
}) {
  serve({
    "/operations": { body: { operations, has_more: hasMore } },
    "/instruments": { body: { instruments, has_more: false } },
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <ScreenCurrencyCountProvider>
        <ToggleProbe />
        <OperationsTable
          accountId="acc-1"
          canDelete={canDelete}
          mode={mode}
          baseCurrency={baseCurrency}
          costBasisRules={costBasisRules}
          onPurchasePrice={onPurchasePrice}
          accountName={accountName}
        />
      </ScreenCurrencyCountProvider>
    </QueryClientProvider>,
  );
}

// One half of a move between two of the family's accounts says where the money
// or the shares went, or came from; a row that is no such half says nothing.
describe("OperationsTable: the other account of a move", () => {
  it("names where money went and where it came from", async () => {
    renderTable({
      operations: [
        makeOperation({ id: "op-out", type: "withdrawal", amount_minor: -100_000, transfer_group_id: "g1", counterpart_account_id: "acc-2" }),
        makeOperation({ id: "op-in", type: "deposit", amount_minor: 50_000, transfer_group_id: "g2", counterpart_account_id: "acc-3" }),
        makeOperation({ id: "op-plain", type: "deposit", amount_minor: 10_000 }),
      ],
      accountName: (id) => ({ "acc-2": "Альфа", "acc-3": "Т-Банк" })[id],
    });
    const named = await screen.findAllByTestId("operation-counterpart");
    expect(named.map((n) => n.textContent)).toEqual(["на «Альфа»", "с «Т-Банк»"]);
  });
});

// A move with a hand-typed basis shows the server's signed change of the
// family's cost on each half; a queue-priced move shows nothing.
describe("OperationsTable: a basis typed by hand", () => {
  it("says the change on a typed basis, that it matches, or nothing", async () => {
    renderTable({
      operations: [
        makeOperation({ id: "op-grew", type: "transfer_in", amount_minor: 150_000, transfer_group_id: "g1", stated_basis_change_minor: 50_000 }),
        makeOperation({ id: "op-same", type: "transfer_out", amount_minor: 100_000, transfer_group_id: "g2", stated_basis_change_minor: 0 }),
        makeOperation({ id: "op-queue", type: "transfer_out", amount_minor: 100_000, transfer_group_id: "g3", stated_basis_change_minor: null }),
      ],
    });
    const said = await screen.findAllByTestId("operation-stated-basis");
    expect(said).toHaveLength(2);
    expect(said[0].textContent).toMatch(/по семье \+500,00\s\$ к стоимости у отправителя/);
    expect(said[1].textContent).toContain("совпадает");
  });
});

// The way back to an arrival's purchases: only on shares from another
// broker, since a move between own accounts carries the source's.
describe("OperationsTable: the purchases of an arrival from another broker", () => {
  const ko = { id: "inst-ko", type: "share", name: "Coca-Cola", ticker: "KO", isin: "", figi: "", currency: "USD", frozen: false } as Instrument;
  const arrival = (overrides: Partial<Operation> = {}) =>
    makeOperation({ id: "op-arrival", type: "transfer_in", instrument_id: "inst-ko", quantity: "10", amount_minor: 0, fee_minor: 0, ...overrides });

  it("offers «цена покупки» on shares from another broker and hands over the paper", async () => {
    const opened: unknown[] = [];
    renderTable({ operations: [arrival()], instruments: [ko], onPurchasePrice: (paper) => opened.push(paper) });
    const action = await screen.findByTestId("operation-purchase-price");
    await screen.findByText("Coca-Cola");
    fireEvent.click(action);
    expect(opened).toEqual([{ id: "inst-ko", name: "Coca-Cola", ticker: "KO" }]);
  });

  it("does not offer it on a move between own accounts", async () => {
    renderTable({ operations: [arrival({ transfer_group_id: "grp-1" })], instruments: [ko], onPurchasePrice: () => {} });
    await screen.findByText("Coca-Cola");
    expect(screen.queryByTestId("operation-purchase-price")).not.toBeInTheDocument();
  });

  it("does not offer it to a reader who cannot write", async () => {
    renderTable({ operations: [arrival()], instruments: [ko] });
    await screen.findByText("Coca-Cola");
    expect(screen.queryByTestId("operation-purchase-price")).not.toBeInTheDocument();
  });
});

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

  it("prints both converted figures of a row in the currency that row's in_base carries, not the session's", async () => {
    // #106: after a base-currency change the session updates at once while
    // the journal still holds old-currency figures; amount and fee each keep
    // their own block's sign.
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
      mode: "base",
      baseCurrency: "EUR",
    });

    const amount = await screen.findByTestId("operation-amount");
    expect(norm(amount.textContent ?? "")).toBe(norm(formatMinor(655_000, "RUB")));
    expect(amount.textContent).not.toContain("€");
    const fee = screen.getByTestId("operation-fee");
    expect(norm(fee.textContent ?? "")).toBe(norm(formatMinor(32_750, "RUB")));
    expect(fee.textContent).not.toContain("€");
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
      // The rate of the operation's day, not "current".
      expect(amount).toHaveAttribute(
        "title",
        "Пересчитано по курсу на дату операции — 14.03.2019",
      );
      expect(screen.getByTestId("operation-fee")).toHaveAttribute(
        "title",
        "Пересчитано по курсу на дату операции — 14.03.2019",
      );
      // The rate date lives in the tooltip, not the cell's text.
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
              // 2019-03-14 has no rate; the nearest earlier is used, so "on the
              // operation's date" would be false.
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

    // A transfer assembled from two earlier purchases: rate_on is the newest
    // purchase, so comparing it with occurred_on gives a false sentence either
    // way; assembled_from_lots is checked first.
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
              // 118 000,00 ₽ from two purchase-day rates, never 149 150,00 ₽.
              amount_minor: 11_800_000,
              fee_minor: 0,
              currency: "RUB",
              // The newest purchase date, not the transfer's (which has a rate).
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
      // rate_on equals occurred_on here, yet the sum is still struck at several
      // rates: the flag decides, not the dates.
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
      // Every piece and the transfer on one day (CheckTransferLots allows buying
      // and moving the same day): the figure was struck at that day's rate, and
      // the rule-naming sentence stays true.
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
      // A newest purchase on a Sunday is valued at Friday's rate; the
      // sentence is about the purchase, so it names dated_on, not rate_on (#80).
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
      // An impossible payload (dated_on is always occurred_on on such rows) to
      // prove which field is compared: dated_on, the valuation day. The
      // resulting wording is not claimed true of it.
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
      // The fee cell carries the same per-cause sentence as the amount: in_base
      // is whole or nothing.
      expect(screen.getByTestId("operation-fee-not-converted")).toHaveAttribute(
        "title",
        "Нет курса на дату операции, а сумма считается по курсу того дня. Если курс появится при обновлении курсов, операция посчитается сама. Поэтому пока числа этой строки показаны в валюте операции",
      );
      // No conversion happened, so no rate date may be claimed.
      expect(amount).not.toHaveAttribute("title");
    });

    it("blames the missing purchase dates, not a missing rate, on a transfer that has none", async () => {
      // An undated transfer: its own date usually has a rate, so «нет курса на
      // дату операции» would be false and promise a figure that never comes.
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
      // Unreachable from the server, but a wording handed no date must not
      // end mid-sentence; the caller decides, since only it knows whether its
      // sentence needs one.
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

  // #79, #80: the server names the term it stopped on (Operation.in_base_gap)
  // and the table says that, as the positions screen does.
  describe("the unconverted caption", () => {
    // The general fallback, spelled out like every caption here. Its positions
    // twin (CAPTION.general) differs only in «операция»/«позиция» and the native
    // currency named; both are on one page.
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

    // Unmounts any previous render so one table is in the document.
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
      // #79: a basis from purchases on other days; the transfer's date has a
      // rate that may not value them.
      const title = await captionFor({
        type: "transfer_in",
        occurred_on: "2026-07-20",
        assembled_from_lots: true,
        in_base_gap: "no_rate_lot_date",
      });

      expect(title).toBe(
        "Сумма этой строки — стоимость покупок, и каждая её часть считается по курсу на день своей покупки. Нет курса на день одной из этих покупок. Если курс появится при обновлении курсов, операция посчитается сама. Поэтому пока числа этой строки показаны в валюте операции",
      );
      // Not «в другие дни»: a piece may be bought on the transfer day; the
      // sentence names the rule.
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
      // Asserted as a difference: a missing rate can close, an unrecorded
      // purchase date never does.
      const undated = await captionFor({
        has_undated_lots: true,
        in_base_gap: "undated_lot",
      });
      expect(undated).toContain("уже неоткуда");
      expect(undated.toLowerCase()).not.toContain("курс появится");

      // The same conditional «Если» as the positions screen (#105).
      for (const gap of ["no_rate_operation_date", "no_rate_lot_date"] as const) {
        const temporary = await captionFor({ in_base_gap: gap });
        expect(temporary).toContain("Если курс появится при обновлении курсов");
        // The bare promise, in the exact shape it had.
        expect(temporary).not.toContain("Курс появится при обновлении курсов");
        expect(temporary).not.toContain("уже неоткуда");
      }
    });

    it("takes the cause from the gap the server published, not from the flag beside it", async () => {
      // The caption reads in_base_gap only: a fully dated parcel whose first
      // piece lacks a rate shows the flag and the gap disagreeing.
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
      // A newer server's unknown value still gets a true, vague sentence.
      const title = await captionFor({
        in_base_gap: "no_rate_next_tuesday" as Operation["in_base_gap"],
      });

      expect(title).toBe(GENERAL_CAPTION);
      // Vague on purpose: an unnameable cause may be about any day at all, so
      // the fallback names none.
      expect(title).not.toContain("дату операции");
      expect(title).not.toContain("покуп");
      // #105: the fallback names no rate, in any phrasing.
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

  // #61: the cost-basis caveat hangs only on transferred parcels'
  // amounts, not as a banner over the table.
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
      // Nothing in the visible text: a tooltip plus a screen-reader copy (#31;
      // see visibleText).
      expect(norm(visibleText(screen.getByTestId("operation-amount")))).toBe(
        norm(formatMinor(11_800_000, "RUB")),
      );
    });

    it("hangs it on the departing leg exactly as on the arriving one", async () => {
      // Both legs carry the flag; the departing one too.
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
      // No caveat over a deposit or dividend: no queue picked them.
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
      // The fee is never a cost basis (this row has one only so the cell
      // exists).
      expect(screen.queryByTestId("operation-fee-caveat")).not.toBeInTheDocument();
    });

    it("asks the server which rows publish a cost basis instead of keeping a list of types", async () => {
      // The flag comes from a stored breakdown, not the type, so an unknown type
      // carries the caveat too.
      renderTable({
        operations: [assembledTransfer({ id: "op-conversion", type: "conversion" })],
        mode: "base",
        baseCurrency: "RUB",
        costBasisRules: britain,
      });

      expect(await screen.findByTestId("operation-amount-caveat")).toBeInTheDocument();
    });

    it("does not credit a queue rule with a figure no queue produced (#81)", async () => {
      // A hand-typed basis (no breakdown) carries no caveat: no queue chose it.
      // has_undated_lots is true here too, which is why it no longer decides.
      renderTable({
        operations: [
          makeOperation({
            id: "op-transfer-undated",
            type: "transfer_out",
            currency: "USD",
            in_base: null,
            has_undated_lots: true,
            assembled_from_lots: false,
            in_base_gap: "undated_lot",
          }),
        ],
        mode: "base",
        baseCurrency: "RUB",
        costBasisRules: britain,
      });

      // The not-converted indicator is separate and unchanged; awaited so the
      // caveat's absence is checked on a rendered row.
      expect(await screen.findByTestId("operation-amount-not-converted")).toHaveAttribute(
        "title",
        "Не записано, когда куплена эта партия или часть её, а её стоимость считается по курсам на дни покупок. Восстановить эти даты уже неоткуда: в базовой валюте эта операция сама не посчитается. Поэтому числа этой строки показаны в валюте операции",
      );
      expect(screen.queryByTestId("operation-amount-caveat")).not.toBeInTheDocument();
    });

    it("keeps it on a breakdown that merely contains a dateless piece", async () => {
      // A breakdown with one undated piece: both flags true, and the caveat is
      // true of it.
      renderTable({
        operations: [
          makeOperation({
            id: "op-transfer-mixed",
            type: "transfer_in",
            currency: "USD",
            in_base: null,
            has_undated_lots: true,
            assembled_from_lots: true,
            in_base_gap: "undated_lot",
          }),
        ],
        mode: "base",
        baseCurrency: "RUB",
        costBasisRules: britain,
      });

      const title = (await screen.findByTestId("operation-amount-caveat")).getAttribute("title");
      expect(title).toContain("правило очереди");
      expect(title).toContain("не самая ранняя покупка");
    });

    it("still qualifies a parcel that has a full breakdown but is already in the base currency (#67)", async () => {
      // #67: a RUB transfer in a RUB space has no in_base, yet its amount is a
      // cost basis and carries the caveat.
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

  // #75: «Цена» here is money per unit; on the positions screen a bond's is
  // a percentage of face.
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
      // Formatted like every price: separators and two decimals.
      renderTable({ operations: [trade({ quantity: "1000", price: "1234.5" })] });

      const price = await screen.findByTestId("operation-price");
      expect(norm(price.textContent ?? "")).toBe("1 234,50 ₽");
      expect(price.textContent).not.toContain("1234.5");
    });

    // #114: the price names its currency, since the amount and fee beside it
    // convert and it does not. Four cases.
    it("names the currency the price is in, so it is not read off the neighbours", async () => {
      // 1: same currency, nothing converts; the sign is shown anyway.
      renderTable({ operations: [trade()], baseCurrency: "RUB", mode: "native" });

      const price = await screen.findByTestId("operation-price");
      expect(norm(price.textContent ?? "")).toBe("950,00 ₽");
    });

    it("keeps the price in the operation's currency while the amount beside it is converted", async () => {
      // 2: base mode, a converted dollar buy: amount in roubles, price in
      // dollars.
      renderTable({
        operations: [
          trade({
            currency: "USD",
            amount_minor: -95_000_00,
            fee_minor: 95_00,
            in_base: inBase({
              amount_minor: -8_550_000_00,
              fee_minor: 8_550_00,
              currency: "RUB",
              rate_on: "2019-03-14",
              dated_on: "2019-03-14",
            }),
          }),
        ],
        baseCurrency: "RUB",
        mode: "base",
      });

      const price = await screen.findByTestId("operation-price");
      expect(norm(price.textContent ?? "")).toBe("950,00 $");
      // The amount really is in the other currency — otherwise this test would
      // pass with the price labelled from the row's converted figure too.
      expect(norm(screen.getByTestId("operation-amount").textContent ?? "")).toContain("₽");
    });

    it("names the operation's currency even where the row could not be converted at all", async () => {
      // 3: base mode, no in_base: the price says its currency on its own.
      renderTable({
        operations: [
          trade({ currency: "USD", amount_minor: -95_000_00, in_base: null, in_base_gap: "no_rate_operation_date" }),
        ],
        baseCurrency: "RUB",
        mode: "base",
      });

      const price = await screen.findByTestId("operation-price");
      expect(norm(price.textContent ?? "")).toBe("950,00 $");
      expect(screen.getByTestId("operation-amount-not-converted")).toBeInTheDocument();
    });

    it("says the same currency in both display modes", async () => {
      // 4: the sign is unconditional, so it does not change with the toggle
      // (as the positions quote, #76).
      const operation = trade({
        currency: "USD",
        amount_minor: -95_000_00,
        in_base: inBase({
          amount_minor: -8_550_000_00,
          fee_minor: 0,
          currency: "RUB",
          rate_on: "2019-03-14",
          dated_on: "2019-03-14",
        }),
      });

      renderTable({ operations: [operation], baseCurrency: "RUB", mode: "native" });
      expect(norm((await screen.findByTestId("operation-price")).textContent ?? "")).toBe("950,00 $");
      cleanup();

      renderTable({ operations: [operation], baseCurrency: "RUB", mode: "base" });
      expect(norm((await screen.findByTestId("operation-price")).textContent ?? "")).toBe("950,00 $");
    });

    it("shows a sub-cent price as itself rather than as a fake zero", async () => {
      // A sub-cent price keeps its digits (#30).
      renderTable({
        operations: [trade({ quantity: "500", price: "0.0025", currency: "USD" })],
      });

      const price = await screen.findByTestId("operation-price");
      // Compared whole: «0,00» is a substring of the right answer.
      expect(norm(price.textContent ?? "")).toBe("0,0025 $");
    });

    it("keeps a price it cannot format rather than dropping the number", async () => {
      // Not a plain decimal: shown raw rather than hidden.
      renderTable({ operations: [trade({ price: "1e-12" })] });

      const price = await screen.findByTestId("operation-price");
      expect(price.textContent).toBe("1e-12");
      // Without a currency: the string was never read as a price.
      expect(price.textContent).not.toContain("₽");
    });

    // The tooltip is on the cell, so "quantity ×" is a hover target too.

    it("says the number is money per unit, in the operation's currency", async () => {
      renderTable({ operations: [trade()], instruments: [makeInstrument()] });

      const price = await screen.findByTestId("operation-price");
      expect(price.closest("td")).toHaveAttribute(
        "title",
        "Цена за единицу — деньги за одну штуку, в валюте операции",
      );
    });

    it("says a bond's price here is money and not the percentage of face", async () => {
      // The demo row: 100 ОФЗ at 950,00 ₽, captioned «95,20 %» in the table
      // above.
      renderTable({
        operations: [trade()],
        instruments: [makeInstrument({ type: "bond", name: "ОФЗ 26238", ticker: "SU26238RMFS4" })],
      });

      const price = await screen.findByTestId("operation-price");
      expect(price.closest("td")).toHaveAttribute(
        "title",
        "Цена за единицу — деньги за одну штуку, в валюте операции\nУ облигации это не процент от номинала: биржа котирует облигацию в процентах, и цена в таблице позиций — та самая котировка. Здесь — деньги за одну бумагу",
      );
      // The money figure, never a derived percentage, with its currency.
      expect(norm(price.textContent ?? "")).toBe("950,00 ₽");
    });

    it("says nothing about bonds over a share", async () => {
      renderTable({ operations: [trade()], instruments: [makeInstrument({ type: "share" })] });

      const price = await screen.findByTestId("operation-price");
      const title = price.closest("td")?.getAttribute("title") ?? "";
      expect(title).not.toContain("облигаци");
      expect(title).not.toContain("номинал");
    });

    it("still says what the number is while the catalog has not answered for the row", async () => {
      // Before the catalog loads, the general sentence, true of every row.
      renderTable({ operations: [trade({ instrument_id: "instr-off-page" })] });

      const price = await screen.findByTestId("operation-price");
      expect(price.closest("td")).toHaveAttribute(
        "title",
        "Цена за единицу — деньги за одну штуку, в валюте операции",
      );
    });

    it("puts the tooltip on the whole cell, not only the price number", async () => {
      // The title is on the whole cell, quantity included.
      renderTable({ operations: [trade()], instruments: [makeInstrument()] });

      const price = await screen.findByTestId("operation-price");
      const cell = price.closest("td");
      expect(cell?.textContent).toContain("100");
      expect(cell).toHaveAttribute(
        "title",
        "Цена за единицу — деньги за одну штуку, в валюте операции",
      );
      // And only there.
      expect(price).not.toHaveAttribute("title");
    });

    it("keeps the dash, and no claim about a price, on a row that has none", async () => {
      renderTable({ operations: [makeOperation({ type: "deposit", quantity: null, price: null })] });

      await screen.findByTestId("operation-amount");
      expect(screen.queryByTestId("operation-price")).not.toBeInTheDocument();
    });
  });

  // #86: "show more" follows has_more, not the page length.
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
      // Two pages by offset; a growing window would refetch page one.
      const asked: { limit: string | null; offset: string | null }[] = [];
      fetchMock.mockImplementation((input: RequestInfo | URL) => {
        const url = input instanceof Request ? input.url : String(input);
        const parsed = new URL(url, "http://localhost");
        let body: unknown = null;
        if (parsed.pathname.endsWith("/instruments")) {
          body = { instruments: [], has_more: false };
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
      // The second request starts where the first ended.
      expect(asked).toEqual([
        { limit: String(JOURNAL_PAGE_SIZE), offset: "0" },
        { limit: String(JOURNAL_PAGE_SIZE), offset: String(JOURNAL_PAGE_SIZE) },
      ]);
    });
  });

  describe("screen currency reporting", () => {
    it("makes the toggle appear when only the journal is multi-currency", async () => {
      // A USD operation on a RUB account in a RUB space: the journal's report
      // is what brings up the toggle.
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

  // Rows another writer owns cannot be deleted (Service.Delete); the
  // journal does not offer it.
  describe("rows an import wrote", () => {
    it("says where an imported row came from and offers no way to delete it", async () => {
      renderTable({
        canDelete: true,
        operations: [
          makeOperation({ id: "op-imported", source: "tinvest" }),
          makeOperation({ id: "op-manual", source: "manual" }),
        ],
      });

      expect(await screen.findByText("Т-Инвестиции")).toBeInTheDocument();
      // One delete button for two rows: the hand-entered one.
      expect(screen.getAllByRole("button", { name: "Удалить" })).toHaveLength(1);
    });

    it("leaves a hand-entered row unlabelled and deletable", async () => {
      renderTable({
        canDelete: true,
        operations: [makeOperation({ source: "manual" })],
      });

      expect(await screen.findByRole("button", { name: "Удалить" })).toBeInTheDocument();
      expect(screen.queryByText("Т-Инвестиции")).not.toBeInTheDocument();
      expect(screen.queryByText("Загружено извне")).not.toBeInTheDocument();
    });

    // A registry row is neither Т-Инвестиции nor deletable: only rows a person
    // owns (manual, csv) are.
    it("does not put another writer's rows under the T-Invest name, nor make them deletable", async () => {
      renderTable({
        canDelete: true,
        operations: [makeOperation({ source: "registry" })],
      });

      expect(await screen.findByText("Загружено извне")).toBeInTheDocument();
      expect(screen.queryByText("Т-Инвестиции")).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: "Удалить" })).not.toBeInTheDocument();
    });

    // Every other writer's row is undeletable; only T-Invest rows are
    // promised to come back.
    it("promises a rebuild only where something rebuilds", async () => {
      renderTable({ canDelete: true, operations: [makeOperation({ source: "tinvest" })] });

      expect(await screen.findByText("Т-Инвестиции")).toHaveAttribute(
        "title",
        "Эту операцию записал импорт из Т-Инвестиций, а не человек: удалить её здесь нельзя — при следующей загрузке она соберётся заново",
      );
    });

    it("tells another writer's row only that it cannot be deleted", async () => {
      renderTable({ canDelete: true, operations: [makeOperation({ source: "registry" })] });

      expect(await screen.findByText("Загружено извне")).toHaveAttribute(
        "title",
        "Эту операцию записал импорт, а не человек: удалить её здесь нельзя",
      );
    });

    // A row loaded from the person's own table is theirs: it says where it came
    // from and can be deleted like a hand entry.
    it("marks a row from a table and lets it be deleted", async () => {
      renderTable({ canDelete: true, operations: [makeOperation({ source: "csv" })] });

      expect(await screen.findByText("из таблицы")).toBeInTheDocument();
      expect(screen.queryByText("Загружено извне")).not.toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Удалить" })).toBeInTheDocument();
    });

    it("keeps the source visible to a viewer, who has no delete column at all", async () => {
      renderTable({
        canDelete: false,
        operations: [makeOperation({ source: "tinvest" })],
      });

      expect(await screen.findByText("Т-Инвестиции")).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: "Удалить" })).not.toBeInTheDocument();
    });
  });
});

// #104: the instrument column is a lookup, so it walks the whole catalog;
// a broker import brings a hundred papers.
describe("OperationsTable — an instrument the first page of the catalog does not hold", () => {
  // The catalog by offset; `asked` records the offsets walked.
  function serveCatalogPages(pages: { instruments: Instrument[]; has_more: boolean }[]) {
    const asked: string[] = [];
    fetchMock.mockImplementation((input: RequestInfo | URL) => {
      const url = input instanceof Request ? input.url : String(input);
      const parsed = new URL(url, "http://localhost");
      let body: unknown = null;
      if (parsed.pathname.endsWith("/instruments")) {
        const offset = parsed.searchParams.get("offset") ?? "0";
        asked.push(offset);
        body = pages[Number(offset)] ?? { instruments: [], has_more: false };
      } else if (parsed.pathname.endsWith("/operations")) {
        body = {
          operations: [makeOperation({ type: "buy", instrument_id: "instr-late", quantity: "1" })],
          has_more: false,
        };
      }
      return Promise.resolve(
        new Response(JSON.stringify(body), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      );
    });
    return asked;
  }

  function renderJournal() {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <QueryClientProvider client={queryClient}>
        <ScreenCurrencyCountProvider>
          <OperationsTable accountId="acc-1" canDelete={false} mode="native" baseCurrency="RUB" />
        </ScreenCurrencyCountProvider>
      </QueryClientProvider>,
    );
  }

  it("names it, instead of printing the tail of its id", async () => {
    // The instrument is on page two.
    const asked = serveCatalogPages([
      { instruments: [makeInstrument({ id: "instr-early", name: "Алроса" })], has_more: true },
      { instruments: [makeInstrument({ id: "instr-late", name: "Ветер" })], has_more: false },
    ]);
    renderJournal();

    expect(await screen.findByText("Ветер")).toBeInTheDocument();
    expect(screen.queryByText(/^#/)).not.toBeInTheDocument();
    // Both pages were asked for, the second at the offset where the first
    // ended — and nothing was asked for after the server said there was no more.
    await waitFor(() => expect(asked).toEqual(["0", "1"]));
  });

  it("stops asking when the server says the catalog is whole", async () => {
    const asked = serveCatalogPages([
      { instruments: [makeInstrument({ id: "instr-late", name: "Ветер" })], has_more: false },
    ]);
    renderJournal();

    expect(await screen.findByText("Ветер")).toBeInTheDocument();
    // One request, not a walk that keeps going against an endpoint answering
    // empty pages for ever.
    await waitFor(() => expect(asked).toEqual(["0"]));
  });

  it("does not hammer a catalog page that keeps failing", async () => {
    // A failed page keeps «есть ещё» and clears «идёт загрузка»; a walk
    // reading only those would retry forever.
    const asked: string[] = [];
    fetchMock.mockImplementation((input: RequestInfo | URL) => {
      const url = input instanceof Request ? input.url : String(input);
      const parsed = new URL(url, "http://localhost");
      if (parsed.pathname.endsWith("/categories")) {
        return Promise.resolve(
          new Response("[]", { status: 200, headers: { "Content-Type": "application/json" } }),
        );
      }
      if (parsed.pathname.endsWith("/instruments")) {
        const offset = parsed.searchParams.get("offset") ?? "0";
        asked.push(offset);
        if (offset !== "0") {
          return Promise.resolve(
            new Response(JSON.stringify({ error: "internal error" }), {
              status: 500,
              headers: { "Content-Type": "application/json" },
            }),
          );
        }
        return Promise.resolve(
          new Response(
            JSON.stringify({
              instruments: [makeInstrument({ id: "instr-early", name: "Алроса" })],
              has_more: true,
            }),
            { status: 200, headers: { "Content-Type": "application/json" } },
          ),
        );
      }
      return Promise.resolve(
        new Response(
          JSON.stringify({
            operations: [makeOperation({ type: "buy", instrument_id: "instr-early", quantity: "1" })],
            has_more: false,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      );
    });
    renderJournal();

    // What did arrive is drawn: the failure costs the names behind it, not the
    // ones already in hand.
    expect(await screen.findByText("Алроса")).toBeInTheDocument();
    await waitFor(() => expect(asked).toEqual(["0", "1"]));

    // And it stays two. Long enough that a loop firing on every render would
    // have run many times over.
    await new Promise((resolve) => setTimeout(resolve, 200));
    expect(asked).toEqual(["0", "1"]);
  });
});

// Примечание: the broker's own words for which bond was redeemed.
describe("примечание операции", () => {
  it("выводится под инструментом", async () => {
    renderTable({
      operations: [
        makeOperation({ type: "redemption", note: "Погашение Инарктика 001Р-01" }),
      ],
    });
    expect(await screen.findByTestId("operation-note")).toHaveTextContent(
      "Погашение Инарктика 001Р-01",
    );
    // И тип назван своим словом, а не «продажей».
    expect(screen.getByText("погашение")).toBeInTheDocument();
  });

  it("не оставляет пустой строки, когда его нет", async () => {
    renderTable({ operations: [makeOperation({ note: "" })] });
    // Ждём саму строку операции: иначе «ничего не отрисовано» прошло бы за
    // «примечания нет» — тест, зеленеющий по неверной причине.
    expect(await screen.findByText("пополнение")).toBeInTheDocument();
    expect(screen.queryByTestId("operation-note")).toBeNull();
  });
});

// Р-14: a foreign dividend carries the tax withheld abroad under its type; any
// other row carries nothing.
describe("OperationsTable — the tax withheld abroad", () => {
  it("shows it under a dividend that has it, and nowhere else", async () => {
    renderTable({
      operations: [
        makeOperation({
          id: "op-div",
          type: "dividend",
          amount_minor: 14,
          currency: "USD",
          withheld_abroad: {
            state: "unknown",
            unknown_reason: "no_calendar",
            currency: "USD",
            received_minor: 14,
            broker_tax: [],
          },
        }),
        makeOperation({ id: "op-dep" }),
      ],
    });

    expect(await screen.findAllByTestId("operation-withheld")).toHaveLength(1);
    expect(screen.getByTestId("operation-withheld-unknown")).toHaveAttribute(
      "title",
      "Календаря дивидендов по этой бумаге нет — оценить не из чего",
    );
  });
});

// On a phone the journal keeps date, kind and amount side by side and folds
// the paper, its count and price, and the fee under the kind: once each.
describe("OperationsTable on a phone", () => {
  it("folds the paper, the count at a price and the fee under the kind", async () => {
    pretendNarrow();
    renderTable({
      operations: [
        makeOperation({
          type: "buy",
          instrument_id: "instr-1",
          quantity: "10",
          price: "250.5",
          currency: "RUB",
          amount_minor: -2_505_00,
          fee_minor: 3_00,
          note: "Покупка",
        }),
      ],
      instruments: [makeInstrument()],
    });

    await screen.findByText("Сбербанк");
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Дата и тип", "Сумма"]);
    expect(screen.getAllByTestId("operation-note")).toHaveLength(1);
    expect(norm(screen.getByTestId("operation-price").textContent ?? "")).toBe(norm("250,50 ₽"));
    expect(norm(screen.getByTestId("operation-fee").textContent ?? "")).toBe(norm(formatMinor(3_00, "RUB")));
  });
});
