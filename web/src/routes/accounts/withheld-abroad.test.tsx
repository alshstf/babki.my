import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import type { Operation } from "@/api/operations";
import { WithheldAbroadNote } from "./withheld-abroad";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const sent = vi.hoisted(() => [] as { method: string; url: string; body: unknown }[]);
vi.hoisted(() => {
  globalThis.fetch = (async (input: Request) => {
    const text = await input.clone().text();
    sent.push({ method: input.method, url: input.url, body: text ? JSON.parse(text) : null });
    return new Response(null, { status: 204 });
  }) as unknown as typeof fetch;
});

type Withheld = NonNullable<Operation["withheld_abroad"]>;

const norm = (s: string | null | undefined) => (s ?? "").replace(/[  ]/g, " ");

const estimated: Withheld = {
  state: "estimated",
  currency: "USD",
  received_minor: 14,
  gross_minor: 20,
  tax_minor: 6,
  rate_percent: "30.0",
  per_share: "0.04",
  shares: "5",
  record_date: "2021-09-02",
  tax_in_base: { currency: "RUB", amount_minor: 438 },
  broker_tax: [],
};

function dividend(withheld: Withheld): Operation {
  return {
    id: "op-div",
    account_id: "acc-1",
    instrument_id: "instr-nvda",
    type: "dividend",
    occurred_on: "2021-09-29",
    amount_minor: 14,
    currency: "USD",
    fee_minor: 0,
    note: "",
    source: "tinvest",
    created_at: "2021-09-29T10:00:00Z",
    has_undated_lots: false,
    assembled_from_lots: false,
    withheld_abroad: withheld,
  } as Operation;
}

function show(withheld: Withheld, editable = false) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <WithheldAbroadNote operation={dividend(withheld)} editable={editable} />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  sent.length = 0;
});

describe("WithheldAbroadNote", () => {
  it("shows the estimate as one, with its arithmetic in the tooltip", () => {
    show(estimated);

    const line = screen.getByTestId("operation-withheld-estimate");
    expect(norm(line.textContent)).toBe("Налог у источника ≈ 0,06 $ (30,0 %), ≈ 4,38 ₽ по курсу дня выплаты");
    expect(norm(line.getAttribute("title"))).toBe(
      "Оценка по календарю дивидендов: объявлено 0,04 $ на акцию × 5 шт. на 02.09.2021 = 0,20 $ до удержания, пришло 0,14 $. Это не документ — сверьте со справкой брокера",
    );
  });

  it("says the sum is unknown and why, never a guess", () => {
    show({
      ...estimated,
      state: "unknown",
      unknown_reason: "another_currency",
      gross_minor: null,
      tax_minor: null,
      rate_percent: null,
      tax_in_base: null,
      currency: "RUB",
      broker_tax: [{ currency: "RUB", amount_minor: -1600 }],
    });

    const line = screen.getByTestId("operation-withheld-unknown");
    expect(line.textContent).toBe("Налог у источника: сумма неизвестна");
    expect(line.getAttribute("title")).toContain("в другой валюте");
    expect(screen.queryByTestId("operation-withheld-estimate")).toBeNull();
    expect(norm(screen.getByTestId("operation-withheld-broker").textContent)).toBe("Налог строкой брокера: -16,00 ₽");
  });

  it("leaves a payment taxed by the broker's own row to that row", () => {
    show({
      ...estimated,
      state: "reported",
      gross_minor: null,
      tax_minor: null,
      rate_percent: null,
      tax_in_base: null,
      broker_tax: [{ currency: "USD", amount_minor: -2 }],
    });

    expect(screen.queryByTestId("operation-withheld-estimate")).toBeNull();
    expect(screen.queryByTestId("operation-withheld-unknown")).toBeNull();
    expect(norm(screen.getByTestId("operation-withheld-broker").textContent)).toBe("Налог строкой брокера: -0,02 $");
  });

  it("shows a stated figure without the estimate's ≈", () => {
    show({ ...estimated, state: "stated", tax_minor: 2, gross_minor: 16, rate_percent: "12.5", tax_in_base: null });

    expect(norm(screen.getByTestId("operation-withheld-stated").textContent)).toBe(
      "Налог у источника: 0,02 $ (12,5 %) — указано вручную",
    );
  });

  it("offers no correction to a reader who may not edit", () => {
    show(estimated);

    expect(screen.queryByTestId("operation-withheld-edit")).toBeNull();
  });
});

describe("WithheldAbroadNote — stating the figure", () => {
  it("sends the tax typed, and shows the rate it implies against what arrived", async () => {
    show({ ...estimated, state: "unknown", unknown_reason: "no_calendar", gross_minor: null, tax_minor: null }, true);
    fireEvent.click(screen.getByTestId("operation-withheld-edit"));

    fireEvent.change(screen.getByLabelText(/Удержано за рубежом/), { target: { value: "0,02" } });
    expect(norm(screen.getByText(/от суммы до удержания/).textContent)).toBe(
      "Это 12,5 % от суммы до удержания (0,16 $)",
    );
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));

    await vi.waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0].method).toBe("PUT");
    expect(sent[0].url).toContain("/api/v1/operations/op-div/withheld-abroad");
    expect(sent[0].body).toEqual({ tax_minor: 2 });
  });

  it("brings the estimate back by clearing a stated figure", async () => {
    show({ ...estimated, state: "stated", tax_minor: 2, gross_minor: 16, rate_percent: "12.5" }, true);
    fireEvent.click(screen.getByTestId("operation-withheld-edit"));
    fireEvent.click(screen.getByRole("button", { name: "Вернуть оценку" }));

    await vi.waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0].method).toBe("DELETE");
  });

  it("does not offer to bring back an estimate nobody replaced", () => {
    show(estimated, true);
    fireEvent.click(screen.getByTestId("operation-withheld-edit"));

    expect(screen.queryByRole("button", { name: "Вернуть оценку" })).toBeNull();
    expect(screen.getByLabelText(/Удержано за рубежом/)).toHaveValue("0.06");
  });
});
