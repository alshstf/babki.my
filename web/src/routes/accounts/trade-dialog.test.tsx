import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { TradeDialog } from "./trade-dialog";
import type { AccountWithBalance } from "@/api/accounts";
import type { Instrument } from "@/api/instruments";

// The API client captures globalThis.fetch once, when @/api/client is first
// imported (openapi-fetch: `fetch: baseFetch = globalThis.fetch`), so the
// double has to be in place *before* that import — hence vi.hoisted, which
// runs ahead of the import statements above.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// The operation body the dialog posted, parsed — null until it posts, which
// is itself an assertion some tests below make. Captured here rather than
// dug out of fetchMock.mock.calls afterwards because openapi-fetch calls
// fetch(new Request(...)): the body is a stream on the Request, readable only
// once and only asynchronously, so it has to be taken as the call happens.
let posted: Record<string, unknown> | null = null;

// Every request gets a FRESH Response built inside the implementation:
// a single Response object handed to mockResolvedValue would work once and
// then throw on the second read, because a body can only be consumed once.
function serve(instruments: Instrument[]) {
  fetchMock.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
    const url = input instanceof Request ? input.url : String(input);
    const path = new URL(url, "http://localhost").pathname;
    if (path.endsWith("/api/v1/instruments")) {
      return Promise.resolve(
        new Response(JSON.stringify(instruments), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      );
    }
    if (path.endsWith("/api/v1/operations")) {
      return (async () => {
        const raw =
          input instanceof Request ? await input.clone().text() : String(init?.body ?? "{}");
        posted = JSON.parse(raw);
        return new Response(JSON.stringify({ ...posted, id: "op-1", source: "manual" }), {
          status: 201,
          headers: { "Content-Type": "application/json" },
        });
      })();
    }
    return Promise.resolve(new Response("null", { status: 404 }));
  });
}

const account: AccountWithBalance = {
  id: "acc-1",
  name: "Брокерский",
  type: "brokerage",
  currency: "RUB",
  institution: "Broker Co",
  status: "active",
  created_at: "2026-01-01T00:00:00Z",
  balance: { as_of: "2026-07-20", amount_minor: 1_000_000 },
};

// The instrument from the issue: an OFZ with a 1 000,00 ₽ face value, quoted
// by every exchange and every broker as a percentage of it.
function ofz(overrides: Partial<Instrument> = {}): Instrument {
  return {
    id: "instr-bond",
    type: "bond",
    name: "ОФЗ 26238",
    ticker: "SU26238RMFS4",
    isin: "RU000A1038V6",
    figi: "",
    currency: "RUB",
    face_value_minor: 100_000,
    face_currency: "RUB",
    frozen: false,
    ...overrides,
  };
}

function share(overrides: Partial<Instrument> = {}): Instrument {
  return {
    id: "instr-share",
    type: "share",
    name: "Сбербанк",
    ticker: "SBER",
    isin: "RU0009029540",
    figi: "",
    currency: "RUB",
    frozen: false,
    ...overrides,
  };
}

// NBSP-insensitive compare: Intl.NumberFormat separates thousands with a
// non-breaking space, so "9 800,00 ₽" off the screen is not the "9 800,00 ₽"
// in this file's source (same helper as positions-table.test.tsx).
const norm = (s: string) => s.replace(/[  ]/g, " ");

// Opens the dialog with the given instrument in the catalog and selects it,
// which is the state every assertion below starts from: the dialog only knows
// an instrument is a bond, and only knows its face value, once one is picked.
async function openWith(instrument: Instrument) {
  await openCatalog([instrument]);
  await pick(instrument);
}

// The same, for the tests that need more than one instrument in the catalog
// because what they are about is switching between them.
async function openCatalog(instruments: Instrument[]) {
  serve(instruments);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TradeDialog open onOpenChange={() => {}} account={account} side="buy" />
    </QueryClientProvider>,
  );
  await screen.findByRole("button", { name: new RegExp(instruments[0].name) });
}

