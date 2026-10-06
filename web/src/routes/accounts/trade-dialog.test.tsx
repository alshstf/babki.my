import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { TradeDialog } from "./trade-dialog";
import type { AccountWithBalance } from "@/api/accounts";
import type { Instrument } from "@/api/instruments";

// The API client captures globalThis.fetch on first import, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// The posted operation body, parsed; null until it posts. Taken as the
// call happens: openapi-fetch passes a Request whose body is a one-shot
// stream.
let posted: Record<string, unknown> | null = null;

// What POST /api/v1/operations answers; the refusal tests set the status,
// the one thing the dialog may branch on (see isConflict).
let operationStatus = 201;

// A fresh Response per request: a body can be read only once.
function serve(instruments: Instrument[]) {
  fetchMock.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
    const url = input instanceof Request ? input.url : String(input);
    const path = new URL(url, "http://localhost").pathname;
    if (path.endsWith("/api/v1/instruments")) {
      return Promise.resolve(
        // The catalog envelope (#104); this fixture is the whole catalog.
        new Response(JSON.stringify({ instruments, has_more: false }), {
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
        if (operationStatus !== 201) {
          // The server's English log prose, so a test asserting the screen does not
          // repeat it has something to fail on.
          return new Response(
            JSON.stringify({
              error:
                "journal would become inconsistent: buy 2026-07-10: currency RUB does not match the USD this position's cost is in",
            }),
            { status: operationStatus, headers: { "Content-Type": "application/json" } },
          );
        }
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
  valued_by_balance: false,
  trades_abroad: false,
  counted_by: "balance",
  balance: { as_of: "2026-07-20", amount_minor: 1_000_000 },
};

// An OFZ with a 1 000,00 ₽ face, quoted as a percentage of it.
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

// NBSP-insensitive compare.
const norm = (s: string) => s.replace(/[  ]/g, " ");

// Opens the dialog and picks the instrument: only then does it know a
// bond and its face.
async function openWith(instrument: Instrument) {
  await openCatalog([instrument]);
  await pick(instrument);
}

// The same with several instruments, for switching between them.
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

// Sets a controlled input's whole value with fireEvent.change, the change
// event React listens for (user-event is not a dependency).
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
  operationStatus = 201;
});

afterEach(() => {
  cleanup();
});

describe("TradeDialog: a bond is quoted in percent of face value (#77)", () => {
  // The owner's case end to end: 98 % of a 1 000,00 ₽ face, ten bonds,
  // 9 800,00 ₽ (not the ten-times-too-small 980,00 ₽).
  it("turns 98 % of a 1 000 ₽ face into 980 per bond and 9 800,00 ₽ for ten", async () => {
    await openWith(ofz());

    typeInto(percentField(), "98");
    typeInto(quantityField(), "10");

    expect(perBondField()).toHaveValue("980.00");
    expect(total()).toContain("9 800,00");
  });

  // The link runs both ways: 980 ₽ a bond shows as 98 %.
  it("turns 980 per bond back into 98 % and the same total", async () => {
    await openWith(ofz());

    typeInto(perBondField(), "980");
    typeInto(quantityField(), "10");

    expect(percentField()).toHaveValue("98.00");
    expect(total()).toContain("9 800,00");
  });

  // Retyping keeps the partner in step: two fields describing two
  // trades, the wrong one unmarked, is worse than one field.
  it("keeps following the percent field through repeated edits", async () => {
    await openWith(ofz());

    typeInto(percentField(), "98");
    expect(perBondField()).toHaveValue("980.00");

    typeInto(percentField(), "104.5");
    expect(perBondField()).toHaveValue("1045.00");

    typeInto(percentField(), "");
    expect(perBondField()).toHaveValue("");
  });

  // Having typed the percentage, the user fixes the money field: the
  // percentage drops its draft and follows.
  it("makes the percentage follow the money field even after the percentage was typed first", async () => {
    await openWith(ofz());

    typeInto(percentField(), "98");
    expect(perBondField()).toHaveValue("980.00");

    typeInto(perBondField(), "990");
    expect(percentField()).toHaveValue("99.00");
  });

  // A percentage that stops converting («98,5», the comma is accepted
  // nowhere) empties the money field, so Buy cannot submit a stale price.
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

  // A typed percentage belongs to its face value: on another bond the money
  // stays and the percentage is recomputed (980 ₽ is 980 % of a 100 ₽
  // face).
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

  // Money per bond is recorded, as `price` is money per unit everywhere. No
  // amount is sent: the server computes it, so a browser's rounding never
  // becomes a cost.
  it("records the money price per bond, never the percentage", async () => {
    await openWith(ofz());

    typeInto(percentField(), "98");
    typeInto(quantityField(), "10");
    fireEvent.click(screen.getByRole("button", { name: "Покупка" }));

    await waitFor(() => expect(posted).not.toBeNull());
    expect(posted?.price).toBe("980.00");
    expect(posted).not.toHaveProperty("amount_minor");
    expect(posted?.quantity).toBe("10");
  });

  // The convention is stated on the form with the positions screen's first
  // clause, word for word.
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
  // No face, no conversion: the field is disabled and the reason names
  // what is missing.
  it("names the missing face value and refuses to convert", async () => {
    await openWith(ofz({ face_value_minor: null, face_currency: null }));

    expect(percentField()).toBeDisabled();
    expect(screen.getByTestId("trade-bond-gap").textContent).toContain(
      "У этой облигации не записан номинал",
    );

    // The money field still works, which is the remedy offered.
    typeInto(perBondField(), "980");
    typeInto(quantityField(), "10");
    expect(total()).toContain("9 800,00");
  });

  // A zero face reaches the dialog (nothing upstream refuses it) and needs
  // its own cause; otherwise the percent field is enabled over «Номинал —
  // 0,00 ₽», blanking the money field with every keystroke.
  it("names a face value of zero and refuses to convert", async () => {
    await openWith(ofz({ face_value_minor: 0 }));

    expect(percentField()).toBeDisabled();
    expect(screen.getByTestId("trade-bond-gap").textContent).toContain(
      "записан нулевым или отрицательным",
    );
    // No face hint either: «0,00 ₽» is not what the percentage is of.
    expect(screen.queryByTestId("trade-bond-hint")).toBeNull();
  });

  // A face with no currency (#93 stopped the API producing it; the client
  // still copes). It needs its own cause: the mismatch sentence would read
  // «Номинал в , а сделка в RUB».
  it("names a face value with no currency, and never a currency that is missing", async () => {
    await openWith(ofz({ face_currency: null }));

    expect(percentField()).toBeDisabled();
    const gap = screen.getByTestId("trade-bond-gap").textContent ?? "";
    expect(gap).toContain("У номинала этой облигации не указана валюта");
    expect(gap).not.toContain("а сделка в");
  });

  // An empty string is not null, but names no currency either. The API
  // refuses to store one (checkFacePair, migration 0012); the client copes
  // as it does for null.
  it("treats an empty face currency as no currency, not as a currency named ''", async () => {
    await openWith(ofz({ face_currency: "" }));

    expect(percentField()).toBeDisabled();
    const gap = screen.getByTestId("trade-bond-gap").textContent ?? "";
    expect(gap).toContain("У номинала этой облигации не указана валюта");
    expect(gap).not.toContain("а сделка в");
  });

  // A face in another currency than the trade: the conversion would not
  // produce the operation's money, and «номинал не записан» would name the
  // wrong cause.
  it("names a face value denominated in another currency", async () => {
    await openWith(ofz({ currency: "USD", face_currency: "RUB" }));

    expect(percentField()).toBeDisabled();
    const gap = screen.getByTestId("trade-bond-gap").textContent ?? "";
    expect(gap).toContain("Номинал в RUB, а сделка в USD");
    expect(gap).not.toContain("не записан");
  });
});

// The fee carries the same bound as the total, so it needs the same two
// sentences told apart: «Проверьте комиссию» over a valid number misleads.
describe("TradeDialog: a fee too large to record", () => {
  const feeField = () => screen.getByLabelText(/Комиссия/);

  it("says the fee is too large rather than telling its author to check it", async () => {
    await openWith(share());
    typeInto(feeField(), "20000000000000");

    expect(screen.getByText(/Слишком большая сумма/)).toBeTruthy();
    expect(screen.queryByText(/Проверьте комиссию/)).toBeNull();
    expect(screen.getByRole("button", { name: "Покупка" })).toBeDisabled();
  });

  it("names the largest fee it would take, in the currency the fee is recorded in", async () => {
    await openWith(share());
    typeInto(feeField(), "10000000000000.01"); // one kopeck past the bound

    const hint = norm(screen.getByText(/Слишком большая сумма/).textContent ?? "").replace(
      /\s/g,
      " ",
    );
    expect(hint).toContain("10 000 000 000 000 ₽");
  });

  // #109.4: the operation's currency follows the instrument, not the
  // account, so the ceiling is named in the instrument's.
  it("names the ceiling in the instrument's currency, not the account's", async () => {
    await openWith(share({ currency: "USD" }));
    typeInto(feeField(), "10000000000000.01");

    const hint = norm(screen.getByText(/Слишком большая сумма/).textContent ?? "").replace(
      /\s/g,
      " ",
    );
    expect(hint).toContain("10 000 000 000 000 $");
    // The account is in rubles and says nothing about this fee.
    expect(hint).not.toContain("₽");
  });

  // No instrument, no currency: the fee field is enabled from the start, and
  // the account's currency would be the wrong sign.
  it("names no currency at all while no instrument has been picked", async () => {
    await openCatalog([share({ currency: "USD" })]);
    typeInto(feeField(), "10000000000000.01");

    const hint = norm(screen.getByText(/Слишком большая сумма/).textContent ?? "");
    expect(hint).toContain("Комиссия записывается в валюте инструмента");
    expect(hint).not.toContain("₽");
    expect(hint).not.toContain("$");
  });

  it("still tells its author to check a fee that is not a number", async () => {
    await openWith(share());
    typeInto(feeField(), "abc");

    expect(screen.getByText(/Проверьте комиссию/)).toBeTruthy();
    expect(screen.queryByText(/Слишком большая сумма/)).toBeNull();
  });

  it("takes the largest fee there is, and complains about nothing", async () => {
    await openWith(share());
    typeInto(screen.getByLabelText(/Цена за единицу/), "305.5");
    typeInto(quantityField(), "10");
    typeInto(feeField(), "10000000000000");

    expect(screen.queryByText(/Слишком большая сумма/)).toBeNull();
    expect(screen.queryByText(/Проверьте комиссию/)).toBeNull();
    expect(screen.getByRole("button", { name: "Покупка" })).toBeEnabled();
  });
});

describe("TradeDialog: instruments that are not bonds", () => {
  // A share: one field, its usual label, total = price × quantity.
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
    expect(posted).not.toHaveProperty("amount_minor");
  });
});

