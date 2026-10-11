import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { ForecastCard } from "./forecast";
import type { Forecast } from "@/api/forecast";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

const norm = (s: string) => s.replace(/[  ]/g, " ");

const event = (on: string, name: string, minor: number, extra: Partial<Forecast["events"][number]> = {}) => ({
  on, name, kind: "regular" as const, account_id: "acc", amount_minor: minor, currency: "RUB", in_base_minor: minor, overdue: false, ...extra,
});

function forecast(over: Partial<Forecast> = {}): Forecast {
  return {
    base_currency: "RUB",
    days: 90,
    start_minor: 51_500_00,
    accounts_counted: 3,
    series: [
      { on: "2026-10-10", balance_minor: 6_500_00 },
      { on: "2026-10-15", balance_minor: -40_956_90 },
      { on: "2026-11-05", balance_minor: 199_043_10 },
    ],
    events: [
      event("2026-10-10", "ИП Смирнова", -45_000_00, { overdue: true }),
      event("2026-10-15", "Ипотека ВТБ", -47_456_90, { kind: "loan" }),
      event("2026-11-05", "ООО «Ромашка»", 180_000_00),
    ],
    lowest: { on: "2026-10-15", balance_minor: -40_956_90 },
    next_income: event("2026-11-05", "ООО «Ромашка»", 180_000_00),
    free_until_income_minor: -40_956_90,
    missing_rates: [],
    ...over,
  };
}

// limited: the budget of this month has a limited category.
function show(f: Forecast, limited = false) {
  fetchMock.mockImplementation((input: Request) => {
    const json = (body: unknown) => Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } }));
    if (new URL(input.url).pathname.endsWith("/budget")) {
      return json({ month: "2026-10", base_currency: "RUB", lines: limited ? [{ category_id: "food" }] : [], planned_minor: 0, spent_minor: 0, left_minor: 0, unlimited_minor: 0, missing_rates: [] });
    }
    return json(f);
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ForecastCard />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("ForecastCard", () => {
  it("counts the budget when asked, once the family has a limit", async () => {
    localStorage.clear();
    show(forecast({ events: [event("2026-10-17", "", -3_500_00, { kind: "budget", account_id: null })] }), true);
    const box = await screen.findByTestId("forecast-with-budget");
    const asked = () => fetchMock.mock.calls.map(([r]) => new URL((r as Request).url)).filter((u) => u.pathname.endsWith("/forecast"));
    expect(asked().every((u) => u.searchParams.get("budget") === "false")).toBe(true);
    fireEvent.click(box);
    await waitFor(() => expect(asked().some((u) => u.searchParams.get("budget") === "true")).toBe(true));
    expect(localStorage.getItem("babki.forecast.budget")).toBe("1");
    await waitFor(() => expect(norm(screen.getByTestId("forecast-events").textContent ?? "")).toMatch(/17\.10\.2026 · траты по бюджету/));
  });

  it("offers no budget without a limit", async () => {
    show(forecast());
    await screen.findByTestId("forecast-events");
    expect(screen.queryByTestId("forecast-with-budget")).toBeNull();
  });

  it("says what is short before the salary, and lists what comes first", async () => {
    show(forecast());
    const free = await screen.findByTestId("forecast-free");
    expect(norm(free.textContent ?? "")).toMatch(/до 05\.11\.2026 \(ООО «Ромашка»\)не хватит 40 956,90 ₽/);
    const items = screen.getByTestId("forecast-events").querySelectorAll("li");
    expect(norm(items[0].textContent ?? "")).toMatch(/^ожидается сегодня · ИП Смирнова/);
    expect(norm(items[1].textContent ?? "")).toMatch(/Ипотека ВТБ · кредит/);
    expect(screen.getByTestId("forecast-chart")).toBeTruthy();
  });

  it("shows what can be spent when the money stays above zero", async () => {
    show(forecast({ free_until_income_minor: 12_000_00 }));
    expect(norm((await screen.findByTestId("forecast-free")).textContent ?? "")).toMatch(/12 000,00 ₽$/);
  });

  it("says when there is no regular income to count to", async () => {
    show(forecast({ next_income: null, free_until_income_minor: null }));
    expect(await screen.findByTestId("forecast-no-income")).toBeTruthy();
  });

  it("is not there without an account to count", async () => {
    show(forecast({ accounts_counted: 0 }));
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByTestId("money-forecast")).toBeNull();
  });
});