async function pick(instrument: Instrument) {
  fireEvent.click(await screen.findByRole("button", { name: new RegExp(instrument.name) }));
}

// Typing into a controlled input, the whole value at once. fireEvent.change is
// what @testing-library/react ships with (user-event is not a dependency of
// this project and the brief is not a reason to add one); it dispatches the
// same change event React listens for, which is the only thing these fields
// react to.
function typeInto(field: HTMLElement, value: string) {
  fireEvent.change(field, { target: { value } });
}

const percentField = () => screen.getByLabelText(/% от номинала/);
const perBondField = () => screen.getByLabelText(/Цена за одну облигацию/);
const quantityField = () => screen.getByLabelText(/Количество/);
const total = () => norm(screen.getByTestId("trade-total").textContent ?? "");

beforeEach(() => {
  fetchMock.mockReset();
  posted = null;
});

afterEach(() => {
  cleanup();
});

describe("TradeDialog: a bond is quoted in percent of face value (#77)", () => {
  // THE case the owner will type first, end to end and digit for digit: he
  // copies 98 out of his broker's terminal, buys 10 bonds of a 1 000,00 ₽
  // face, and the trade must cost 9 800,00 ₽. The defect this replaces made
  // it 980,00 ₽ — ten times too little, with nothing on screen to say so,
  // and that figure became the position's cost basis and the expense side of
  // the tax calculation.
  it("turns 98 % of a 1 000 ₽ face into 980 per bond and 9 800,00 ₽ for ten", async () => {
    await openWith(ofz());

    typeInto(percentField(), "98");
    typeInto(quantityField(), "10");

    expect(perBondField()).toHaveValue("980.00");
    expect(total()).toContain("9 800,00");
  });

  // The link runs both ways, which is the shape the owner chose over a
  // percent-only field: someone who knows a bond cost him 980 ₽ apiece types
  // that, and the field above tells him what percentage of face his broker
  // would have called it.
  it("turns 980 per bond back into 98 % and the same total", async () => {
    await openWith(ofz());

    typeInto(perBondField(), "980");
    typeInto(quantityField(), "10");

    expect(percentField()).toHaveValue("98.00");
    expect(total()).toContain("9 800,00");
  });

  // Retyping must not leave a stale partner behind. A pair that updates once
  // and then stops is worse than no pair at all: the two fields would sit
  // side by side describing two different trades, and the one that is wrong
  // is not marked.
  it("keeps following the percent field through repeated edits", async () => {
    await openWith(ofz());

    typeInto(percentField(), "98");
    expect(perBondField()).toHaveValue("980.00");

    typeInto(percentField(), "104.5");
    expect(perBondField()).toHaveValue("1045.00");

    typeInto(percentField(), "");
    expect(perBondField()).toHaveValue("");
  });

  // The correction the previous test does not make: having typed the
  // percentage, the user fixes the MONEY field instead. The percentage must
  // give up its own draft and follow — otherwise the two fields sit side by
  // side saying 98 % and 990 ₽ of a 1 000 ₽ face, which is one trade
  // described two ways, only one of them true, and neither marked. (A
  // mutation run found this: dropping the draft-clearing line left every
  // other assertion in this file green.)
  it("makes the percentage follow the money field even after the percentage was typed first", async () => {
    await openWith(ofz());

    typeInto(percentField(), "98");
    expect(perBondField()).toHaveValue("980.00");

    typeInto(perBondField(), "990");
    expect(percentField()).toHaveValue("99.00");
  });

  // A percentage that stops converting takes the money price with it. «98,5»
  // — the Russian decimal comma, which this application accepts nowhere — is
  // not a number this dialog can turn into money, and the money field is the
  // one that gets recorded: a stale 980,00 left standing beside it would keep
  // the Buy button enabled over a price the user can see is not the one he
  // just wrote. Emptied, the trade cannot be submitted until the price is a
  // price again. (The comma itself is rejected everywhere in this app and is
  // its own follow-up; this is about what the pair does when it is typed.)
  it("empties the money field when the percentage stops converting", async () => {
    await openWith(ofz());

    typeInto(percentField(), "98");
    typeInto(quantityField(), "10");
    expect(perBondField()).toHaveValue("980.00");
    expect(screen.getByRole("button", { name: "Покупка" })).toBeEnabled();

    typeInto(percentField(), "98,5");
    expect(perBondField()).toHaveValue("");
    expect(screen.getByRole("button", { name: "Покупка" })).toBeDisabled();
  });

  // The percentage a user typed belongs to the face value it was typed
  // against. Picking a different bond changes that face value, and a
  // percentage left standing beside a price it no longer describes is the
  // same pair-out-of-step defect as a field that stops updating — only harder
  // to notice, because both fields still hold plausible numbers. 980 ₽ a
  // bond is 98 % of a 1 000 ₽ face and 980 % of a 100 ₽ one; the money is
  // what the user entered and stays, the percentage is what has to move.
  it("re-answers the percentage against the new instrument's face value", async () => {
    const cheaper = ofz({
      id: "instr-bond-2",
      name: "Корпоративная облигация",
      ticker: "RU000A0JX0J2",
      face_value_minor: 10_000,
    });
    await openCatalog([ofz(), cheaper]);

    await pick(ofz());
    typeInto(percentField(), "98");
    expect(perBondField()).toHaveValue("980.00");

    await pick(cheaper);
    expect(perBondField()).toHaveValue("980.00");
    expect(percentField()).toHaveValue("980.00");
  });

  // What actually reaches the journal, and the reason the money field is the
  // one that gets sent: `price` is money per unit everywhere else in this
  // application (the journal line renders it as «quantity × price», and that
  // multiplication has to come out to the amount beside it), so a bond's row
  // carries 980, not 98. amount_minor is negative because a buy is an
  // outflow.
  it("records the money price per bond, never the percentage", async () => {
    await openWith(ofz());

    typeInto(percentField(), "98");
    typeInto(quantityField(), "10");
    fireEvent.click(screen.getByRole("button", { name: "Покупка" }));

    await waitFor(() => expect(posted).not.toBeNull());
    expect(posted?.price).toBe("980.00");
    expect(posted?.amount_minor).toBe(-980_000);
    expect(posted?.quantity).toBe("10");
  });

  // The convention is stated on the form, not left to be inferred from the
  // label alone, and it is stated the way the positions screen states it —
  // the same first clause, word for word, because two screens explaining one
  // convention differently is how a reader learns to trust neither.
  it("says what the percentage is a percentage of, and names the face value", async () => {
    await openWith(ofz());

    const hint = screen.getByTestId("trade-bond-hint");
    expect(hint.textContent).toContain(
      "Облигация котируется в процентах от номинала, а не в деньгах за штуку",
    );
    expect(norm(hint.textContent ?? "")).toContain("1 000,00 ₽");
  });
});