// #23: a 409 covers several refusals the client cannot tell apart, and the
// refused row need not be the posted one (see POST /api/v1/operations and
// TestConflictIsNotOnlyAnOversell).
describe("TradeDialog: a journal the server would not replay (#23)", () => {
  // A buy releases nothing, so "not enough securities" cannot be the
  // reason.
  async function refuseABuyWith(status: number) {
    operationStatus = status;
    await openWith(share());
    typeInto(screen.getByLabelText(/Цена за единицу/), "100");
    typeInto(quantityField(), "1");
    fireEvent.click(screen.getByRole("button", { name: "Покупка" }));
    return screen.findByRole("alert");
  }

  it("says the journal did not add up, and names no cause of its own", async () => {
    const alert = await refuseABuyWith(409);

    expect(norm(alert.textContent ?? "")).toContain(
      "Журнал счёта с этой операцией не сошёлся",
    );
    // The old sentence, checked against the whole document.
    expect(document.body.textContent).not.toContain("Недостаточно бумаг");
    // And still not the server's own English, which is a log line (#95).
    expect(document.body.textContent).not.toContain("journal would become inconsistent");
  });

  // A non-409 must not borrow the conflict's sentence.
  it("says only that something went wrong when the refusal is not a conflict", async () => {
    const alert = await refuseABuyWith(500);

    expect(alert.textContent).toContain("Что-то пошло не так");
    expect(document.body.textContent).not.toContain("Журнал счёта");
  });
});
