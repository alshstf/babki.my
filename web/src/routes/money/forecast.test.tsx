import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
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

function show(f: Forecast) {
  fetchMock.mockImplementation(() =>
    Promise.resolve(new Response(JSON.stringify(f), { status: 200, headers: { "Content-Type": "application/json" } })),
  );
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