describe("TradeDialog: a bond whose face value cannot be used", () => {
  // Honesty over silence. Without a face value there is no conversion to
  // make, and the field that would need one is not left sitting there looking
  // ready: it is disabled and the reason names the thing that is actually
  // missing. A plausible number here would be indistinguishable from a real
  // one, and it would be a cost basis.
  it("names the missing face value and refuses to convert", async () => {
    await openWith(ofz({ face_value_minor: null, face_currency: null }));

    expect(percentField()).toBeDisabled();
    expect(screen.getByTestId("trade-bond-gap").textContent).toContain(
      "У этой облигации не записан номинал",
    );

    // The money field still works — that is the whole remedy the sentence
    // offers — and the trade it produces is the ordinary one.
    typeInto(perBondField(), "980");
    typeInto(quantityField(), "10");
    expect(total()).toContain("9 800,00");
  });

  // The face value the catalog DOES hold and the conversion still cannot use:
  // zero. Nothing upstream refuses it — instrument creation checks only that a
  // face value and its currency arrive together, and api/openapi.yaml declares
  // a plain integer with no minimum — so it reaches this dialog, where every
  // percentage of it is zero. Without a cause of its own it would fall past
  // the currency checks and come out looking convertible: an ENABLED percent
  // field over a hint reading «Номинал — 0,00 ₽», each keystroke in it
  // silently blanking the money field. That is the "enabled and doing nothing"
  // state this whole set of refusals exists to make impossible.
  it("names a face value of zero and refuses to convert", async () => {
    await openWith(ofz({ face_value_minor: 0 }));

    expect(percentField()).toBeDisabled();
    expect(screen.getByTestId("trade-bond-gap").textContent).toContain(
      "записан нулевым или отрицательным",
    );
    // And no face-value hint beside it. That hint names the number the
    // percentage is a percentage OF, and «0,00 ₽» is not such a number.
    expect(screen.queryByTestId("trade-bond-hint")).toBeNull();
  });

  // A face value with no currency at all — the state a PATCH clearing
  // face_currency leaves behind (no pairing check on update, filed separately).
  // It needs a cause of its own, and the sentence is the reason why: fall
  // through to the currency-mismatch one below and it reads «Номинал в , а
  // сделка в RUB» — a caption naming a currency that is not there, which in
  // this repository is not a typo but the defect class itself.
  it("names a face value with no currency, and never a currency that is missing", async () => {
    await openWith(ofz({ face_currency: null }));

    expect(percentField()).toBeDisabled();
    const gap = screen.getByTestId("trade-bond-gap").textContent ?? "";
    expect(gap).toContain("У номинала этой облигации не указана валюта");
    expect(gap).not.toContain("а сделка в");
  });

  // A second cause, and a different sentence for it: the face value is
  // recorded but in another currency than the trade, so the money this
  // conversion would produce is not money in the operation's currency and no
  // fx rate is at hand to make it so. Reusing the «номинал не записан»
  // sentence here would name a cause that is not the cause — the mistake
  // this project has made four times and now tests for.
  it("names a face value denominated in another currency", async () => {
    await openWith(ofz({ currency: "USD", face_currency: "RUB" }));

    expect(percentField()).toBeDisabled();
    const gap = screen.getByTestId("trade-bond-gap").textContent ?? "";
    expect(gap).toContain("Номинал в RUB, а сделка в USD");
    expect(gap).not.toContain("не записан");
  });
});

describe("TradeDialog: instruments that are not bonds", () => {
  // Nothing changes for a share: one field, the label it always had, and a
  // total that is price × quantity. A percentage field on a share would be
  // nonsense — a share has no face value to be a percentage of — and a
  // conversion applied to one would multiply the price by a number that is
  // not there.
  it("shows a single per-unit price field and no percentage", async () => {
    await openWith(share());

    expect(screen.queryByLabelText(/% от номинала/)).toBeNull();
    expect(screen.queryByTestId("trade-bond-hint")).toBeNull();
    expect(screen.getByLabelText(/Цена за единицу/)).toBeInTheDocument();

    typeInto(screen.getByLabelText(/Цена за единицу/), "305.5");
    typeInto(quantityField(), "10");
    expect(total()).toContain("3 055,00");
  });

  it("sends a share's price exactly as typed", async () => {
    await openWith(share());

    typeInto(screen.getByLabelText(/Цена за единицу/), "305.5");
    typeInto(quantityField(), "10");
    fireEvent.click(screen.getByRole("button", { name: "Покупка" }));

    await waitFor(() => expect(posted).not.toBeNull());
    expect(posted?.price).toBe("305.5");
    expect(posted?.amount_minor).toBe(-305_500);
  });
});
